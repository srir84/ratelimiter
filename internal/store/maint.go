package store

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/store/resp"
)

// The maintenance calls of the cleanup runner and the reclaimer (DESIGN §A4.5, §A4.7, §A5.4,
// §A5.8; IMPLEMENTATION_PLAN S7 step 4): package functions over an Adapter, every command on
// its maintenance connection, so acquires never queue behind them. The adapter observes
// rl_store_call_seconds for the three scripts (Call and CallBatch) and rl_store_connections
// for the maintenance connection.

const (
	// CleanupPipeline is the most cleanup_entry calls one pipeline (CallBatch) carries (§A5.8).
	CleanupPipeline = 100
	// scanMatch and scanCount are the reclaimer's SCAN pattern and COUNT hint (§A4.7).
	scanMatch = "rl:e:*"
	scanCount = "1000"
	// maxCursor is the most digits a SCAN cursor has (an unsigned 64-bit number).
	maxCursor = 20
)

// Cleanup is what the maintenance calls take from the instance: the epoch it joined and the
// floors of §A5.7, the policy values cleanup_entry needs (§A4.1), and CleanupPipelines.
type Cleanup struct {
	EpochID, FP          string
	FloorHwm, FloorGen   int64
	CapUnits, RateTokens int64
	PushUs               int64 // receipt_lifetime_us: the push of a quarantined key (§A4.5)
	Pipelines            int   // cleanup_entry pipelines in flight at most; below 1 is 1
}

// DueEntries runs due_entries for one page (§A4.1, §A4.5): at most limit ix members scored up
// to min(T0, cutoff), cutoff 0 being none, through Call, which loads the script and resends
// once after NOSCRIPT within the call.
func DueEntries(ctx context.Context, a Adapter, c Cleanup, limit int, cutoff int64) Response {
	return a.Call(ctx, Request{
		Kind:   Maintenance,
		Script: scripts.DueEntries,
		Keys:   []string{keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(c.EpochID)},
		Args: []string{
			c.EpochID, c.FP, strconv.Itoa(limit), strconv.FormatInt(cutoff, 10),
			strconv.FormatInt(c.FloorHwm, 10), strconv.FormatInt(c.FloorGen, 10),
		},
	})
}

// cleanupRequest is the cleanup_entry step for member (§A4.1): KEYS epoch, ctl, ix, q and
// member's k and mi, or the placeholders rl:e:<id>:k:! and rl:e:<id>:mi:! when member fails
// is_enc (keys.IsEnc), so the step reaches its integrity hold (§A4.5, N1); ARGV epoch_id, fp,
// member, cap_units, rate_tokens, push_us, floor_hwm, floor_gen.
func cleanupRequest(c Cleanup, member string) Request {
	k, mi := keys.BucketPlaceholder(c.EpochID), keys.MarkerIndexPlaceholder(c.EpochID)
	if keys.IsEnc(member) {
		k, mi = keys.BucketKey(c.EpochID, member), keys.MarkerIndexKey(c.EpochID, member)
	}
	return Request{
		Kind:   Maintenance,
		Script: scripts.CleanupEntry,
		Keys:   []string{keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(c.EpochID), keys.QuarantineKey(c.EpochID), k, mi},
		Args: []string{
			c.EpochID, c.FP, member, strconv.FormatInt(c.CapUnits, 10), strconv.FormatInt(c.RateTokens, 10),
			strconv.FormatInt(c.PushUs, 10), strconv.FormatInt(c.FloorHwm, 10), strconv.FormatInt(c.FloorGen, 10),
		},
	}
}

// CleanupEntries runs one cleanup_entry step per member and returns each member's response,
// in order (§A5.4, §A5.8). The steps go out as pipelines of at most CleanupPipeline calls, at
// most c.Pipelines of them in flight, so at most c.Pipelines × CleanupPipeline commands. A
// pipeline starts only while every response so far is a step's result (codes 30–33) or
// NOSCRIPT, since the first failed command stops the pass (API-1f): the members it never sends
// are unavailable, with nothing sent. Then the steps that got NOSCRIPT, which provably did
// nothing, and come before the first failed step in member order (plan choice: the pass stops
// at that step, so a NOSCRIPT step after it keeps its NOSCRIPT response, uncounted, and with
// none before it nothing more is sent): one SCRIPT LOAD, and each is resent exactly once, in
// the same way, except that a resend pipeline starts only while every resent step so far got
// a step's result: a second NOSCRIPT, like any other failure, stops the pass. The reload and
// the resends stay within each step's own deadline, min(ctx deadline, start + D) from its
// first send (§A5.4, API-22): the SCRIPT LOAD ends by the earliest deadline of the steps it
// serves and each resend pipeline by the earliest of its own steps', never later. A second
// NOSCRIPT, or a load that does not confirm the script (one its deadline ends included),
// leaves them outcome_unknown, with the load's error reply or reply for Diag; a step no resend
// pipeline carried keeps its NOSCRIPT response.
func CleanupEntries(ctx context.Context, a Adapter, c Cleanup, members []string) []Response {
	reqs := make([]Request, len(members))
	out := make([]Response, len(members))
	sentAt := make([]time.Time, len(members)) // when each step's first pipeline went to the adapter
	all := make([]int, len(members))
	for i, m := range members {
		reqs[i] = cleanupRequest(c, m)
		out[i] = Response{Outcome: Unavailable} // not sent until a pipeline carries it
		all[i] = i
	}
	pipelines(ctx, reqs, all, out, c.Pipelines, stepFailed, func(ctx context.Context, part []int, batch []Request) []Response {
		now := time.Now() // no later than the adapter's start, so now + D is no later than the step's deadline
		for _, i := range part {
			sentAt[i] = now
		}
		return a.CallBatch(ctx, batch)
	})
	var again []int
	for i, r := range out {
		if stepFailed(r) { // the pass stops here: the NOSCRIPT steps after it stay unsent
			break
		}
		if r.NoScript && r.Outcome != OK {
			again = append(again, i)
		}
	}
	if len(again) == 0 {
		return out
	}
	v, load := a.Do(withCallStart(ctx, earliest(sentAt, again)), Maintenance, "SCRIPT", "LOAD", scripts.CleanupEntry.Text)
	if load.Outcome != OK || v.Kind != resp.Bulk || v.Null || v.Str != scripts.CleanupEntry.SHA {
		for _, i := range again { // already outcome_unknown, from the NOSCRIPT reply
			if load.StoreError != "" {
				out[i].StoreError = load.StoreError
			} else if load.Outcome == OK {
				out[i].Raw = v.String() // the store loaded something else
			}
		}
		return out
	}
	first := make([]Response, len(out))
	copy(first, out)
	for _, i := range again {
		out[i] = Response{Outcome: Unavailable}
	}
	pipelines(ctx, reqs, again, out, c.Pipelines, resendFailed, func(ctx context.Context, part []int, batch []Request) []Response {
		return a.CallBatch(withCallStart(ctx, earliest(sentAt, part)), batch)
	})
	for _, i := range again {
		r := out[i]
		if !r.Sent && r.StoreError == "" {
			// Not resent: a resend pipeline before it failed (a second NOSCRIPT included), or
			// nothing left; as in Call, the step keeps its NOSCRIPT response.
			out[i] = first[i]
			continue
		}
		// The first EVALSHA counts, and the resend unless the store refused it before running it
		// (LOADING, BUSY), as in Call (API-1d); from here on anything unconfirmed is
		// outcome_unknown, never unavailable (API-22).
		r.Sent, r.SentCommands, r.NoScript = true, 1+r.SentCommands, true
		if r.Outcome == Unavailable || r.Outcome == Cancelled {
			r.Outcome = OutcomeUnknown
		}
		out[i] = r
	}
	return out
}

// pipelines sends the requests reqs[idx[…]] as pipelines of at most CleanupPipeline, in idx
// order, with at most inFlight pipelines in flight, and stores each response at its index in
// out. send sends one pipeline, the requests batch at the indexes part, as one CallBatch that
// ends by its deadline. A pipeline starts only while no response so far has failed (failed:
// stepFailed in the first round, resendFailed in the resend); the requests it leaves keep
// their response in out. Each goroutine ends when its last send returns, and pipelines waits
// for them.
func pipelines(ctx context.Context, reqs []Request, idx []int, out []Response, inFlight int, failed func(Response) bool,
	send func(ctx context.Context, part []int, batch []Request) []Response) {
	var parts [][]int
	for lo := 0; lo < len(idx); lo += CleanupPipeline {
		parts = append(parts, idx[lo:min(lo+CleanupPipeline, len(idx))])
	}
	work := make(chan []int, len(parts)) // every pipeline, queued before the workers start
	for _, part := range parts {
		work <- part
	}
	close(work)
	var stopped atomic.Bool
	var wg sync.WaitGroup
	for range min(max(inFlight, 1), cap(work)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for part := range work {
				if stopped.Load() {
					return
				}
				batch := make([]Request, len(part))
				for j, i := range part {
					batch[j] = reqs[i]
				}
				rs := send(ctx, part, batch)
				for j, i := range part {
					r := Response{Outcome: OutcomeUnknown} // a missing response: fail closed
					if j < len(rs) {
						r = rs[j]
					}
					out[i] = r
					if failed(r) {
						stopped.Store(true)
					}
				}
			}
		}()
	}
	wg.Wait()
}

// earliest returns the earliest of sentAt[i] for i in idx: the start whose deadline, start + D,
// is the earliest of those steps' deadlines. Each step of a resend was sent in the first round,
// so each has its time. For an empty idx, which CleanupEntries never passes (it sends no load
// without a step to resend, and no empty pipeline), no step's deadline bounds the call: it
// returns now, the start a call of its own takes (callStart).
func earliest(sentAt []time.Time, idx []int) time.Time {
	if len(idx) == 0 {
		return time.Now()
	}
	e := sentAt[idx[0]]
	for _, i := range idx[1:] {
		if sentAt[i].Before(e) {
			e = sentAt[i]
		}
	}
	return e
}

// stepFailed reports whether a first-round cleanup_entry response stops the pass (API-1f): a
// decoded reply other than a step's result (30–33: a refusal 20–22, or 25), or no decoded
// reply other than NOSCRIPT, which the resend settles. A member never sent (unavailable) counts
// as failed: only a failure leaves one unsent.
func stepFailed(r Response) bool {
	if r.Outcome == OK {
		return r.Reply.Code < 30 || r.Reply.Code > 33
	}
	return !r.NoScript
}

// resendFailed reports whether a resent cleanup_entry step's response stops the pass (§A5.4):
// anything but a step's result (30–33), a second NOSCRIPT included, which leaves the step
// outcome_unknown.
func resendFailed(r Response) bool {
	return r.Outcome != OK || r.Reply.Code < 30 || r.Reply.Code > 33
}

// Scan runs SCAN cursor MATCH rl:e:* COUNT 1000 on the maintenance connection (§A4.7) and
// returns the next cursor and the keys of the reply. A reply that is not [cursor, [key, …]],
// with a cursor that is an unsigned 64-bit decimal, is outcome_unknown, with the raw reply for
// Diag; then, as after any failure, the cursor returned is the one given.
func Scan(ctx context.Context, a Adapter, cursor string) (string, []string, Response) {
	v, r := a.Do(ctx, Maintenance, "SCAN", cursor, "MATCH", scanMatch, "COUNT", scanCount)
	if r.Outcome != OK {
		return cursor, nil, r
	}
	next, found, ok := scanReply(v)
	if !ok {
		r.Outcome, r.Raw = OutcomeUnknown, v.String()
		return cursor, nil, r
	}
	return next, found, r
}

// scanReply reads a SCAN reply: [cursor, [key, …]], the cursor an unsigned 64-bit decimal (1–20
// decimal digits, no sign, at most 2^64 − 1, so a cursor the store could not take back is never
// kept) and every key a bulk string.
func scanReply(v resp.Value) (string, []string, bool) {
	if v.Kind != resp.Array || v.Null || len(v.Elems) != 2 {
		return "", nil, false
	}
	c, list := v.Elems[0], v.Elems[1]
	if c.Kind != resp.Bulk || c.Null || len(c.Str) > maxCursor || list.Kind != resp.Array || list.Null {
		return "", nil, false
	}
	if _, err := strconv.ParseUint(c.Str, 10, 64); err != nil { // decimal digits only, within uint64
		return "", nil, false
	}
	found := make([]string, len(list.Elems))
	for i, k := range list.Elems {
		if k.Kind != resp.Bulk || k.Null {
			return "", nil, false
		}
		found[i] = k.Str
	}
	return c.Str, found, true
}

// Reclaim runs reclaim.lua for candidates (§A4.1, §A4.7): KEYS rl:epoch, then the candidates;
// through Call on the maintenance connection.
func Reclaim(ctx context.Context, a Adapter, candidates []string) Response {
	return a.Call(ctx, Request{
		Kind:   Maintenance,
		Script: scripts.Reclaim,
		Keys:   append([]string{keys.EpochKey}, candidates...),
	})
}
