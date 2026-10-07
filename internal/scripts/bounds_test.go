//go:build scripttest

package scripts_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/storeproc"
)

// stepCommands is C of plan S7's TestScriptCommandBounds: the fixed reads and writes of one
// cleanup_entry step, counted from DESIGN §A4.2 and §A4.5. Reads: HGETALL rl:epoch and GET
// rl:ctl:time (the prelude), ZSCORE ix, HEXISTS q, and check_key's HGETALL k, ZRANGE mi and
// ZSCORE ix: 7. Writes on the removal path: DEL k mi, ZREM ix, HINCRBY nb, HINCRBY nm and
// write_hwm's HSET: 5. The other path writes HINCRBY mc, HINCRBY nm, ZADD ix and HSET hwm (4,
// the HINCRBYs only with due markers) besides the chunked HDEL and ZREM.
const stepCommands = 12

// scriptCommands are the commands INFO commandstats counts that a script call adds, as the
// rest of the counts: every entry but the call itself (evalsha, and script|load for a script
// the store does not know yet), the reads of INFO and an adapter's AUTH.
func scriptCommands(t *testing.T, ops *storeproc.Client) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for k, v := range storeproc.ParseInfo(mustDo(t, ops, "INFO", "commandstats").Str) {
		cmd, ok := strings.CutPrefix(k, "cmdstat_")
		if !ok || cmd == "evalsha" || cmd == "info" || cmd == "auth" || strings.HasPrefix(cmd, "script") {
			continue
		}
		calls, found := "", false
		for _, kv := range strings.Split(v, ",") {
			if calls, found = strings.CutPrefix(kv, "calls="); found {
				break
			}
		}
		n, err := strconv.ParseInt(calls, 10, 64)
		if !found || err != nil || n < 0 {
			t.Fatalf("commandstats entry %s:%s", k, v)
		}
		out[cmd] = n
	}
	return out
}

// counted runs req as rl_lib and returns the commands INFO commandstats, read as rl_ops before
// and after, counts for it, per command and in all (scriptCommands).
func (e *env) counted(ops *storeproc.Client, req store.Request) (map[string]int64, int64, []any) {
	e.t.Helper()
	before := scriptCommands(e.t, ops)
	got := e.run(e.la, req)
	after := scriptCommands(e.t, ops)
	delta, total := map[string]int64{}, int64(0)
	for cmd, n := range after {
		if d := n - before[cmd]; d != 0 {
			delta[cmd] = d
			total += d
		}
	}
	return delta, total, got
}

// TestScriptCommandBounds (plan S7, L2; DESIGN §C2, §C3, §A4.8 rule 3): the commands each
// script call runs, from INFO commandstats deltas read as rl_ops (S1's
// TestC2_CommandstatsCountsScriptCommands pins that the store counts a script's commands; the
// INFO reads themselves are left out). One cleanup_entry step with n due markers, n = 0, 1, 500,
// 501 and 1,001 (max_markers_per_key raised to 10,000), runs at most C + 2⌈n/500⌉ commands, C =
// stepCommands: the removal path exactly C, whatever n (its DEL removes every marker), and the
// other path 11 + 2⌈n/500⌉ (9 for n = 0) with exactly ⌈n/500⌉ HDEL and ⌈n/500⌉ ZREM. An acquire
// (allowed) and a refund (applied) on existing buckets holding 0, 10 and 999 markers run the
// same number of commands: 9 and 11, counted from §A4.3 and §A4.4. Each call runs at a later
// time than the one before, so each stores the high-water mark.
func TestScriptCommandBounds(t *testing.T) {
	p := tenK()
	e := newEnv(t, p)
	ops := dial(t, e.h, "rl_ops")
	enc := func(name string) string {
		s, err := keys.Encode(name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	type stepCase struct {
		enc      string
		eligible bool
		n        int
	}
	var steps []stepCase
	nb, nm := 0, 0
	for _, eligible := range []bool{true, false} {
		for _, n := range []int{0, 1, 500, 501, 1001} {
			c := stepCase{enc: enc(fmt.Sprintf("step-%v-%d", eligible, n)), eligible: eligible, n: n}
			steps = append(steps, c)
			nb, nm = nb+1, nm+n
		}
	}
	type opCase struct {
		enc string
		n   int
	}
	var acquires, refunds []opCase
	for _, n := range []int{0, 10, 999} {
		acquires = append(acquires, opCase{enc(fmt.Sprintf("acquire-%d", n)), n})
		refunds = append(refunds, opCase{enc(fmt.Sprintf("refund-%d", n)), n})
		nb, nm = nb+2, nm+2*n
	}
	e.record(healthy(e.fp).with("nb", strconv.Itoa(nb), "nm", strconv.Itoa(nm)))
	e.clock(now0)
	for _, c := range steps {
		x, last := int64(pastX), int64(pastX) // removable: every marker expired, x passed
		if !c.eligible {
			x, last = now0+100_000_000, now0-1_000_000 // x unexpired, every marker due
		}
		ms := marks(c.n, last)
		score := int64(pastX)
		if c.n > 0 {
			score = ms[0].expiry
		}
		e.writeKey(c.enc, 0, oldT, int64(c.n)+1, x, num(score), ms...)
	}
	for _, c := range slices.Concat(acquires, refunds) {
		// Full at now0, x 50 s on, the markers unexpired: receipts 1 … n were refunded, and the
		// bucket's sequence is n + 1.
		x := int64(now0 + 50_000_000)
		ms := marks(c.n, now0+40_000_000)
		score := x
		if c.n > 0 {
			score = ms[0].expiry
		}
		e.writeKey(c.enc, 0, oldT, int64(c.n)+1, x, num(score), ms...)
	}

	at := int64(now0)
	for _, c := range steps {
		at++
		e.clock(at)
		chunks := int64((c.n + 499) / 500)
		delta, total, got := e.counted(ops, cleanupReq(p, e.fp, c.enc))
		name := fmt.Sprintf("cleanup_entry, eligible %v, %d due markers", c.eligible, c.n)
		wantTotal, wantHdel, wantZrem, wantDel := int64(stepCommands), int64(0), int64(1), int64(1)
		if c.eligible {
			checkReply(t, name, got, rep(30, at, "", c.n))
		} else {
			checkReply(t, name, got, rep(31, at, "", c.n))
			wantTotal, wantHdel, wantZrem, wantDel = 9, chunks, chunks, 0
			if c.n > 0 {
				wantTotal = 11 + 2*chunks
			}
		}
		if bound := stepCommands + 2*chunks; total > bound {
			t.Errorf("%s: %d commands, want at most C + 2⌈n/500⌉ = %d (%v)", name, total, bound, delta)
		}
		if total != wantTotal || delta["hdel"] != wantHdel || delta["zrem"] != wantZrem || delta["del"] != wantDel {
			t.Errorf("%s: %d commands, %d HDEL, %d ZREM, %d DEL; want %d, %d, %d, %d (%v)",
				name, total, delta["hdel"], delta["zrem"], delta["del"], wantTotal, wantHdel, wantZrem, wantDel, delta)
		}
	}
	for _, c := range acquires {
		at++
		e.clock(at)
		delta, total, got := e.counted(ops, acquireReq(p, e.fp, c.enc))
		name := fmt.Sprintf("acquire, %d markers", c.n)
		checkReply(t, name, got, rep(1, at, "", capUnits-ratePeriod, gen0, c.n+2, at+lifetimeUs))
		if total != 9 {
			t.Errorf("%s: %d commands, want 9 (%v)", name, total, delta)
		}
	}
	for _, c := range refunds {
		at++
		e.clock(at)
		s, exp := int64(c.n)+1, int64(now0+45_000_000)
		delta, total, got := e.counted(ops, refundReq(p, e.fp, c.enc, gen0, s, exp))
		name := fmt.Sprintf("refund, %d markers", c.n)
		checkReply(t, name, got, rep(3, at, "", 0)) // a full bucket: nothing to credit
		if total != 11 {
			t.Errorf("%s: %d commands, want 11 (%v)", name, total, delta)
		}
	}
}
