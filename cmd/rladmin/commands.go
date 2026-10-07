package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
)

// Exit codes (DESIGN §A6, for every command).
const (
	exitOK = 0
	// exitRefused: a {41} refusal, with its reason printed; also an invalid policy and a usage
	// error (plan choices).
	exitRefused = 2
	// exitStore: a store or integrity problem: {20}, {22}, {25}, an error reply, a reply that
	// fails the §A4.9 layout or range checks, "epoch changed during read", or a failed
	// connection or exchange.
	exitStore = 3
	// exitClock: clock_error, {21}.
	exitClock = 4
)

// cmdError ends a command: its exit code, the message printed to stderr, the raw reply when
// one is shown (plan S8 step 11) and a recovery hint.
type cmdError struct {
	code int
	msg  string
	raw  *resp.Value
	hint string
}

func (e *cmdError) Error() string { return e.msg }

// storeError makes err, from a store exchange, a store problem (exit 3); a *replyError keeps
// its raw reply.
func storeError(err error) *cmdError {
	var re *replyError
	if errors.As(err, &re) {
		raw := re.raw
		return &cmdError{code: exitStore, msg: re.what, raw: &raw}
	}
	return &cmdError{code: exitStore, msg: err.Error()}
}

// recoverHint is printed with the refusals that only a new epoch fixes (DESIGN §A4.6).
const recoverHint = "the store has no readable epoch record: start one with " +
	"`rladmin start-epoch --policy FILE` (without --replace)"

// call runs script s and decodes its reply with store.Decode against lim (plan S8 step 11),
// so the §A4.9 layout and range checks apply: an exchange failure, an error reply or a reply
// that fails a check is a store problem (exit 3), the last printed with the raw reply.
func call(ctx context.Context, c *conn, s scripts.Script, ks, args []string, lim store.Limits) (store.Reply, error) {
	v, err := c.script(ctx, s, ks, args)
	if err != nil {
		return store.Reply{}, storeError(err)
	}
	if v.Kind == resp.Error {
		return store.Reply{}, &cmdError{code: exitStore, msg: s.Name + ": store error reply: " + v.Str}
	}
	r, err := store.Decode(s.Name, v, lim)
	if err != nil {
		return store.Reply{}, &cmdError{code: exitStore, msg: s.Name + ": reply fails the §A4.9 checks: " + err.Error(), raw: &v}
	}
	return r, nil
}

// verdict maps an admin script's decoded reply to its exit (DESIGN §A6): nil for 40, a
// refusal (2) for 41, clock_error (4) for 21, a store or integrity problem (3) for 20, 22, 25.
// The decoder admits only the codes each script's layout lists, so no other code arrives; one
// that did would be a store problem too (fail closed).
func verdict(script string, r store.Reply) error {
	switch r.Code {
	case 40:
		return nil
	case 41:
		e := &cmdError{code: exitRefused, msg: script + " refused: " + r.Reason}
		if r.Reason == "epoch_record_missing" || r.Reason == "epoch_record_malformed" {
			e.hint = recoverHint
		}
		return e
	case 21:
		return &cmdError{code: exitClock, msg: script + ": clock_error: " + r.Reason}
	case 20, 22, 25:
		return &cmdError{code: exitStore, msg: fmt.Sprintf("%s: code %d: %s", script, r.Code, r.Reason)}
	}
	return &cmdError{code: exitStore, msg: fmt.Sprintf("%s: reply code %d, which no §A4.9 layout of it lists", script, r.Code)}
}

// newEpochID returns a new epoch ID: 16 bytes from crypto/rand in lowercase hex (§A5.9), so
// an ID never repeats, even after a wipe (ADM-1).
func newEpochID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("drawing an epoch ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// startEpoch is ADM-1 (DESIGN §A4.6, §A6): admin_start_epoch with a new ID, the policy's
// fingerprint, schema 1 and clock source, and the replace flag with the expected ID. It
// returns the new ID and the epoch's start time (origin = hwm = created).
func startEpoch(ctx context.Context, c *conn, pol policyInfo, replace bool, expect string) ([]field, error) {
	id, err := newEpochID()
	if err != nil {
		return nil, &cmdError{code: exitStore, msg: err.Error()}
	}
	replaceArg := "0"
	if replace {
		replaceArg = "1"
	}
	r, err := call(ctx, c, scripts.AdminStartEpoch, []string{keys.EpochKey, keys.CtlTimeKey},
		[]string{id, pol.fp, "1", pol.p.ClockSource.String(), replaceArg, expect}, pol.lim)
	if err != nil {
		return nil, err
	}
	if err := verdict(scripts.AdminStartEpoch.Name, r); err != nil {
		return nil, err
	}
	if got := r.Elems[3].Str; got != id {
		raw := resp.Value{Kind: resp.Array, Elems: r.Elems}
		return nil, &cmdError{code: exitStore, msg: "admin_start_epoch: the reply names another epoch ID than the one sent", raw: &raw}
	}
	return []field{{"epoch_id", id}, {"t_us", r.TUs}}, nil
}

// expectedID is the epoch ID of read_epoch's reply (field 4, DESIGN §A4.6): the record's id
// whenever it validates, also under a hold, and "" otherwise; admin_status and admin_inspect
// then refuse a missing or malformed record themselves. read_epoch takes the placeholder q:
// rladmin joins no epoch (§A4.1).
func expectedID(ctx context.Context, c *conn) (string, error) {
	r, err := call(ctx, c, scripts.ReadEpoch, []string{keys.EpochKey, keys.CtlTimeKey, keys.QuarantinePlaceholder}, nil, store.Limits{})
	if err != nil {
		return "", err
	}
	return r.Elems[3].Str, nil
}

// namespaceID is the epoch ID to build key names from: id, or "!" when there is none (the
// record check refuses before the scripts use the names, as with the q placeholder).
func namespaceID(id string) string {
	if id == "" {
		return "!"
	}
	return id
}

// errEpochChanged ends a status or inspect whose epoch changed twice (DESIGN §A4.6).
func errEpochChanged() error {
	return &cmdError{code: exitStore, msg: "epoch changed during read"}
}

// status is ADM-4 status (DESIGN §A4.6, §A6): admin_status with the expected ID from
// read_epoch; on epoch_changed it reads the ID again and retries once.
func status(ctx context.Context, c *conn) ([]field, error) {
	for range 2 {
		id, err := expectedID(ctx, c)
		if err != nil {
			return nil, err
		}
		r, err := call(ctx, c, scripts.AdminStatus, []string{keys.EpochKey, keys.QuarantineKey(namespaceID(id))}, []string{id}, store.Limits{})
		if err != nil {
			return nil, err
		}
		if r.Code == 41 && r.Reason == "epoch_changed" {
			continue
		}
		if err := verdict(scripts.AdminStatus.Name, r); err != nil {
			return nil, err
		}
		e := r.Elems // {40, 0, '', id, fp, hold, hwm_or_'', nb, nm, quarantined_count}
		return []field{
			{"epoch_id", e[3].Str},
			{"fingerprint", e[4].Str},
			{"hold", e[5].Str},
			{"hwm_us", opt(e[6])},
			{"tracked_buckets", e[7].Int},
			{"live_markers", e[8].Int},
			{"quarantined_keys", e[9].Int},
		}, nil
	}
	return nil, errEpochChanged()
}

// inspect is ADM-4 per-key inspect (DESIGN §A4.6, §A6): admin_inspect of the key whose
// encoding is enc, with the expected ID from read_epoch; on epoch_changed it reads the ID
// again and retries once.
func inspect(ctx context.Context, c *conn, enc string) ([]field, error) {
	for range 2 {
		id, err := expectedID(ctx, c)
		if err != nil {
			return nil, err
		}
		ns := namespaceID(id)
		r, err := call(ctx, c, scripts.AdminInspect,
			[]string{keys.EpochKey, keys.IndexKey(ns), keys.QuarantineKey(ns), keys.BucketKey(ns, enc), keys.MarkerIndexKey(ns, enc)},
			[]string{id, enc}, store.Limits{})
		if err != nil {
			return nil, err
		}
		if r.Code == 41 && r.Reason == "epoch_changed" {
			continue
		}
		if err := verdict(scripts.AdminInspect.Name, r); err != nil {
			return nil, err
		}
		// {40, 0, '', present, u, t, g, s, x, mc, ix_score, quarantine, n, expiry_1 … expiry_n}
		e := r.Elems
		expiries := make([]*int64, 0, len(e)-13)
		for _, v := range e[13:] { // the decoder checked the length: 13 + n
			expiries = append(expiries, opt(v))
		}
		return []field{
			{"present", e[3].Int},
			{"u", opt(e[4])},
			{"t_us", opt(e[5])},
			{"g", opt(e[6])},
			{"s", opt(e[7])},
			{"x_us", opt(e[8])},
			{"mc", opt(e[9])},
			{"ix_score", opt(e[10])},
			{"quarantine", e[11].Str},
			{"marker_expiries", expiries},
		}, nil
	}
	return nil, errEpochChanged()
}

// balanceArg is restore's balance in units, as sent (DESIGN §A6): --balance-units U as it is,
// --balance-tokens T as T × rate_period_ns, computed exactly, or 0 when neither is given.
// admin_restore checks 0 ≤ balance ≤ cap_units in the store (§A4.6).
func balanceArg(units int64, unitsSet bool, tokens int64, tokensSet bool, ratePeriodNs int64) string {
	switch {
	case tokensSet:
		return new(big.Int).Mul(big.NewInt(tokens), big.NewInt(ratePeriodNs)).String()
	case unitsSet:
		return strconv.FormatInt(units, 10)
	}
	return "0"
}

// restoreKey is ADM-2 (DESIGN §A4.6, §A6): admin_restore of the key whose encoding is enc in the
// expected epoch, with the policy's fingerprint, cap_units and lifetime_us. It returns the new
// generation and the restore's budget time.
func restoreKey(ctx context.Context, c *conn, pol policyInfo, enc, expect, balance string) ([]field, error) {
	r, err := call(ctx, c, scripts.AdminRestore, perKeyKeys(expect, enc),
		[]string{expect, pol.fp, enc, balance, i64(pol.lim.CapUnits), i64(pol.lim.LifetimeUs)}, pol.lim)
	if err != nil {
		return nil, err
	}
	if err := verdict(scripts.AdminRestore.Name, r); err != nil {
		return nil, err
	}
	return []field{{"g", r.Elems[3].Int}, {"t_us", r.TUs}}, nil
}

// clearKey is ADM-3 (DESIGN §A4.6, §A6): admin_clear of the key whose encoding is enc in the
// expected epoch, with the policy's fingerprint and cap_units. It returns the clear's budget
// time.
func clearKey(ctx context.Context, c *conn, pol policyInfo, enc, expect string) ([]field, error) {
	r, err := call(ctx, c, scripts.AdminClear, perKeyKeys(expect, enc),
		[]string{expect, pol.fp, enc, i64(pol.lim.CapUnits)}, pol.lim)
	if err != nil {
		return nil, err
	}
	if err := verdict(scripts.AdminClear.Name, r); err != nil {
		return nil, err
	}
	return []field{{"t_us", r.TUs}}, nil
}

// perKeyKeys are the KEYS of admin_restore and admin_clear (§A4.1), named from the expected
// epoch: the scripts check the ID before they assert the per-key names.
func perKeyKeys(id, enc string) []string {
	return []string{keys.EpochKey, keys.CtlTimeKey, keys.IndexKey(id), keys.QuarantineKey(id), keys.BucketKey(id, enc), keys.MarkerIndexKey(id, enc)}
}

func i64(n int64) string { return strconv.FormatInt(n, 10) }

// infoSection is one section of an INFO reply: its name in lower case and its fields.
type infoSection struct {
	name   string
	fields map[string]string
	lines  []string // the "name:value" lines, in order, for the table
}

// slowEntry is one SLOWLOG GET entry (DESIGN §A6).
type slowEntry struct {
	ID         int64    `json:"id"`
	Timestamp  int64    `json:"timestamp"`
	DurationUs int64    `json:"duration_us"`
	Args       []string `json:"args"`
	Client     string   `json:"client"`
	ClientName string   `json:"client_name"`
}

// storeInfo is the store's INFO (persistence, clients, memory, stats) and its slow log.
type storeInfo struct {
	sections []infoSection
	slowlog  []slowEntry
}

// fetchStoreInfo runs INFO persistence clients memory stats and SLOWLOG GET 20 (DESIGN §A6).
// A reply of another shape is a store problem (exit 3), printed with the raw reply.
func fetchStoreInfo(ctx context.Context, c *conn) (storeInfo, error) {
	v, err := c.do(ctx, "INFO", "INFO", "persistence", "clients", "memory", "stats")
	if err != nil {
		return storeInfo{}, storeError(err)
	}
	secs, err := parseInfo(v)
	if err != nil {
		return storeInfo{}, err
	}
	sv, err := c.do(ctx, "SLOWLOG GET", "SLOWLOG", "GET", "20")
	if err != nil {
		return storeInfo{}, storeError(err)
	}
	slow, err := parseSlowlog(sv)
	if err != nil {
		return storeInfo{}, err
	}
	return storeInfo{sections: secs, slowlog: slow}, nil
}

// parseInfo splits an INFO reply into its sections ("# Name" headers, "name:value" lines).
func parseInfo(v resp.Value) ([]infoSection, error) {
	if v.Kind == resp.Error {
		return nil, &cmdError{code: exitStore, msg: "INFO: store error reply: " + v.Str}
	}
	if v.Kind != resp.Bulk || v.Null {
		return nil, &cmdError{code: exitStore, msg: "INFO: the reply is not a bulk string", raw: &v}
	}
	var out []infoSection
	for _, line := range strings.Split(v.Str, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
		case strings.HasPrefix(line, "#"):
			name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "#")))
			out = append(out, infoSection{name: name, fields: map[string]string{}})
		default:
			k, val, ok := strings.Cut(line, ":")
			if !ok || len(out) == 0 {
				return nil, &cmdError{code: exitStore, msg: "INFO: a line outside the name:value form", raw: &v}
			}
			s := &out[len(out)-1]
			s.fields[k] = val
			s.lines = append(s.lines, line)
		}
	}
	return out, nil
}

// parseSlowlog decodes a SLOWLOG GET reply: entries of id, timestamp, duration in µs and the
// command's arguments, then (Valkey) the client's address and name.
func parseSlowlog(v resp.Value) ([]slowEntry, error) {
	if v.Kind == resp.Error {
		return nil, &cmdError{code: exitStore, msg: "SLOWLOG GET: store error reply: " + v.Str}
	}
	bad := &cmdError{code: exitStore, msg: "SLOWLOG GET: a reply of an unexpected shape", raw: &v}
	if v.Kind != resp.Array || v.Null {
		return nil, bad
	}
	out := make([]slowEntry, 0, len(v.Elems))
	for _, e := range v.Elems {
		if e.Kind != resp.Array || len(e.Elems) < 4 {
			return nil, bad
		}
		f := e.Elems
		if f[0].Kind != resp.Int || f[1].Kind != resp.Int || f[2].Kind != resp.Int || f[3].Kind != resp.Array {
			return nil, bad
		}
		se := slowEntry{ID: f[0].Int, Timestamp: f[1].Int, DurationUs: f[2].Int, Args: []string{}}
		for _, a := range f[3].Elems {
			if a.Kind != resp.Bulk || a.Null {
				return nil, bad
			}
			se.Args = append(se.Args, a.Str)
		}
		for i, dst := range []*string{&se.Client, &se.ClientName} {
			if 4+i < len(f) {
				if f[4+i].Kind != resp.Bulk || f[4+i].Null {
					return nil, bad
				}
				*dst = f[4+i].Str
			}
		}
		out = append(out, se)
	}
	return out, nil
}

// rewritePoll is how often store-rewrite reads INFO persistence (plan S8 step 3).
const rewritePoll = 100 * time.Millisecond

// storeRewrite runs BGREWRITEAOF, then reads INFO persistence every poll until no rewrite is
// in progress or scheduled (aof_rewrite_in_progress:0, aof_rewrite_scheduled:0), within ctx's
// deadline (DESIGN §A6). A rewrite that ends with aof_last_bgrewrite_status other than ok is a
// store problem (exit 3). When a rewrite already runs (store.RewriteRunningReply: an automatic
// one, or one an interrupted run left behind), it waits the same way for that rewrite to end,
// its first read a poll later, then sends BGREWRITEAOF again, as often as that reply comes
// back; only its own rewrite's status counts. Any other error reply exits 3.
func storeRewrite(ctx context.Context, c *conn, poll time.Duration) ([]field, error) {
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		v, err := c.do(ctx, "BGREWRITEAOF", "BGREWRITEAOF")
		if err != nil {
			return nil, storeError(err)
		}
		running := v.Kind == resp.Error && store.RewriteRunningReply(v.Str)
		switch {
		case running:
		case v.Kind == resp.Error:
			return nil, &cmdError{code: exitStore, msg: "BGREWRITEAOF: store error reply: " + v.Str}
		case v.Kind != resp.Simple:
			return nil, &cmdError{code: exitStore, msg: "BGREWRITEAOF: a reply of an unexpected shape", raw: &v}
		}
		p, err := waitRewrite(ctx, c, tick.C, running)
		if err != nil {
			return nil, err
		}
		if running {
			continue
		}
		st := p["aof_last_bgrewrite_status"]
		if st != "ok" {
			return nil, &cmdError{code: exitStore, msg: "the AOF rewrite failed: aof_last_bgrewrite_status:" + st}
		}
		return []field{{"aof_rewrite_in_progress", p["aof_rewrite_in_progress"]}, {"aof_last_bgrewrite_status", st}}, nil
	}
}

// waitRewrite reads INFO persistence at once (later: at the next tick) and then at every tick
// until no AOF rewrite is in progress or scheduled, and returns the persistence fields it read
// last. ctx ending first (its deadline or a signal) is a store problem (exit 3).
func waitRewrite(ctx context.Context, c *conn, tick <-chan time.Time, later bool) (map[string]string, error) {
	for wait := later; ; wait = true {
		if wait {
			select {
			case <-ctx.Done():
				return nil, &cmdError{code: exitStore, msg: "waiting for the AOF rewrite: " + ctx.Err().Error()}
			case <-tick:
			}
		}
		iv, err := c.do(ctx, "INFO persistence", "INFO", "persistence")
		if err != nil {
			return nil, storeError(err)
		}
		secs, err := parseInfo(iv)
		if err != nil {
			return nil, err
		}
		p := map[string]string{}
		for _, s := range secs {
			if s.name == "persistence" {
				p = s.fields
			}
		}
		inProgress, ok1 := p["aof_rewrite_in_progress"]
		scheduled, ok2 := p["aof_rewrite_scheduled"]
		if !ok1 || !ok2 {
			return nil, &cmdError{code: exitStore, msg: "INFO persistence: no aof_rewrite_in_progress or aof_rewrite_scheduled field", raw: &iv}
		}
		if inProgress == "0" && scheduled == "0" {
			return p, nil
		}
	}
}
