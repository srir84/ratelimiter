//go:build rltest

package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/store/resp"
)

// a511Family is one row of the DESIGN §A5.11 metrics table, written out with the label values
// IMPLEMENTATION_PLAN S9 step 2 registers.
type a511Family struct {
	name, typ, label string
	values           []string // nil for a family without a label
}

// a511Families is the §A5.11 table: 17 families. The label sets are the plan's (S9 step 2):
// acquire results 14, refund results 5, cleanup pass status 6 (complete and the 5 stop
// results), cleanup actions 4, cleanup stop results 5, scripts 7 (the rl_lib scripts), slot
// kinds 2, connection kinds 3.
func a511Families() []a511Family {
	stops := []string{"state_integrity_error", "config_error", "clock_error", "unavailable", "outcome_unknown"}
	return []a511Family{
		{"rl_slots_in_use", "gauge", "kind", []string{"receipt", "refund"}},
		{"rl_pending_refunds", "gauge", "", nil},
		{"rl_refund_results_total", "counter", "result", []string{"applied", "already_applied", "receipt_invalid", "receipt_expired", "unresolved"}},
		{"rl_cleanup_passes_total", "counter", "status", append([]string{"complete"}, stops...)},
		{"rl_cleanup_entries_total", "counter", "action", []string{"removed", "moved", "quarantined", "skipped"}},
		{"rl_cleanup_stops_total", "counter", "result", stops},
		{"rl_cleanup_lag_seconds", "gauge", "", nil},
		{"rl_clock_lag_microseconds", "gauge", "", nil},
		{"rl_listener_drops_total", "counter", "", nil},
		{"rl_integrity_hold", "gauge", "", nil},
		{"rl_reclaim_keys_total", "counter", "", nil},
		{"rl_acquire_results_total", "counter", "result", []string{
			"allowed", "degraded", "limited", "validation_error", "clock_error", "busy", "cancelled", "config_error",
			"not_ready", "state_integrity_error", "capacity_error", "unavailable", "outcome_unknown", "receipt_error",
		}},
		{"rl_store_call_seconds", "histogram", "script", []string{"acquire", "refund", "due_entries", "cleanup_entry", "reclaim", "read_epoch", "hold"}},
		{"rl_tracked_buckets", "gauge", "", nil},
		{"rl_live_markers", "gauge", "", nil},
		{"rl_quarantined_keys", "gauge", "", nil},
		{"rl_store_connections", "gauge", "kind", []string{"call", "refund", "maintenance"}},
	}
}

// callSecondsBounds are rl_store_call_seconds' bucket upper bounds in seconds: 1, 2, 5, 10,
// 20, 50, 100, 200, 500, 1,000 and 2,000 ms (plan S9 step 2, within §A5.11's "1 ms … 2 s").
func callSecondsBounds() []float64 {
	return []float64{0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2}
}

// seriesList names every series of a snapshot, in order: family, type, label and label value,
// and a histogram's bucket bounds.
func seriesList(s metrics.MetricsSnapshot) []string {
	var out []string
	for _, f := range s.Families {
		for _, se := range f.Series {
			id := fmt.Sprintf("%s %s %s=%q", f.Name, f.Type, f.LabelName, se.LabelValue)
			for _, b := range se.Buckets {
				id += " le=" + strconv.FormatFloat(b.UpperBound, 'g', -1, 64)
			}
			out = append(out, id)
		}
	}
	return out
}

// seriesOf returns the series of family name with label value label.
func seriesOf(t *testing.T, s metrics.MetricsSnapshot, name, label string) metrics.Series {
	t.Helper()
	for _, f := range s.Families {
		if f.Name != name {
			continue
		}
		for _, se := range f.Series {
			if se.LabelValue == label {
				return se
			}
		}
	}
	t.Fatalf("no series %s{%q}", name, label)
	return metrics.Series{}
}

// TestMetrics_SeriesConstant (DESIGN §A5.11; SPEC STA-14, API-1g; plan S9 Tests (a)): a limiter
// whose real adapter talks to a fakeresp store serves 10,000 distinct keys, allowed (then
// done) and limited in turn. The series list (family, type, label value, histogram buckets) is
// the same at construction and afterwards, so no key became a series; it is exactly the
// §A5.11 table with the plan's label sets and buckets, 55 series in all. The traffic did move
// the series: 5,000 allowed, 5,000 limited and 10,000 acquire calls observed.
func TestMetrics_SeriesConstant(t *testing.T) {
	ts := newTestStore(t)
	p := testPolicy(Closed)
	fp := fingerprint.Of(p)
	arr := func(vs ...resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: vs} }
	ts.srv.Load(scripts.ReadEpoch.Text)
	ts.srv.Handle(scripts.ReadEpoch.SHA, func(_, _ []string) resp.Value {
		return arr(intVal(0), intVal(0), strVal(""), strVal(epochA), strVal(fp), intVal(tNow), intVal(1),
			intVal(tNow), intVal(0), intVal(0), intVal(0))
	})
	var calls atomic.Int64
	ts.srv.Load(scripts.Acquire.Text)
	ts.srv.Handle(scripts.Acquire.SHA, func(_, _ []string) resp.Value {
		if calls.Add(1)%2 == 1 { // E_ALLOWED {1, t, '', u, g, s, t + lifetime}
			return arr(intVal(1), intVal(tNow), strVal(""), intVal(19_000_000_000), intVal(1), intVal(1), intVal(tNow+lifeUs))
		}
		return arr(intVal(2), intVal(tNow), strVal(""), intVal(0)) // E_LIMITED {2, t, '', u}
	})
	l, err := ts.limiter(t, p, testInstance(p), nil, Hooks{})
	if err != nil {
		t.Fatalf("NewWithHooks: %v", err)
	}
	built := l.Metrics()
	waitReason(t, l, reasonReady)

	const keys = 10_000
	for k := range keys {
		r := l.Acquire(t.Context(), "series-"+strconv.Itoa(k))
		switch {
		case k%2 == 0 && r.Kind == Allowed:
			r.Permit.Done()
		case k%2 == 1 && r.Kind == Limited:
		default:
			t.Fatalf("acquire %d: %v (%+v), want allowed and limited in turn", k, r.Kind, r.Diag)
		}
	}
	after := l.Metrics()

	before, now := seriesList(built), seriesList(after)
	if !slices.Equal(before, now) {
		t.Errorf("the series list changed after %d keys:\nat construction %q\nafterwards      %q", keys, before, now)
	}
	want := a511Families()
	if len(after.Families) != len(want) {
		t.Fatalf("%d families, want the %d of §A5.11", len(after.Families), len(want))
	}
	total := 0
	for i, w := range want {
		f := after.Families[i]
		values := make([]string, 0, len(f.Series))
		for _, se := range f.Series {
			values = append(values, se.LabelValue)
		}
		wantValues := w.values
		if wantValues == nil {
			wantValues = []string{""}
		}
		if f.Name != w.name || f.Type != w.typ || f.LabelName != w.label || !slices.Equal(values, wantValues) {
			t.Errorf("family %d: %s %s %s=%q, want %s %s %s=%q", i, f.Name, f.Type, f.LabelName, values, w.name, w.typ, w.label, wantValues)
		}
		total += len(f.Series)
		if w.typ != "histogram" {
			continue
		}
		for _, se := range f.Series {
			var bounds []float64
			for _, b := range se.Buckets {
				bounds = append(bounds, b.UpperBound)
			}
			if !slices.Equal(bounds, callSecondsBounds()) {
				t.Errorf("%s{%q} buckets %v s, want %v s", f.Name, se.LabelValue, bounds, callSecondsBounds())
			}
		}
	}
	if total != 55 {
		t.Errorf("%d series, want 55", total)
	}
	for _, c := range []struct {
		label string
		n     float64
	}{{"allowed", keys / 2}, {"limited", keys / 2}} {
		if v := seriesOf(t, after, "rl_acquire_results_total", c.label).Value; v != c.n {
			t.Errorf("rl_acquire_results_total{result=%q} = %v, want %v", c.label, v, c.n)
		}
	}
	if n := seriesOf(t, after, "rl_store_call_seconds", "acquire").Count; n != keys {
		t.Errorf("rl_store_call_seconds{script=\"acquire\"} count %d, want %d", n, keys)
	}
	for _, name := range seriesList(after) {
		if strings.Contains(name, "series-") {
			t.Errorf("a series names a key: %s", name)
		}
	}
}

// TestHealth_Fields (DESIGN §A5.11; SPEC API-1g; plan S9 Tests (a)): every Health field is set.
// A limiter built through NewWithHooks on a fake store, joined to epoch A, reports status and
// reason ready, epoch_id A, clock_source store, its per-instance settings, listener_drops 1 (of
// three refund events with a queue of 1 and the listener blocked in the first, the third is
// dropped), store_error the text of the last error reply (an acquire answered READONLY, which
// is outcome_unknown) and fixtures true. fixtures is true only through NewWithHooks: limiters
// built through New, valid or settings_invalid, report false, and a settings_invalid one built
// through NewWithHooks reports true.
func TestHealth_Fields(t *testing.T) {
	s := newTestStore(t)
	p := testPolicy(Closed)
	in := testInstance(p)
	in.ListenerQueueSize = 1
	fake := newFake(joined(p), func(store.Request) store.Response { return allowedResp(1, 1) })
	fake.setRefund(func(store.Request) store.Response { return okResp(3, tNow, "", int64(0)) }) // E_APPLIED
	unblock := make(chan struct{})
	var delivered atomic.Int32
	l := s.ready(t, p, in, fake, Hooks{}, reasonReady, WithRefundListener(func(RefundEvent) {
		delivered.Add(1)
		<-unblock
	}))
	t.Cleanup(func() { close(unblock) }) // runs before l.Close (cleanups run last in, first out)
	var permits []*Permit
	for k := range 3 {
		r := l.Acquire(t.Context(), "health-"+strconv.Itoa(k))
		if r.Kind != Allowed {
			t.Fatalf("acquire %d: %v (%+v), want allowed", k, r.Kind, r.Diag)
		}
		permits = append(permits, r.Permit)
	}
	permits[0].Release()
	waitFor(t, "the first refund event in the listener", func() bool { return delivered.Load() == 1 })
	permits[1].Release() // one of these two events waits in the queue, the other is dropped
	permits[2].Release()
	waitFor(t, "a listener drop", func() bool { return l.Health().ListenerDrops == 1 })
	const readOnly = "READONLY You can't write against a read only replica."
	fake.setAcquire(func(store.Request) store.Response {
		return store.Response{Outcome: store.OutcomeUnknown, Sent: true, SentCommands: 1, StoreError: readOnly}
	})
	if r := l.Acquire(t.Context(), "health-readonly"); r.Kind != OutcomeUnknown || r.Diag.StoreError != readOnly {
		t.Fatalf("acquire answered READONLY: %v (%+v), want outcome_unknown with the reply text", r.Kind, r.Diag)
	}

	h := l.Health()
	want := Health{
		Status: statusReady, Reason: reasonReady, EpochID: epochA, ClockSource: "store", Instance: in,
		ListenerDrops: 1, StoreError: readOnly, Fixtures: true,
	}
	if h != want {
		t.Errorf("Health() = %+v,\nwant %+v", h, want)
	}
	if h.Status != "ready" || h.Reason != "ready" { // the §A5.11 strings, written out
		t.Errorf("status %q, reason %q; want ready, ready", h.Status, h.Reason)
	}
	if d := seriesOf(t, l.Metrics(), "rl_listener_drops_total", "").Value; d != 1 {
		t.Errorf("rl_listener_drops_total %v, want 1, as Health.ListenerDrops", d)
	}

	// fixtures: New never sets it; NewWithHooks always does, also when the settings are invalid.
	prod, err := New(p, in, s.options())
	t.Cleanup(func() { prod.Close() })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h := prod.Health(); h.Fixtures || h.Instance != in || h.ClockSource != "store" {
		t.Errorf("a limiter built through New: fixtures %v, instance %+v, clock_source %q; want false, its settings, store", h.Fixtures, h.Instance, h.ClockSource)
	}
	bad := in
	bad.StoreConns = 0
	badProd, err := New(p, bad, s.options())
	t.Cleanup(func() { badProd.Close() })
	if err == nil {
		t.Fatal("New with store_conns 0 succeeded, want settings_invalid")
	}
	badHooks, err := NewWithHooks(p, bad, s.options(), Hooks{})
	t.Cleanup(func() { badHooks.Close() })
	if err == nil {
		t.Fatal("NewWithHooks with store_conns 0 succeeded, want settings_invalid")
	}
	for _, c := range []struct {
		name     string
		l        *Limiter
		fixtures bool
	}{{"New, settings_invalid", badProd, false}, {"NewWithHooks, settings_invalid", badHooks, true}} {
		if h := c.l.Health(); h.Fixtures != c.fixtures || h.Reason != reasonSettingsInvalid || h.Status != statusNotReady {
			t.Errorf("%s: fixtures %v, %s/%s; want fixtures %v, not_ready/settings_invalid", c.name, h.Fixtures, h.Status, h.Reason, c.fixtures)
		}
	}
}

// TestHealth_ReclaimScanError (plan S9 step 1: the last error reply text reaches
// Health.store_error; DESIGN §A4.7, §A5.11): a limiter whose real adapter talks to a fakeresp
// store (read_epoch joins epoch A; due_entries gives an empty page) runs a pass whose one SCAN
// the store answers with an error reply. Reclaim ends there and the pass stays complete with
// an empty Diag (plan S7 choice), and Health.store_error is the reply's text, as for every
// other store call. The re-read and cleanup intervals are 1 h, so no epoch read clears the
// text and no timed pass takes the error reply.
func TestHealth_ReclaimScanError(t *testing.T) {
	ts := newTestStore(t)
	p := testPolicy(Closed)
	p.CleanupIntervalNs = hourNs
	fp := fingerprint.Of(p)
	arr := func(vs ...resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: vs} }
	ts.srv.Load(scripts.ReadEpoch.Text)
	ts.srv.Handle(scripts.ReadEpoch.SHA, func(_, _ []string) resp.Value {
		return arr(intVal(0), intVal(0), strVal(""), strVal(epochA), strVal(fp), intVal(tNow), intVal(1),
			intVal(tNow), intVal(0), intVal(0), intVal(0))
	})
	ts.srv.Load(scripts.DueEntries.Text)
	ts.srv.Handle(scripts.DueEntries.SHA, func(_, _ []string) resp.Value { // {0, T0, '', 0}: an empty page
		return arr(intVal(0), intVal(tNow), strVal(""), intVal(0))
	})
	const noPerm = "NOPERM User rl_lib has no permissions to run the 'scan' command"
	ts.srv.ReplyError("SCAN", noPerm, 1)
	l := ts.ready(t, p, testInstance(p), nil, Hooks{}, reasonReady)
	if h := l.Health(); h.StoreError != "" {
		t.Fatalf("Health.StoreError %q before the pass, want empty", h.StoreError)
	}
	checkReport(t, "a pass whose SCAN gets an error reply", l.RunCleanupPass(t.Context()), CleanupReport{Status: "complete"})
	if n := ts.srv.Count("SCAN"); n != 1 {
		t.Errorf("%d SCAN calls, want 1: reclaim ends at the error reply", n)
	}
	if h := l.Health(); h.StoreError != noPerm {
		t.Errorf("Health.StoreError %q after the pass, want the SCAN error reply %q", h.StoreError, noPerm)
	}
}

// serveEpochA makes ts's fake store answer read_epoch with epoch A under p's fingerprint, so a
// limiter whose real adapter talks to it joins A.
func serveEpochA(ts *testStore, p Policy) {
	fp := fingerprint.Of(p)
	arr := func(vs ...resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: vs} }
	ts.srv.Load(scripts.ReadEpoch.Text)
	ts.srv.Handle(scripts.ReadEpoch.SHA, func(_, _ []string) resp.Value {
		return arr(intVal(0), intVal(0), strVal(""), strVal(epochA), strVal(fp), intVal(tNow), intVal(1),
			intVal(tNow), intVal(0), intVal(0), intVal(0))
	})
}

// waitWarm waits until every connection of l is ready (rl_store_connections): in.StoreConns
// call, in.RefundConns refund and 1 maintenance, so no dial is left in progress.
func waitWarm(t *testing.T, l *Limiter, in Instance) {
	t.Helper()
	waitFor(t, "every connection ready", func() bool {
		s := l.Metrics()
		return seriesOf(t, s, "rl_store_connections", "call").Value == float64(in.StoreConns) &&
			seriesOf(t, s, "rl_store_connections", "refund").Value == float64(in.RefundConns) &&
			seriesOf(t, s, "rl_store_connections", "maintenance").Value == 1
	})
}

// wrongPass is the store's refusal of an AUTH with a wrong password, as Valkey and fakeresp
// send it.
const wrongPass = "WRONGPASS invalid username-password pair or user is disabled."

// TestHealth_AuthRefusedText (plan S9 step 1: the last error reply text reaches Diag.StoreError
// and Health.store_error, also when it refused a dial; DESIGN §A5.4, §A5.11): a limiter whose
// real adapter talks to a fakeresp store joins epoch A, every connection ready; then the store
// answers every AUTH with WRONGPASS and drops the connections. Once each of the 11 slots'
// redials was refused (a slot redials 25–50 ms after its loss and next 50 ms or more after a
// refused dial, so the first 11 AUTH after the drop are one per slot), an acquire is
// unavailable (closed mode) or degraded (open mode) with Diag.StoreError the refusal's text, and
// Health.store_error is that text, the instance still ready. A limiter given a wrong password
// never joins (no_epoch), and its Health.store_error is the store's WRONGPASS text, not only the
// outcome's name.
func TestHealth_AuthRefusedText(t *testing.T) {
	for _, c := range []struct {
		name string
		mode FailureMode
		want ResultKind
	}{{"closed", Closed, Unavailable}, {"open", Open, Degraded}} {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestStore(t)
			p := testPolicy(c.mode)
			in := testInstance(p)
			serveEpochA(ts, p)
			l := ts.ready(t, p, in, nil, Hooks{}, reasonReady)
			waitWarm(t, l, in) // no first dial still in progress takes a refusal
			before := ts.srv.Count("AUTH")
			ts.srv.ReplyError("AUTH", wrongPass, 1000) // every AUTH for the test's length
			ts.srv.DropConns()
			slots := in.StoreConns + in.RefundConns + 1
			waitFor(t, "every slot's redial refused", func() bool { return ts.srv.Count("AUTH") >= before+slots })
			r := l.Acquire(t.Context(), "auth-refused")
			if r.Permit != nil {
				r.Permit.Done()
			}
			if r.Kind != c.want || r.Diag.StoreError != wrongPass {
				t.Errorf("acquire once the redials are refused: %v (%+v), want %v with Diag.StoreError %q", r.Kind, r.Diag, c.want, wrongPass)
			}
			if h := l.Health(); h.StoreError != wrongPass || h.Status != statusReady {
				t.Errorf("Health: store_error %q, status %q; want %q, %s", h.StoreError, h.Status, wrongPass, statusReady)
			}
		})
	}
	t.Run("wrong password", func(t *testing.T) {
		ts := newTestStore(t)
		p := testPolicy(Closed)
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		st := ts.options()
		st.Password = hex.EncodeToString(b) // not the store's
		l, err := NewWithHooks(p, testInstance(p), st, Hooks{})
		if l == nil {
			t.Fatal("NewWithHooks returned a nil limiter")
		}
		t.Cleanup(func() { l.Close() })
		if err != nil {
			t.Fatalf("NewWithHooks: %v", err)
		}
		waitFor(t, "a health store error", func() bool { return l.Health().StoreError != "" })
		if h := l.Health(); h.StoreError != wrongPass || h.Reason != reasonNoEpoch {
			t.Errorf("Health: store_error %q, reason %q; want %q, %s", h.StoreError, h.Reason, wrongPass, reasonNoEpoch)
		}
	})
}
