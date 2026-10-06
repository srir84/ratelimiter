package core

import (
	"context"
	"sync"
	"time"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/receipt"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
)

// watchState is the epoch watch's state (DESIGN §A5.7, SPEC CFG-6).
type watchState int

const (
	wsNoEpoch  watchState = iota // nothing read yet, or the record missing and never joined: not_ready (no_epoch)
	wsJoined                     // joined a matching epoch: ready
	wsMismatch                   // the record's fingerprint differs: config_error (fingerprint_mismatch)
	wsLost                       // record missing after joining, malformed, hwm missing or malformed: state_lost
	wsHeld                       // the integrity hold is set, or this instance saw a rollback: integrity_hold
)

// read_epoch reasons the watch tells apart (§A4.2, §A4.9), and the hold reason it sets.
const (
	reasonRecordMissing = "epoch_record_missing"
	reasonIntegrityHold = "integrity_hold"
	reasonStoreRollback = "store_rollback"
)

// epochState is one immutable snapshot of the watch, which Acquire and Health read.
type epochState struct {
	state    watchState
	reason   string   // a lost state's Diag reason: the read_epoch reason
	id       string   // the epoch joined last; "" before the first join
	idBytes  [16]byte // id decoded, for receipts
	floorHwm int64    // the highest hwm read from the record in epoch id (§A5.7 floors)
	floorGen int64    // the highest gen read from the record in epoch id
}

// with returns a copy of e in state s with Diag reason reason; the joined epoch and its
// floors stay.
func (e *epochState) with(s watchState, reason string) *epochState {
	n := *e
	n.state, n.reason = s, reason
	return &n
}

// epochRecord is a decoded read_epoch reply (§A4.9): {state, 0, reason, id, fp, hwm, gen,
// clock_us, nb, nm, quarantined_or_”}.
type epochRecord struct {
	state                     int64
	reason, id, fp            string
	hwm, gen, clockUs, nb, nm int64
	quarantined               int64 // 0 for ''
}

// epochWatch is the epoch watch of DESIGN §A5.7: one goroutine (run) that reads read_epoch on
// a call connection at startup, every EpochRereadIntervalNs, and early after an acquire's
// epoch-wide config_error or state_integrity_error, at most once per interval. Until its
// first successful read the state is no_epoch: New never waits for the store (plan choice).
type epochWatch struct {
	l        *Limiter
	interval time.Duration
	early    chan struct{} // capacity 1: a trigger while one is pending is dropped

	mu   sync.Mutex      // serializes read
	left map[string]bool // epoch ids this instance joined and then left (ADM-1 ids never repeat)
}

func newEpochWatch(l *Limiter, interval time.Duration) *epochWatch {
	return &epochWatch{l: l, interval: interval, early: make(chan struct{}, 1), left: map[string]bool{}}
}

// trigger asks for an early read without waiting; run takes at most one per interval.
func (w *epochWatch) trigger() {
	if w == nil {
		return
	}
	select {
	case w.early <- struct{}{}:
	default:
	}
}

// run is the watch goroutine: it reads at once, then on every tick and on early triggers,
// until ctx (the instance lifetime) ends.
func (w *epochWatch) run(ctx context.Context) {
	w.read(ctx)
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	var lastEarly time.Time // the zero time: Sub saturates, so the first trigger reads
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.read(ctx)
		case <-w.early:
			if now := time.Now(); now.Sub(lastEarly) >= w.interval {
				lastEarly = now
				w.read(ctx)
			}
		}
	}
}

// read runs read_epoch once and applies its row of §A5.7. KEYS[3] is the joined epoch's q,
// or rl:e:!:q before the first join. A store failure or a reply that does not decode leaves
// the state unchanged and sets the health store error; a decoded reply clears it.
func (w *epochWatch) read(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cur := w.l.epoch.Load()
	q := keys.QuarantinePlaceholder
	if cur.id != "" {
		q = keys.QuarantineKey(cur.id)
	}
	r := w.l.adapter.Call(ctx, store.Request{
		Kind: store.Call, Script: scripts.ReadEpoch, Keys: []string{keys.EpochKey, keys.CtlTimeKey, q},
	})
	rec, ok := decodeEpoch(r)
	if !ok {
		if ctx.Err() == nil {
			w.l.setStoreError(failureText(r, "read_epoch reply out of shape"))
		}
		return
	}
	w.l.setStoreError("")
	next, holdID := w.next(cur, rec)
	w.l.epoch.Store(next)
	w.gauges(rec, next)
	if holdID != "" {
		w.hold(ctx, holdID)
	}
}

// next maps a decoded read to the new state (§A5.7), integrity rows before the fingerprint
// row (CFG-6). holdID names the epoch to put on hold when the read shows a rollback.
func (w *epochWatch) next(cur *epochState, rec epochRecord) (next *epochState, holdID string) {
	if rec.state == 22 {
		switch rec.reason {
		case reasonRecordMissing:
			if cur.id == "" {
				return cur.with(wsNoEpoch, ""), ""
			}
		case reasonIntegrityHold:
			return cur.with(wsHeld, reasonIntegrityHold), ""
		}
		// missing after joining, epoch_record_malformed, hwm_missing, hwm_malformed (and any
		// other reason: fail closed)
		return cur.with(wsLost, rec.reason), ""
	}
	// state 0: the record is well-formed and not held.
	if w.left[rec.id] { // an epoch this instance already left: the store went back in time
		return cur.with(wsHeld, reasonIntegrityHold), rec.id
	}
	if rec.id == cur.id && (rec.hwm < cur.floorHwm || rec.gen < cur.floorGen) {
		return cur.with(wsHeld, reasonIntegrityHold), rec.id
	}
	if rec.fp != w.l.fp {
		return cur.with(wsMismatch, ""), ""
	}
	if rec.id != cur.id { // join, or switch to a replacement epoch with fresh floors
		id, err := receipt.ParseEpochID(rec.id)
		if err != nil { // decodeEpoch accepted only a valid ID; fail closed all the same
			return cur.with(wsLost, "epoch_record_malformed"), ""
		}
		if cur.id != "" {
			w.left[cur.id] = true
		}
		return &epochState{state: wsJoined, id: rec.id, idBytes: id, floorHwm: rec.hwm, floorGen: rec.gen}, ""
	}
	n := cur.with(wsJoined, "")
	n.floorHwm, n.floorGen = max(cur.floorHwm, rec.hwm), max(cur.floorGen, rec.gen)
	return n, ""
}

// hold runs hold.lua(id, 'store_rollback') (§A4.1, §A5.7). Whatever it answers, the state is
// already held; the next read shows the hold, or sees the rollback again and retries.
func (w *epochWatch) hold(ctx context.Context, id string) {
	r := w.l.adapter.Call(ctx, store.Request{
		Kind: store.Call, Script: scripts.Hold, Keys: []string{keys.EpochKey}, Args: []string{id, reasonStoreRollback},
	})
	if r.Outcome != store.OK && ctx.Err() == nil {
		w.l.setStoreError(failureText(r, "hold reply out of shape"))
	}
}

// gauges sets the gauges every read refreshes (§A5.7, §A5.11).
func (w *epochWatch) gauges(rec epochRecord, next *epochState) {
	reg := w.l.reg
	lag := int64(0) // hwm − clock_us; 0 when negative or either is unreadable (0)
	if rec.hwm > 0 && rec.clockUs > 0 && rec.hwm > rec.clockUs {
		lag = rec.hwm - rec.clockUs
	}
	reg.Set(metrics.ClockLagMicroseconds, 0, float64(lag))
	reg.Set(metrics.TrackedBuckets, 0, float64(rec.nb))
	reg.Set(metrics.LiveMarkers, 0, float64(rec.nm))
	reg.Set(metrics.QuarantinedKeys, 0, float64(rec.quarantined))
	hold := 0.0
	if next.state == wsHeld {
		hold = 1
	}
	reg.Set(metrics.IntegrityHold, 0, hold)
}

// decodeEpoch reads a read_epoch response. The adapter has already checked the layout and
// ranges (§A4.9); this repeats the shape checks a wrapped adapter could skip, so a bad reply
// is a failed read instead of a panic. A state-0 reply must carry a valid epoch ID.
func decodeEpoch(r store.Response) (epochRecord, bool) {
	e := r.Reply.Elems
	if r.Outcome != store.OK || (r.Reply.Code != 0 && r.Reply.Code != 22) || len(e) != 11 {
		return epochRecord{}, false
	}
	rec := epochRecord{state: r.Reply.Code, reason: r.Reply.Reason}
	var ok [9]bool
	rec.id, ok[0] = elemStr(e, 3)
	rec.fp, ok[1] = elemStr(e, 4)
	rec.hwm, ok[2] = elemUint(e, 5)
	rec.gen, ok[3] = elemUint(e, 6)
	rec.clockUs, ok[4] = elemUint(e, 7)
	rec.nb, ok[5] = elemUint(e, 8)
	rec.nm, ok[6] = elemUint(e, 9)
	rec.quarantined, ok[7] = elemUint(e, 10)
	if s, isStr := elemStr(e, 10); isStr && s == "" {
		ok[7] = true
	}
	_, idErr := receipt.ParseEpochID(rec.id)
	ok[8] = rec.state != 0 || idErr == nil
	for _, v := range ok {
		if !v {
			return epochRecord{}, false
		}
	}
	return rec, true
}

// elemInt returns the integer at position i of a reply.
func elemInt(e []resp.Value, i int) (int64, bool) {
	if i >= len(e) || e[i].Kind != resp.Int {
		return 0, false
	}
	return e[i].Int, true
}

// elemUint returns the integer at position i of a reply when it lies in 0 … 2^53 − 1.
func elemUint(e []resp.Value, i int) (int64, bool) {
	v, ok := elemInt(e, i)
	return v, ok && v >= 0 && v < 1<<53
}

// elemStr returns the bulk string at position i of a reply.
func elemStr(e []resp.Value, i int) (string, bool) {
	if i >= len(e) || e[i].Kind != resp.Bulk || e[i].Null {
		return "", false
	}
	return e[i].Str, true
}

// failureText is the health store error of a failed call: the error reply's text, else the
// outcome's name, or what, for a reply that did not decode as expected.
func failureText(r store.Response, what string) string {
	switch {
	case r.StoreError != "":
		return r.StoreError
	case r.Outcome == store.OK:
		return what
	}
	return r.Outcome.String()
}
