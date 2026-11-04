package core

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/internal/fingerprint"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/internal/store/resp"
)

// The cleanup tests' values (IMPLEMENTATION_PLAN S7 Tests (a)): the epoch the fake store
// reports, another one, and budget times in µs.
const (
	clEpoch = "00112233445566778899aabbccddeeff"
	clOther = "ffeeddccbbaa99887766554433221100"
	clT0    = 1_760_000_000_123_456 // the first page's T0
	clHour  = 3_600_000_000_000     // a cleanup interval and re-read interval that never fire in a test
	// clDueCalls is the most due_entries calls a cleanFake answers; later ones are
	// unavailable, so a pass that never stops on an empty page still ends, with a status the
	// test checks, instead of running until the test binary times out.
	clDueCalls = 100
)

func clInt(n int64) resp.Value  { return resp.Value{Kind: resp.Int, Int: n} }
func clStr(s string) resp.Value { return resp.Value{Kind: resp.Bulk, Str: s} }

// clOK is an OK response carrying the decoded reply {code, t, reason, vals…}.
func clOK(code, t int64, reason string, vals ...resp.Value) store.Response {
	elems := append([]resp.Value{clInt(code), clInt(t), clStr(reason)}, vals...)
	return store.Response{Outcome: store.OK, Sent: true, SentCommands: 1,
		Reply: store.Reply{Code: code, TUs: t, Reason: reason, Elems: elems}}
}

// clEpochResp is a read_epoch response (§A4.9): state 0 for the record of id with fingerprint
// fp, hwm and gen; or state 22 with reason when reason is not "".
func clEpochResp(id, fp, reason string, hwm, gen int64) store.Response {
	if reason != "" {
		return clOK(22, 0, reason, clStr(""), clStr(""), clInt(0), clInt(0), clInt(0), clInt(0), clInt(0), clStr(""))
	}
	return clOK(0, 0, "", clStr(id), clStr(fp), clInt(hwm), clInt(gen), clInt(clT0), clInt(0), clInt(0), clInt(0))
}

// clPage is a due_entries page {0, T0, ”, oldest, member…}.
func clPage(t0, oldest int64, members ...string) store.Response {
	vals := []resp.Value{clInt(oldest)}
	for _, m := range members {
		vals = append(vals, clStr(m))
	}
	return clOK(0, t0, "", vals...)
}

// clStep is a cleanup_entry reply {code, t, reason, markers_removed}.
func clStep(code int64, reason string) store.Response {
	return clOK(code, clT0+1, reason, clInt(0))
}

// cleanFake is the store adapter of the cleanup tests (hookSet.storeWrapper): read_epoch
// answers with the epoch response; due_entries (up to clDueCalls calls), the cleanup_entry
// pipelines (CallBatch), SCAN (Do) and reclaim go to the test's functions (unavailable, nothing
// sent, when unset). It records every call except read_epoch, in order, as a line, keeps the
// requests, and counts the pipelines in flight; hold, when set, runs inside each CallBatch
// while it counts as in flight.
type cleanFake struct {
	mu       sync.Mutex
	closed   int // the number of recorded calls when Close came; -1 before
	epoch    store.Response
	due      func(req store.Request) store.Response
	step     func(member string) store.Response
	scan     func(cursor string) (resp.Value, store.Response)
	reclaim  func(req store.Request) store.Response
	hold     func()
	calls    []string
	reqs     []store.Request   // the due_entries and reclaim requests, in order
	batches  [][]store.Request // the CallBatch requests, one slice per pipeline, in order of return
	inFlight int               // CallBatch calls in progress
	most     int               // the most CallBatch calls in progress at once
}

func (f *cleanFake) wrap(inner store.Adapter) store.Adapter {
	inner.Close()
	return f
}

func (f *cleanFake) set(fn func(f *cleanFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *cleanFake) record(line string, req *store.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, line)
	if req != nil {
		f.reqs = append(f.reqs, *req)
	}
}

func (f *cleanFake) Call(_ context.Context, req store.Request) store.Response {
	f.mu.Lock()
	ep, due, rec := f.epoch, f.due, f.reclaim
	f.mu.Unlock()
	switch req.Script.Name {
	case "read_epoch":
		return ep
	case "due_entries":
		f.record("due_entries cutoff="+req.Args[3], &req)
		if due != nil && len(f.requests("due_entries")) <= clDueCalls {
			return due(req)
		}
	case "reclaim":
		f.record(fmt.Sprintf("reclaim %d", len(req.Keys)-1), &req)
		if rec != nil {
			return rec(req)
		}
	default:
		f.record("call "+req.Script.Name, &req)
	}
	return store.Response{Outcome: store.Unavailable}
}

func (f *cleanFake) CallBatch(_ context.Context, reqs []store.Request) []store.Response {
	f.mu.Lock()
	step, hold := f.step, f.hold
	f.inFlight++
	f.most = max(f.most, f.inFlight)
	f.mu.Unlock()
	if hold != nil {
		hold()
	}
	members := make([]string, len(reqs))
	out := make([]store.Response, len(reqs))
	for i, r := range reqs {
		members[i] = r.Args[2]
		out[i] = store.Response{Outcome: store.Unavailable}
		if step != nil && r.Script.Name == "cleanup_entry" {
			out[i] = step(members[i])
		}
	}
	f.mu.Lock()
	f.inFlight--
	f.batches = append(f.batches, slices.Clone(reqs))
	f.mu.Unlock()
	f.record(fmt.Sprintf("cleanup_entry %v", members), nil)
	return out
}

func (f *cleanFake) Do(_ context.Context, _ store.Kind, args ...string) (resp.Value, store.Response) {
	f.mu.Lock()
	scan := f.scan
	f.mu.Unlock()
	f.record(fmt.Sprintf("%v", args), nil)
	if scan != nil && len(args) == 6 && args[0] == "SCAN" {
		return scan(args[1])
	}
	return resp.Value{}, store.Response{Outcome: store.Unavailable}
}

func (f *cleanFake) ReadyConns(store.Kind) int { return 0 }

// Close notes how many calls came before it; wrap closed the real adapter.
func (f *cleanFake) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed < 0 {
		f.closed = len(f.calls)
	}
}

// callsAfterClose returns the calls recorded after Close, or all of them before Close.
func (f *cleanFake) callsAfterClose() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls[max(f.closed, 0):])
}

// log returns the recorded calls.
func (f *cleanFake) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// pipelines returns the CallBatch requests, one slice per pipeline, and the most pipelines
// that were in flight at once.
func (f *cleanFake) pipelines() ([][]store.Request, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.batches), f.most
}

// requests returns the recorded requests of one script.
func (f *cleanFake) requests(script string) []store.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Request
	for _, r := range f.reqs {
		if r.Script.Name == script {
			out = append(out, r)
		}
	}
	return out
}

// scanDone answers SCAN with an ended iteration and no keys.
func scanDone(string) (resp.Value, store.Response) {
	return scanValue("0"), store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
}

// scanValue is a SCAN reply [cursor, [key…]].
func scanValue(cursor string, found ...string) resp.Value {
	list := make([]resp.Value, len(found))
	for i, k := range found {
		list[i] = clStr(k)
	}
	return resp.Value{Kind: resp.Array, Elems: []resp.Value{clStr(cursor), {Kind: resp.Array, Elems: list}}}
}

// scanCursors returns the cursors of the SCAN calls among the recorded calls, in order.
func scanCursors(log []string) []string {
	var out []string
	for _, line := range log {
		if rest, ok := strings.CutPrefix(line, "[SCAN "); ok {
			out = append(out, strings.Fields(rest)[0])
		}
	}
	return out
}

// cleanSettings are lifecycleSettings with timed passes off (a 1 h cleanup interval).
func cleanSettings() (Policy, Instance) {
	p, in := lifecycleSettings()
	p.CleanupIntervalNs = clHour
	return p, in
}

// cleanLimiter builds a limiter on f with settings p and in (newLimiter, as New would) and
// waits until its health reason is reason; t's cleanup closes it.
func cleanLimiter(t *testing.T, f *cleanFake, p Policy, in Instance, reason string) *Limiter {
	t.Helper()
	return cleanLimiterHooks(t, p, in, reason, hookSet{storeWrapper: f.wrap})
}

// cleanLimiterHooks is cleanLimiter with the hook set h, which holds the fake's wrapper; an
// unset jitter draw gets the production one, as New would pass. t's cleanup closes it within
// lifecycleWait (closeInCleanup).
func cleanLimiterHooks(t *testing.T, p Policy, in Instance, reason string, h hookSet) *Limiter {
	t.Helper()
	if h.cleanupDraw == nil {
		h.cleanupDraw = jitterDraw
	}
	l, err := newLimiter(p, in, StoreOptions{}, h, nil)
	t.Cleanup(func() { closeInCleanup(t, l) })
	if err != nil {
		t.Fatalf("newLimiter: %v", err)
	}
	pollUntil(t, "health reason "+reason, func() bool { return l.Health().Reason == reason })
	return l
}

// closeInCleanup is the cleanup Close of a limiter a cleanup test built: it fails the test
// with Errorf, so the remaining cleanups and tests still run, when Close does not return
// within lifecycleWait, such as when the cleanup runner never stops (API-1e); the stuck Close
// is left behind. These limiters hold no permit, so Close has nothing to drain.
func closeInCleanup(t *testing.T, l *Limiter) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Close()
	}()
	timer := time.NewTimer(lifecycleWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Errorf("Close did not return within %v", lifecycleWait)
	}
}

// joinedFake is a fake whose read_epoch shows clEpoch with p's fingerprint, hwm clT0 − 9 and
// gen 4, and whose SCAN ends at once.
func joinedFake(p Policy) *cleanFake {
	return &cleanFake{closed: -1, epoch: clEpochResp(clEpoch, fingerprint.Of(p), "", clT0-9, 4), scan: scanDone}
}

// metricValue is a series' value (a counter's count, a gauge's value).
func metricValue(t *testing.T, l *Limiter, name, label string) float64 {
	t.Helper()
	return gaugeValue(t, l.Metrics(), name, label)
}

// checkReport fails the test unless rep is want.
func checkReport(t *testing.T, what string, rep, want CleanupReport) {
	t.Helper()
	if rep != want {
		t.Errorf("%s: report %+v, want %+v", what, rep, want)
	}
}

// TestCleanupPassProcedure (plan S7, L1; DESIGN §A4.5, §A5.8; SPEC API-1f): against a fake
// adapter, RunCleanupPass calls due_entries with cutoff 0, keeps the first page's T0 as the
// cutoff of every later page (whatever their own T0), fetches the next page only after the
// page's steps complete, and stops at an empty page, then runs the reclaimer; the steps count
// 30 removed, 31 moved, 32 quarantined, 33 skipped (1, 2, 3 and 4 of them, so each count
// reaches its own rl_cleanup_entries_total{action} series), and the metrics follow. A failed
// call stops the pass with its status at once: no further page, no reclaim, only confirmed
// steps counted, each stop in rl_cleanup_stops_total. An instance that is not ready returns
// its status at once with no adapter call, also while another pass holds the pass lock.
func TestCleanupPassProcedure(t *testing.T) {
	t.Run("pages", func(t *testing.T) {
		p, in := cleanSettings()
		f := joinedFake(p)
		f.due = func(req store.Request) store.Response {
			switch n := len(f.requests("due_entries")); n {
			case 1:
				return clPage(clT0, clT0-5_000_000, "m1", "m2", "m3", "m4", "m5", "m6")
			case 2:
				return clPage(clT0+70, clT0-1, "m7", "m8", "m9", "m10")
			}
			return clPage(clT0+90, 0)
		}
		steps := map[string]store.Response{
			"m1": clStep(30, ""),
			"m2": clStep(31, "quarantined"), "m3": clStep(31, ""),
			"m4": clStep(32, "malformed_bucket"), "m5": clStep(32, "marker_count"), "m6": clStep(32, "malformed_marker"),
			"m7": clStep(33, "gone"), "m8": clStep(33, "not_due"), "m9": clStep(33, "gone"), "m10": clStep(33, "not_due"),
		}
		f.step = func(m string) store.Response { return steps[m] }
		l := cleanLimiter(t, f, p, in, reasonReady)
		rep := l.RunCleanupPass(t.Context())
		checkReport(t, "pass", rep, CleanupReport{Status: "complete", Removed: 1, Moved: 2, Quarantined: 3, Skipped: 4})
		cutoff := strconv.Itoa(clT0)
		want := []string{
			"due_entries cutoff=0", "cleanup_entry [m1 m2 m3 m4 m5 m6]",
			"due_entries cutoff=" + cutoff, "cleanup_entry [m7 m8 m9 m10]",
			"due_entries cutoff=" + cutoff,
			"[SCAN 0 MATCH rl:e:* COUNT 1000]",
		}
		if got := f.log(); !slices.Equal(got, want) {
			t.Errorf("calls %q, want %q", got, want)
		}
		for i, r := range f.requests("due_entries") {
			wantArgs := []string{clEpoch, fingerprint.Of(p), strconv.Itoa(in.CleanupBatch), []string{"0", cutoff, cutoff}[i], strconv.Itoa(clT0 - 9), "4"}
			if !slices.Equal(r.Args, wantArgs) || r.Kind != store.Maintenance {
				t.Errorf("due_entries %d: kind %v, ARGV %q; want maintenance, %q", i+1, r.Kind, r.Args, wantArgs)
			}
		}
		for _, c := range []struct {
			name, label string
			want        float64
		}{
			{"rl_cleanup_passes_total", "complete", 1},
			{"rl_cleanup_entries_total", "removed", 1}, {"rl_cleanup_entries_total", "moved", 2},
			{"rl_cleanup_entries_total", "quarantined", 3}, {"rl_cleanup_entries_total", "skipped", 4},
			{"rl_cleanup_lag_seconds", "", 5}, // (T0 − oldest) / 10^6 of the first page
		} {
			if got := metricValue(t, l, c.name, c.label); got != c.want {
				t.Errorf("%s{%q} = %v, want %v", c.name, c.label, got, c.want)
			}
		}
		// A pass whose first page is empty: lag 0, complete, nothing counted.
		f.set(func(f *cleanFake) { f.due = func(store.Request) store.Response { return clPage(clT0+100, 0) } })
		checkReport(t, "an empty pass", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
		if got := metricValue(t, l, "rl_cleanup_lag_seconds", ""); got != 0 {
			t.Errorf("rl_cleanup_lag_seconds after an empty first page = %v, want 0", got)
		}
		if got := metricValue(t, l, "rl_cleanup_passes_total", "complete"); got != 2 {
			t.Errorf("rl_cleanup_passes_total{complete} = %v, want 2", got)
		}
	})

	// One page of 450 members: the core's cleanup_entry ARGV (§A4.1: epoch_id, fp, member,
	// cap_units, rate_tokens, push_us and the joined record's floors), and the steps go out in
	// pipelines of 100 with CleanupPipelines (4) in flight at once: each pipeline waits until 4
	// are in flight, or lifecycleWait has passed.
	t.Run("cleanup_entry requests", func(t *testing.T) {
		p, in := cleanSettings()
		f := joinedFake(p)
		ms := make([]string, 450)
		for i := range ms {
			ms[i] = fmt.Sprintf("m%d", i+1)
		}
		f.due = func(store.Request) store.Response {
			if len(f.requests("due_entries")) == 1 {
				return clPage(clT0, clT0, ms...)
			}
			return clPage(clT0+5, 0)
		}
		f.step = func(string) store.Response { return clStep(33, "gone") }
		gate := make(chan struct{})
		var opened sync.Once
		f.hold = func() {
			f.mu.Lock()
			n := f.inFlight
			f.mu.Unlock()
			if n >= in.CleanupPipelines {
				opened.Do(func() { close(gate) })
			}
			select {
			case <-gate:
			case <-time.After(lifecycleWait):
				opened.Do(func() { close(gate) })
			}
		}
		l := cleanLimiter(t, f, p, in, reasonReady)
		checkReport(t, "pass", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete", Skipped: 450})
		batches, most := f.pipelines()
		var sizes []int
		var m1 *store.Request
		for _, b := range batches {
			sizes = append(sizes, len(b))
			for i := range b {
				if b[i].Args[2] == "m1" {
					m1 = &b[i]
				}
			}
		}
		slices.Sort(sizes)
		if !slices.Equal(sizes, []int{50, 100, 100, 100, 100}) || most != 4 {
			t.Errorf("pipelines of %v, %d in flight at most; want 100, 100, 100, 100 and 50, 4 in flight at once", sizes, most)
		}
		// The default policy: cap_units = capacity × rate_period_ns = 20 × 10^9, rate_tokens 10,
		// push_us = receipt_lifetime_ns / 1000 = 60 × 10^9 / 1000; the floors are the joined
		// record's hwm (clT0 − 9) and gen (4).
		want := []string{clEpoch, fingerprint.Of(p), "m1", "20000000000", "10", "60000000", strconv.Itoa(clT0 - 9), "4"}
		switch {
		case m1 == nil:
			t.Errorf("no cleanup_entry request for m1")
		case m1.Script.Name != "cleanup_entry" || m1.Kind != store.Maintenance || !slices.Equal(m1.Args, want):
			t.Errorf("m1's request: script %s, kind %v, ARGV %q; want cleanup_entry, maintenance, %q", m1.Script.Name, m1.Kind, m1.Args, want)
		}
	})

	unavailable := store.Response{Outcome: store.Unavailable}
	unknown := store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: "ERR test"}
	stops := []struct {
		name   string
		due1   store.Response // the first page's response
		due2   store.Response // the second page's (when the first is a page)
		steps  []store.Response
		want   CleanupReport
		label  string
		nDue   int    // due_entries calls made
		nBatch int    // pipelines sent
		health string // Health.StoreError afterwards: the last error reply seen
	}{
		{name: "first page config_error", due1: clOK(20, 0, "epoch_or_fingerprint"),
			want: CleanupReport{Status: "config_error", Diag: Diag{Reason: "epoch_or_fingerprint"}}, label: "config_error", nDue: 1},
		{name: "first page clock_error", due1: clOK(21, 0, "clock"),
			want: CleanupReport{Status: "clock_error", Diag: Diag{Reason: "clock"}}, label: "clock_error", nDue: 1},
		{name: "first page state_integrity_error", due1: clOK(22, 0, "integrity_hold"),
			want: CleanupReport{Status: "state_integrity_error", Diag: Diag{Scope: scopeEpoch, Reason: "integrity_hold"}}, label: "state_integrity_error", nDue: 1},
		{name: "first page unavailable", due1: unavailable, want: CleanupReport{Status: "unavailable"}, label: "unavailable", nDue: 1},
		{name: "first page cancelled", due1: store.Response{Outcome: store.Cancelled}, want: CleanupReport{Status: "unavailable"}, label: "unavailable", nDue: 1},
		{name: "first page outcome_unknown", due1: unknown, want: CleanupReport{Status: "outcome_unknown", Diag: Diag{StoreError: "ERR test"}}, label: "outcome_unknown", nDue: 1, health: "ERR test"},
		{name: "first page capacity_error", due1: store.Response{Outcome: store.CapacityError, StoreError: "OOM test"},
			want: CleanupReport{Status: "outcome_unknown", Diag: Diag{StoreError: "OOM test"}}, label: "outcome_unknown", nDue: 1, health: "OOM test"},
		{name: "second page clock_error", due1: clPage(clT0, clT0, "m1"), due2: clOK(21, 0, "clock"), steps: []store.Response{clStep(30, "")},
			want: CleanupReport{Status: "clock_error", Removed: 1, Diag: Diag{Reason: "clock"}}, label: "clock_error", nDue: 2, nBatch: 1},
		{name: "a step state_integrity_error", due1: clPage(clT0, clT0, "m1", "m2"), steps: []store.Response{clStep(31, ""), clOK(22, 0, "hwm_missing")},
			want: CleanupReport{Status: "state_integrity_error", Moved: 1, Diag: Diag{Scope: scopeEpoch, Reason: "hwm_missing"}}, label: "state_integrity_error", nDue: 1, nBatch: 1},
		{name: "a step config_error", due1: clPage(clT0, clT0, "m1"), steps: []store.Response{clOK(20, 0, "epoch_or_fingerprint")},
			want: CleanupReport{Status: "config_error", Diag: Diag{Reason: "epoch_or_fingerprint"}}, label: "config_error", nDue: 1, nBatch: 1},
		{name: "a step clock_error", due1: clPage(clT0, clT0, "m1"), steps: []store.Response{clOK(21, 0, "clock")},
			want: CleanupReport{Status: "clock_error", Diag: Diag{Reason: "clock"}}, label: "clock_error", nDue: 1, nBatch: 1},
		{name: "a step keys_mismatch (a library defect)", due1: clPage(clT0, clT0, "m1"), steps: []store.Response{clOK(25, 0, "keys_mismatch")},
			want: CleanupReport{Status: "outcome_unknown", Diag: Diag{Reason: "keys_mismatch"}}, label: "outcome_unknown", nDue: 1, nBatch: 1},
		{name: "a step unavailable", due1: clPage(clT0, clT0, "m1", "m2"), steps: []store.Response{clStep(33, "gone"), unavailable},
			want: CleanupReport{Status: "unavailable", Skipped: 1}, label: "unavailable", nDue: 1, nBatch: 1},
		// Unknown steps are not counted; the confirmed ones around them are.
		{name: "a step outcome_unknown", due1: clPage(clT0, clT0, "m1", "m2", "m3"), steps: []store.Response{clStep(30, ""), unknown, clStep(31, "")},
			want: CleanupReport{Status: "outcome_unknown", Removed: 1, Moved: 1, Diag: Diag{StoreError: "ERR test"}}, label: "outcome_unknown", nDue: 1, nBatch: 1, health: "ERR test"},
		// The first failed step, in member order, gives the status.
		{name: "two failed steps", due1: clPage(clT0, clT0, "m1", "m2"), steps: []store.Response{clOK(21, 0, "clock"), unknown},
			want: CleanupReport{Status: "clock_error", Diag: Diag{Reason: "clock"}}, label: "clock_error", nDue: 1, nBatch: 1, health: "ERR test"},
	}
	for _, c := range stops {
		t.Run(c.name, func(t *testing.T) {
			p, in := cleanSettings()
			f := joinedFake(p)
			f.due = func(store.Request) store.Response {
				if len(f.requests("due_entries")) == 1 {
					return c.due1
				}
				return c.due2
			}
			f.step = func(m string) store.Response {
				i, _ := strconv.Atoi(m[1:])
				return c.steps[i-1]
			}
			l := cleanLimiter(t, f, p, in, reasonReady)
			checkReport(t, c.name, l.RunCleanupPass(t.Context()), c.want)
			due, batches, scans := 0, 0, 0
			for _, line := range f.log() {
				switch {
				case len(line) > 11 && line[:11] == "due_entries":
					due++
				case len(line) > 13 && line[:13] == "cleanup_entry":
					batches++
				case len(line) > 5 && line[:5] == "[SCAN":
					scans++
				}
			}
			if due != c.nDue || batches != c.nBatch || scans != 0 {
				t.Errorf("%d due_entries, %d pipelines, %d SCAN; want %d, %d and no reclaim after a stopped pass (%q)", due, batches, scans, c.nDue, c.nBatch, f.log())
			}
			if got := metricValue(t, l, "rl_cleanup_stops_total", c.label); got != 1 {
				t.Errorf("rl_cleanup_stops_total{%q} = %v, want 1", c.label, got)
			}
			if got := metricValue(t, l, "rl_cleanup_passes_total", c.label); got != 1 {
				t.Errorf("rl_cleanup_passes_total{%q} = %v, want 1", c.label, got)
			}
			if got := metricValue(t, l, "rl_cleanup_passes_total", "complete"); got != 0 {
				t.Errorf("rl_cleanup_passes_total{complete} = %v, want 0", got)
			}
			if got := l.Health().StoreError; got != c.health {
				t.Errorf("Health.StoreError %q, want %q", got, c.health)
			}
		})
	}

	// A page that fails a check the decoder already made is outcome_unknown all the same
	// (CleanupBatch 3 here); each bound is pinned from both sides.
	pageInts := func(vals ...any) store.Response {
		var vs []resp.Value
		for _, v := range vals[3:] {
			switch v := v.(type) {
			case int:
				vs = append(vs, clInt(int64(v)))
			case string:
				vs = append(vs, clStr(v))
			}
		}
		return clOK(int64(vals[0].(int)), int64(vals[1].(int)), vals[2].(string), vs...)
	}
	for _, c := range []struct {
		name string
		page store.Response
		ok   bool
	}{
		{"T0 at TBOUND", clPage(tBound, 0, "m1"), false},
		{"T0 just below TBOUND", clPage(tBound-1, 0, "m1"), true},
		{"T0 negative", clPage(-1, -1, "m1"), false},
		{"T0 zero", clPage(0, 0, "m1"), true},
		{"oldest negative", clPage(clT0, -1, "m1"), false},
		{"oldest after T0", clPage(clT0, clT0+1, "m1"), false},
		{"oldest at T0", clPage(clT0, clT0, "m1"), true},
		{"4 members, above CleanupBatch", clPage(clT0, 0, "m1", "m2", "m3", "m4"), false},
		{"3 members, CleanupBatch", clPage(clT0, 0, "m1", "m2", "m3"), true},
		{"a member not a string", pageInts(0, clT0, "", 0, "m1", 7), false},
		{"oldest not an integer", pageInts(0, clT0, "", "x", "m1"), false},
		{"no oldest", pageInts(0, clT0, ""), false},
		{"another code", clOK(23, clT0, "quarantined"), false},
	} {
		t.Run("page: "+c.name, func(t *testing.T) {
			p, in := cleanSettings()
			in.CleanupBatch = 3
			f := joinedFake(p)
			f.due = func(store.Request) store.Response {
				if len(f.requests("due_entries")) == 1 {
					return c.page
				}
				return clPage(clT0+5, 0)
			}
			f.step = func(string) store.Response { return clStep(33, "gone") }
			l := cleanLimiter(t, f, p, in, reasonReady)
			rep := l.RunCleanupPass(t.Context())
			members := len(c.page.Reply.Elems) - 4
			switch {
			case c.ok && (rep.Status != "complete" || rep.Skipped != int64(members)):
				t.Errorf("report %+v, want complete with %d skipped", rep, members)
			case !c.ok && (rep.Status != "outcome_unknown" || rep != CleanupReport{Status: "outcome_unknown"}):
				t.Errorf("report %+v, want outcome_unknown, nothing counted", rep)
			}
		})
	}

	notReady := []struct {
		name   string
		build  func(t *testing.T, f *cleanFake) *Limiter
		ctx    func(t *testing.T) context.Context
		status string
		diag   Diag
	}{
		{name: "settings_invalid", build: func(t *testing.T, f *cleanFake) *Limiter {
			p, in := cleanSettings()
			in.MarginNs = 59_000_000 // RCP-8: 59 ms × 10^6 < 60 s × 1,000 ppm
			l, err := newLimiter(p, in, StoreOptions{}, hookSet{storeWrapper: f.wrap, cleanupDraw: jitterDraw}, nil)
			t.Cleanup(func() { l.Close() })
			if err == nil {
				t.Fatal("settings that fail RCP-8 were accepted")
			}
			return l
		}, status: "config_error", diag: Diag{Reason: reasonSettingsInvalid}},
		{name: "fingerprint_mismatch", build: func(t *testing.T, f *cleanFake) *Limiter {
			p, in := cleanSettings()
			f.epoch = clEpochResp(clEpoch, "ffffffffffffffffffffffffffffffff", "", clT0, 4)
			return cleanLimiter(t, f, p, in, reasonMismatch)
		}, status: "config_error", diag: Diag{Reason: reasonMismatch}},
		{name: "no_epoch", build: func(t *testing.T, f *cleanFake) *Limiter {
			p, in := cleanSettings()
			f.epoch = clEpochResp("", "", reasonRecordMissing, 0, 0)
			return cleanLimiter(t, f, p, in, reasonNoEpoch)
		}, status: "state_integrity_error", diag: Diag{Scope: scopeEpoch, Reason: reasonNoEpoch}},
		{name: "state_lost", build: func(t *testing.T, f *cleanFake) *Limiter {
			p, in := cleanSettings()
			l := cleanLimiter(t, f, p, in, reasonReady)
			f.set(func(f *cleanFake) { f.epoch = clEpochResp("", "", "hwm_missing", 0, 0) })
			l.watch.read(t.Context())
			if r := l.Health().Reason; r != reasonStateLost {
				t.Fatalf("health %q after hwm_missing, want state_lost", r)
			}
			return l
		}, status: "state_integrity_error", diag: Diag{Scope: scopeEpoch, Reason: "hwm_missing"}},
		{name: "integrity_hold", build: func(t *testing.T, f *cleanFake) *Limiter {
			p, in := cleanSettings()
			f.epoch = clEpochResp("", "", reasonIntegrityHold, 0, 0)
			return cleanLimiter(t, f, p, in, reasonIntegrityHold)
		}, status: "state_integrity_error", diag: Diag{Scope: scopeEpoch, Reason: reasonIntegrityHold}},
		{name: "closing", build: func(t *testing.T, f *cleanFake) *Limiter {
			p, in := cleanSettings()
			l := cleanLimiter(t, f, p, in, reasonReady)
			closeWithin(t, l, lifecycleWait)
			return l
		}, status: "unavailable", diag: Diag{Reason: reasonClosing}},
		{name: "ctx already ended", build: func(t *testing.T, f *cleanFake) *Limiter {
			p, in := cleanSettings()
			return cleanLimiter(t, f, p, in, reasonReady)
		}, ctx: func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		}, status: "unavailable"},
	}
	for _, c := range notReady {
		t.Run("not ready: "+c.name, func(t *testing.T) {
			p, _ := cleanSettings()
			f := joinedFake(p)
			f.due = func(store.Request) store.Response { return clPage(clT0, clT0, "m1") }
			f.step = func(string) store.Response { return clStep(30, "") }
			l := c.build(t, f)
			ctx := t.Context()
			if c.ctx != nil {
				ctx = c.ctx(t)
			}
			checkReport(t, c.name, l.RunCleanupPass(ctx), CleanupReport{Status: c.status, Diag: c.diag})
			if got := f.log(); len(got) != 0 {
				t.Errorf("adapter calls %q, want none", got)
			}
			if got := metricValue(t, l, "rl_cleanup_stops_total", c.status); got != 1 {
				t.Errorf("rl_cleanup_stops_total{%q} = %v, want 1", c.status, got)
			}
		})
	}

	t.Run("passes serialized", func(t *testing.T) {
		p, in := cleanSettings()
		f := joinedFake(p)
		release := make(chan struct{})
		entered := make(chan struct{}, 4)
		var mu sync.Mutex
		inPass, maxIn := 0, 0
		f.due = func(store.Request) store.Response {
			mu.Lock()
			inPass++
			maxIn = max(maxIn, inPass)
			mu.Unlock()
			entered <- struct{}{}
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			mu.Lock()
			inPass--
			mu.Unlock()
			return clPage(clT0, 0)
		}
		l := cleanLimiter(t, f, p, in, reasonReady)
		first := make(chan CleanupReport, 1)
		go func() { first <- l.RunCleanupPass(t.Context()) }()
		<-entered
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		checkReport(t, "a pass whose ctx ends while another runs", l.RunCleanupPass(ctx), CleanupReport{Status: "unavailable"})
		second := make(chan CleanupReport, 1)
		go func() { second <- l.RunCleanupPass(t.Context()) }()
		time.Sleep(50 * time.Millisecond)
		if n := len(f.requests("due_entries")); n != 1 {
			t.Errorf("%d due_entries while the first pass runs, want 1: the second pass waits", n)
		}
		close(release)
		for i, ch := range []chan CleanupReport{first, second} {
			select {
			case rep := <-ch:
				checkReport(t, fmt.Sprintf("pass %d", i+1), rep, CleanupReport{Status: "complete"})
			case <-time.After(5 * time.Second):
				t.Fatalf("pass %d did not return", i+1)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if maxIn != 1 {
			t.Errorf("%d passes ran at once, want 1", maxIn)
		}
	})

	// A pass in progress holds the pass lock (the fake holds its first due_entries); then the
	// instance stops being ready, and RunCleanupPass, with a ctx that would end 1 s later,
	// returns its readiness status at once (§A5.8), with no adapter call, instead of waiting
	// behind that pass and returning unavailable when its ctx ends.
	behind := []struct {
		name    string
		unready func(t *testing.T, f *cleanFake, l *Limiter)
		health  string
		status  string
		diag    Diag
	}{
		{name: "state_lost", unready: func(t *testing.T, f *cleanFake, l *Limiter) {
			f.set(func(f *cleanFake) { f.epoch = clEpochResp("", "", "hwm_missing", 0, 0) })
			l.watch.read(t.Context())
		}, health: reasonStateLost, status: "state_integrity_error", diag: Diag{Scope: scopeEpoch, Reason: "hwm_missing"}},
		{name: "integrity_hold", unready: func(t *testing.T, f *cleanFake, l *Limiter) {
			f.set(func(f *cleanFake) { f.epoch = clEpochResp("", "", reasonIntegrityHold, 0, 0) })
			l.watch.read(t.Context())
		}, health: reasonIntegrityHold, status: "state_integrity_error", diag: Diag{Scope: scopeEpoch, Reason: reasonIntegrityHold}},
		{name: "fingerprint_mismatch", unready: func(t *testing.T, f *cleanFake, l *Limiter) {
			f.set(func(f *cleanFake) { f.epoch = clEpochResp(clEpoch, "ffffffffffffffffffffffffffffffff", "", clT0, 4) })
			l.watch.read(t.Context())
		}, health: reasonMismatch, status: "config_error", diag: Diag{Reason: reasonMismatch}},
		{name: "closing", unready: func(t *testing.T, _ *cleanFake, l *Limiter) {
			closeWithin(t, l, lifecycleWait) // Close does not wait for a pass the host runs
		}, health: reasonClosing, status: "unavailable", diag: Diag{Reason: reasonClosing}},
	}
	for _, c := range behind {
		t.Run("not ready behind a pass in progress: "+c.name, func(t *testing.T) {
			p, in := cleanSettings()
			f := joinedFake(p)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.due = func(store.Request) store.Response {
				once.Do(func() {
					close(entered)
					select {
					case <-release:
					case <-time.After(lifecycleWait):
					}
				})
				return clPage(clT0, 0)
			}
			l := cleanLimiter(t, f, p, in, reasonReady)
			first := make(chan CleanupReport, 1)
			go func() { first <- l.RunCleanupPass(t.Context()) }()
			defer func() {
				close(release)
				select {
				case <-first:
				case <-time.After(lifecycleWait):
					t.Errorf("the pass in progress did not return within %v of its release", lifecycleWait)
				}
			}()
			select {
			case <-entered:
			case <-time.After(lifecycleWait):
				t.Fatalf("no pass called due_entries within %v", lifecycleWait)
			}
			c.unready(t, f, l)
			if r := l.Health().Reason; r != c.health {
				t.Fatalf("health %q, want %q", r, c.health)
			}
			before := f.log()
			const wait = time.Second
			began := time.Now()
			ctx, cancel := context.WithTimeout(t.Context(), wait)
			defer cancel()
			rep := l.RunCleanupPass(ctx)
			el := time.Since(began)
			checkReport(t, "a pass of an instance no longer ready", rep, CleanupReport{Status: c.status, Diag: c.diag})
			if el >= wait {
				t.Errorf("returned %v after the call, want at once: it waited behind the pass in progress", el)
			}
			if got := f.log(); len(got) != len(before) {
				t.Errorf("adapter calls %q, want none", got[len(before):])
			}
		})
	}
}

// TestCleanupJitter (plan S7, L1; DESIGN §A5.1): the runner's period is cleanup_interval ×
// U[0.9, 1.0]: 10,000 periods from seeded uniform draws all lie in [0.9, 1.0] × interval and
// spread over it (some within 1 % of each end), for the default 5 s and for an interval that
// 10 does not divide; u = 0 gives exactly interval − ⌊interval / 10⌋, and the largest u below
// 1 stays at or below the interval.
func TestCleanupJitter(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for _, interval := range []time.Duration{5 * time.Second, 1_234_567_000} {
		lo, hi := (9*interval+9)/10, interval // ⌈0.9 × interval⌉ and interval
		minP, maxP := hi, lo
		for range 10_000 {
			d := cleanupPeriod(interval, r.Float64())
			if d < lo || d > hi {
				t.Fatalf("interval %v: period %v outside [%v, %v]", interval, d, lo, hi)
			}
			minP, maxP = min(minP, d), max(maxP, d)
		}
		if spread := interval / 100; minP > lo+spread || maxP < hi-spread {
			t.Errorf("interval %v: periods in [%v, %v], want some within %v of %v and of %v", interval, minP, maxP, spread, lo, hi)
		}
		if got := cleanupPeriod(interval, 0); got != interval-interval/10 {
			t.Errorf("interval %v, u = 0: %v, want %v", interval, got, interval-interval/10)
		}
		if got := cleanupPeriod(interval, math.Nextafter(1, 0)); got > interval || got < interval-1_000 {
			t.Errorf("interval %v, u just below 1: %v, want at most the interval and within 1 µs of it", interval, got)
		}
	}
}

// TestCleanupRunner (plan S7 steps 5 and 8, L1; DESIGN §A5.1, §A5.8; SPEC API-1e, STA-5):
// with a 20 ms cleanup interval the runner runs timed passes on its own, each counted; it
// draws the jitter once per period (hookSet.cleanupDraw: one draw for the first wait and one
// after each pass) and waits cleanup_interval × U[0.9, 1.0] for that draw: with every draw just
// below 1, each wait is at least 18 ms + ⌊2 ms × u⌋ = 19,999,999 ns, so consecutive draws are
// at least that far apart. Close stops it before it closes the store adapter, and once Close
// returns no pass follows.
func TestCleanupRunner(t *testing.T) {
	p, in := cleanSettings()
	p.CleanupIntervalNs = 20_000_000
	const minWait = 19_999_999 * time.Nanosecond // cleanupPeriod(20 ms, u) for u just below 1
	u := math.Nextafter(1, 0)
	f := joinedFake(p)
	f.due = func(store.Request) store.Response { return clPage(clT0, 0) }
	var mu sync.Mutex
	var draws []time.Time
	draw := func() float64 {
		mu.Lock()
		defer mu.Unlock()
		draws = append(draws, time.Now())
		return u
	}
	l := cleanLimiterHooks(t, p, in, reasonReady, hookSet{storeWrapper: f.wrap, cleanupDraw: draw})
	pollUntil(t, "3 timed passes", func() bool { return len(f.requests("due_entries")) >= 3 })
	closeWithin(t, l, lifecycleWait)
	after := len(f.requests("due_entries"))
	if got := metricValue(t, l, "rl_cleanup_passes_total", "complete"); got < 3 {
		t.Errorf("rl_cleanup_passes_total{complete} = %v after 3 timed passes, want at least 3", got)
	}
	passes := 0.0 // every timed pass, whatever its status (one before the watch joined is not ready)
	for _, label := range []string{"complete", "state_integrity_error", "config_error", "clock_error", "unavailable", "outcome_unknown"} {
		passes += metricValue(t, l, "rl_cleanup_passes_total", label)
	}
	mu.Lock()
	drawn := slices.Clone(draws)
	mu.Unlock()
	if float64(len(drawn)) != passes+1 {
		t.Errorf("%d jitter draws for %v timed passes, want one per period: %v", len(drawn), passes, passes+1)
	}
	for i := 1; i < len(drawn); i++ {
		if gap := drawn[i].Sub(drawn[i-1]); gap < minWait {
			t.Errorf("draws %d and %d %v apart, want at least %v: the wait after draw %d is not cleanup_interval × U[0.9, 1.0] for it", i, i+1, gap, minWait, i)
		}
	}
	time.Sleep(100 * time.Millisecond) // five intervals
	if n := len(f.requests("due_entries")); n != after {
		t.Errorf("%d due_entries after Close returned, then %d: the runner did not stop", after, n)
	}
	if got := f.callsAfterClose(); len(got) != 0 {
		t.Errorf("calls after the adapter closed: %q, want none (the runner stops first)", got)
	}
}

// TestCleanupRunnerCloseWaits (plan S7 step 8, L1; SPEC API-1e; AGENTS.md §2 goroutines): Close
// waits for a timed pass that is inside a store call, since the runner is one of the
// instance's goroutines: while the fake holds the pass's first due_entries, Close has not
// returned 200 ms later; once the call is released, Close returns, and no call reaches the
// adapter after it closed.
func TestCleanupRunnerCloseWaits(t *testing.T) {
	p, in := cleanSettings()
	p.CleanupIntervalNs = 20_000_000
	f := joinedFake(p)
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	f.due = func(store.Request) store.Response {
		first.Do(func() {
			close(entered)
			select {
			case <-release:
			case <-time.After(lifecycleWait):
			}
		})
		return clPage(clT0, 0)
	}
	l := cleanLimiter(t, f, p, in, reasonReady)
	select {
	case <-entered:
	case <-time.After(lifecycleWait):
		t.Fatalf("no timed pass called due_entries within %v", lifecycleWait)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		l.Close()
	}()
	select {
	case <-closed:
		t.Errorf("Close returned while a timed pass was in a store call")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(lifecycleWait):
		t.Fatalf("Close did not return within %v of the call's release", lifecycleWait)
	}
	if got := f.callsAfterClose(); len(got) != 0 {
		t.Errorf("calls after the adapter closed: %q, want none (Close waits for the runner)", got)
	}
}

// TestCleanupStopMapping (plan S7 step 6, L1; DESIGN §A5.8; AGENTS.md §2 switches): the
// epoch-wide refusal codes map to their stops, 20 config_error, 21 clock_error and 22
// state_integrity_error (scope epoch), and any other code fails closed: outcome_unknown, a
// library defect reported in Diag with the reason and the raw reply. Each not-ready health
// reason maps to its stop (§A5.8): settings_invalid and fingerprint_mismatch config_error,
// closing unavailable, no_epoch, state_lost and integrity_hold an epoch-wide
// state_integrity_error; any other reason fails closed, reported in Diag.
func TestCleanupStopMapping(t *testing.T) {
	for _, c := range []struct {
		code   int64
		status passStatus
		diag   Diag
	}{
		{20, passConfigError, Diag{Reason: "why"}},
		{21, passClockError, Diag{Reason: "why"}},
		{22, passStateIntegrityError, Diag{Scope: scopeEpoch, Reason: "why"}},
		{0, passOutcomeUnknown, Diag{Reason: "why", StoreError: "raw"}},
		{23, passOutcomeUnknown, Diag{Reason: "why", StoreError: "raw"}},
		{25, passOutcomeUnknown, Diag{Reason: "why", StoreError: "raw"}},
		{30, passOutcomeUnknown, Diag{Reason: "why", StoreError: "raw"}},
	} {
		r := clOK(c.code, 0, "why")
		r.Raw = "raw"
		if status, d := refusalStop(r); status != c.status || d != c.diag {
			t.Errorf("refusalStop(code %d) = %v, %+v; want %v, %+v", c.code, status, d, c.status, c.diag)
		}
	}
	lost := Diag{Scope: scopeEpoch, Reason: "hwm_missing"}
	held := Diag{Scope: scopeEpoch, Reason: reasonIntegrityHold}
	for _, c := range []struct {
		health string
		diag   Diag // the readiness look's Diag
		status passStatus
		want   Diag
	}{
		{reasonSettingsInvalid, Diag{Reason: reasonSettingsInvalid}, passConfigError, Diag{Reason: reasonSettingsInvalid}},
		{reasonMismatch, Diag{Reason: reasonMismatch}, passConfigError, Diag{Reason: reasonMismatch}},
		{reasonClosing, Diag{Reason: reasonClosing}, passUnavailable, Diag{Reason: reasonClosing}},
		{reasonNoEpoch, Diag{Reason: reasonNoEpoch}, passStateIntegrityError, Diag{Scope: scopeEpoch, Reason: reasonNoEpoch}},
		{reasonStateLost, lost, passStateIntegrityError, lost},
		{reasonIntegrityHold, held, passStateIntegrityError, held},
		{reasonReady, Diag{}, passOutcomeUnknown, Diag{Reason: reasonReady}},
		{"another_reason", Diag{Scope: scopeEpoch, Reason: "x"}, passOutcomeUnknown, Diag{Reason: "another_reason"}},
	} {
		if status, d := notReadyStop(readiness{health: c.health, diag: c.diag}); status != c.status || d != c.want {
			t.Errorf("notReadyStop(%q) = %v, %+v; want %v, %+v", c.health, status, d, c.status, c.want)
		}
	}
}

// TestReclaimFilter (plan S7, L1; DESIGN §A4.7; SPEC ADM-1): the reclaimer runs after a
// complete pass: the namespace ID is the text between rl:e: and the next ':'; of 2,700 keys
// that 3 SCAN calls return, the 2,500 outside the joined epoch go to reclaim.lua in 3 batches
// of at most 1,000 (ReclaimBatch), its counts added to rl_reclaim_keys_total; a pass makes at
// most 10 SCAN calls and the next pass continues the cursor; after a full iteration (cursor 0)
// the next pass starts again at 0; a failed SCAN or reclaim ends reclaim for that pass, the
// pass still complete, and the cursor stays at the reply not yet reclaimed, a SCAN reply whose
// cursor is outside uint64 failing too; a stopped pass does not reclaim.
func TestReclaimFilter(t *testing.T) {
	t.Run("namespace", func(t *testing.T) {
		for _, c := range []struct {
			key, id string
			ok      bool
		}{
			{"rl:e:" + clEpoch + ":k:YQ", clEpoch, true}, {"rl:e:" + clOther + ":ix", clOther, true},
			{"rl:e::q", "", true}, {"rl:e:abc", "", false}, {"rl:epoch", "", false}, {"x:rl:e:a:b", "", false},
		} {
			if id, ok := namespaceID(c.key); id != c.id || ok != c.ok {
				t.Errorf("namespaceID(%q) = %q, %v; want %q, %v", c.key, id, ok, c.id, c.ok)
			}
		}
		got := reclaimCandidates([]string{"rl:e:" + clEpoch + ":ix", "rl:e:" + clOther + ":ix", "rl:e:abc", "rl:e::q", "rl:e:" + clEpoch + "x:q"}, clEpoch)
		if want := []string{"rl:e:" + clOther + ":ix", "rl:e::q", "rl:e:" + clEpoch + "x:q"}; !slices.Equal(got, want) {
			t.Errorf("candidates %q, want %q", got, want)
		}
	})

	t.Run("batches", func(t *testing.T) {
		p, in := cleanSettings()
		f := joinedFake(p)
		f.due = func(store.Request) store.Response { return clPage(clT0, 0) }
		keysOf := func(id string, from, n int) []string {
			out := make([]string, n)
			for i := range out {
				out[i] = fmt.Sprintf("rl:e:%s:k:K%04d", id, from+i)
			}
			return out
		}
		replies := map[string]resp.Value{
			"0":  scanValue("11", slices.Concat(keysOf(clOther, 0, 600), keysOf(clEpoch, 0, 100), keysOf(clOther, 600, 600))...),
			"11": scanValue("22", slices.Concat(keysOf(clEpoch, 100, 100), keysOf(clOther, 1200, 800))...),
			"22": scanValue("0", keysOf(clOther, 2000, 500)...),
		}
		f.scan = func(cursor string) (resp.Value, store.Response) {
			return replies[cursor], store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
		}
		f.reclaim = func(req store.Request) store.Response { return clOK(40, 0, "", clInt(int64(len(req.Keys)-1))) }
		l := cleanLimiter(t, f, p, in, reasonReady)
		checkReport(t, "pass", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
		var sizes []int
		for _, r := range f.requests("reclaim") {
			sizes = append(sizes, len(r.Keys)-1)
			if r.Keys[0] != "rl:epoch" || r.Kind != store.Maintenance {
				t.Errorf("reclaim KEYS[1] %q, kind %v; want rl:epoch, maintenance", r.Keys[0], r.Kind)
			}
			for _, k := range r.Keys[1:] {
				if ns, _ := namespaceID(k); ns != clOther {
					t.Errorf("reclaim candidate %q is not in the old namespace", k)
				}
			}
		}
		if !slices.Equal(sizes, []int{1000, 1000, 500}) {
			t.Errorf("reclaim batches %v, want 1,000, 1,000 and 500: 2,500 candidates in 3 batches", sizes)
		}
		if got := metricValue(t, l, "rl_reclaim_keys_total", ""); got != 2500 {
			t.Errorf("rl_reclaim_keys_total = %v, want 2500", got)
		}
		// The iteration ended (cursor 0): the next pass starts again at 0.
		checkReport(t, "second pass", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
		if want := []string{"0", "11", "22", "0", "11", "22"}; !slices.Equal(scanCursors(f.log()), want) {
			t.Errorf("SCAN cursors %q, want %q", scanCursors(f.log()), want)
		}
	})

	t.Run("at most 10 SCAN per pass, cursor kept", func(t *testing.T) {
		p, in := cleanSettings()
		f := joinedFake(p)
		f.due = func(store.Request) store.Response { return clPage(clT0, 0) }
		f.scan = func(cursor string) (resp.Value, store.Response) {
			n, _ := strconv.Atoi(cursor)
			return scanValue(strconv.Itoa(n + 1)), store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
		}
		l := cleanLimiter(t, f, p, in, reasonReady)
		for pass := range 2 {
			before := len(f.log())
			checkReport(t, "pass", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
			cursors := scanCursors(f.log()[before:])
			var want []string
			for i := range 10 {
				want = append(want, strconv.Itoa(10*pass+i))
			}
			if !slices.Equal(cursors, want) {
				t.Errorf("pass %d: SCAN cursors %q, want %q", pass+1, cursors, want)
			}
		}
		if n := len(f.requests("reclaim")); n != 0 {
			t.Errorf("%d reclaim calls with no candidates, want none", n)
		}
	})

	t.Run("a failed call ends reclaim", func(t *testing.T) {
		p, in := cleanSettings()
		f := joinedFake(p)
		f.due = func(store.Request) store.Response { return clPage(clT0, 0) }
		failScan, failReclaim := true, true
		f.scan = func(cursor string) (resp.Value, store.Response) {
			if cursor == "0" && failScan {
				return resp.Value{}, store.Response{Outcome: store.Unavailable}
			}
			return scanValue(map[string]string{"0": "5", "5": "9", "9": "0"}[cursor], "rl:e:"+clOther+":ix"), store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
		}
		f.reclaim = func(req store.Request) store.Response {
			if failReclaim {
				return store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: "ERR reclaim"}
			}
			return clOK(40, 0, "", clInt(int64(len(req.Keys)-1)))
		}
		l := cleanLimiter(t, f, p, in, reasonReady)
		checkReport(t, "SCAN fails", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
		f.set(func(*cleanFake) { failScan = false })
		checkReport(t, "reclaim fails", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
		if got := l.Health().StoreError; got != "ERR reclaim" {
			t.Errorf("Health.StoreError %q after the failed reclaim, want ERR reclaim", got)
		}
		f.set(func(*cleanFake) { failReclaim = false })
		checkReport(t, "reclaim succeeds", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
		// Pass 1: SCAN 0 fails. Pass 2: SCAN 0, 5 and 9, then the batch of their 3 candidates
		// fails, so the cursor stays 0. Pass 3: the same, and the batch is reclaimed.
		if want := []string{"0", "0", "5", "9", "0", "5", "9"}; !slices.Equal(scanCursors(f.log()), want) {
			t.Errorf("SCAN cursors %q, want %q", scanCursors(f.log()), want)
		}
		if n := len(f.requests("reclaim")); n != 2 {
			t.Errorf("%d reclaim calls, want 2", n)
		}
		if got := metricValue(t, l, "rl_reclaim_keys_total", ""); got != 3 {
			t.Errorf("rl_reclaim_keys_total = %v, want 3", got)
		}
	})

	t.Run("the cursor moves past a reply once its candidates are sent", func(t *testing.T) {
		p, in := cleanSettings()
		in.ReclaimBatch = 1
		f := joinedFake(p)
		f.due = func(store.Request) store.Response { return clPage(clT0, 0) }
		next := map[string]string{"0": "3", "3": "6", "6": "0"}
		f.scan = func(cursor string) (resp.Value, store.Response) {
			return scanValue(next[cursor], "rl:e:"+clOther+":k:C"+cursor), store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
		}
		failOnce := true
		f.reclaim = func(req store.Request) store.Response {
			if req.Keys[1] == "rl:e:"+clOther+":k:C3" && failOnce {
				failOnce = false
				return store.Response{Outcome: store.Unavailable}
			}
			return clOK(40, 0, "", clInt(1))
		}
		l := cleanLimiter(t, f, p, in, reasonReady)
		for range 2 {
			checkReport(t, "pass", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
		}
		// Pass 1: SCAN 0, its key reclaimed (cursor 3); SCAN 3, its key fails. Pass 2: SCAN 3
		// again, then 6, until the cursor is 0.
		if want := []string{"0", "3", "3", "6"}; !slices.Equal(scanCursors(f.log()), want) {
			t.Errorf("SCAN cursors %q, want %q", scanCursors(f.log()), want)
		}
		if got := metricValue(t, l, "rl_reclaim_keys_total", ""); got != 3 {
			t.Errorf("rl_reclaim_keys_total = %v, want 3", got)
		}
	})

	// ReclaimBatch 1,000: SCAN 0 gives 3 candidates, fewer than a batch, and cursor 5, and
	// SCAN 5 fails, so nothing is sent and the cursor stays 0; the next pass starts again at
	// SCAN 0, SCAN 5 ends the iteration, and the 3 candidates go out in one batch.
	t.Run("a failed SCAN keeps the cursor before unsent candidates", func(t *testing.T) {
		p, in := cleanSettings()
		f := joinedFake(p)
		f.due = func(store.Request) store.Response { return clPage(clT0, 0) }
		cands := []string{"rl:e:" + clOther + ":k:A", "rl:e:" + clOther + ":k:B", "rl:e:" + clOther + ":ix"}
		failOnce := true
		f.scan = func(cursor string) (resp.Value, store.Response) {
			ok := store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
			switch {
			case cursor == "0":
				return scanValue("5", cands...), ok
			case cursor == "5" && failOnce:
				failOnce = false
				return resp.Value{}, store.Response{Outcome: store.Unavailable}
			}
			return scanValue("0"), ok
		}
		f.reclaim = func(req store.Request) store.Response { return clOK(40, 0, "", clInt(int64(len(req.Keys)-1))) }
		l := cleanLimiter(t, f, p, in, reasonReady)
		if in.ReclaimBatch != 1000 {
			t.Fatalf("ReclaimBatch %d, want 1,000", in.ReclaimBatch)
		}
		for range 2 {
			checkReport(t, "pass", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
		}
		if want := []string{"0", "5", "0", "5"}; !slices.Equal(scanCursors(f.log()), want) {
			t.Errorf("SCAN cursors %q, want %q", scanCursors(f.log()), want)
		}
		if reqs := f.requests("reclaim"); len(reqs) != 1 || !slices.Equal(reqs[0].Keys[1:], cands) {
			t.Errorf("reclaim calls %v, want one with %q", reqs, cands)
		}
		if got := metricValue(t, l, "rl_reclaim_keys_total", ""); got != 3 {
			t.Errorf("rl_reclaim_keys_total = %v, want 3", got)
		}
	})

	// SCAN 0 answers cursor 2^64 − 1, kept (no candidates); SCAN 2^64 − 1 answers 2^64, outside
	// the cursor range, so that SCAN fails: reclaim ends for the pass, the kept cursor still
	// 2^64 − 1, and the next pass continues from it, never from 2^64 (which a store refuses).
	t.Run("a cursor outside uint64 ends reclaim", func(t *testing.T) {
		const maxU64 = "18446744073709551615"
		p, in := cleanSettings()
		f := joinedFake(p)
		f.due = func(store.Request) store.Response { return clPage(clT0, 0) }
		f.scan = func(cursor string) (resp.Value, store.Response) {
			ok := store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
			switch cursor {
			case "0":
				return scanValue(maxU64), ok
			case maxU64:
				return scanValue("18446744073709551616"), ok
			}
			return resp.Value{Kind: resp.Error, Str: "ERR invalid cursor"}, store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: "ERR invalid cursor"}
		}
		l := cleanLimiter(t, f, p, in, reasonReady)
		for range 2 {
			checkReport(t, "pass", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
		}
		if want := []string{"0", maxU64, maxU64}; !slices.Equal(scanCursors(f.log()), want) {
			t.Errorf("SCAN cursors %q, want %q", scanCursors(f.log()), want)
		}
	})

	t.Run("no reclaim after a stopped pass", func(t *testing.T) {
		p, in := cleanSettings()
		f := joinedFake(p)
		f.due = func(store.Request) store.Response { return store.Response{Outcome: store.Unavailable} }
		l := cleanLimiter(t, f, p, in, reasonReady)
		checkReport(t, "pass", l.RunCleanupPass(t.Context()), CleanupReport{Status: "unavailable"})
		if got := f.log(); !slices.Equal(got, []string{"due_entries cutoff=0"}) {
			t.Errorf("calls %q, want only the first due_entries", got)
		}
	})
}
