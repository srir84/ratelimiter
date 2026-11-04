//go:build rltest

package spectest

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/driver"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/testutil/storestate"
	"github.com/srir84/ratelimiter/limiter"
)

// admTraffic runs T-ADM-1's traffic on its own goroutine: acquires one at a time across the
// three instances on ks, the controlled clock 150 ms further every 5th op, every 7th op that is
// allowed released (its refund applied before the next op, so the op log stays sequential) and
// the others finished with done(), until at least 60 ops ran and the admin loop counted 12
// rladmin runs (runs), within 30 s. It closes the returned channel when it stops; failures are
// reported with t.Errorf.
func admTraffic(t *testing.T, d *driver.Driver, ks []string, runs *atomic.Int64, wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		deadline := time.Now().Add(30 * time.Second)
		for n := 0; n < 60 || runs.Load() < 12; n++ {
			if !time.Now().Before(deadline) {
				t.Errorf("traffic: %d ops and %d rladmin runs after 30 s, want 60 and 12", n, runs.Load())
				return
			}
			if n%5 == 4 {
				if err := d.Clock().Advance(150_000_000); err != nil {
					t.Errorf("traffic: %v", err)
					return
				}
			}
			op := d.Acquire(n%3, ks[n%len(ks)])
			switch {
			case op.Kind == limiter.Allowed && n%7 == 0:
				d.Release(op)
				if ev, err := d.WaitRefund(op, waitMax); err != nil || ev.Result != limiter.Applied {
					t.Errorf("traffic: release of op %d: %+v, %v; want applied", n, ev, err)
					return
				}
			case op.Kind == limiter.Allowed:
				d.Done(op)
			case op.Kind != limiter.Limited:
				t.Errorf("traffic: op %d on %q: %v %+v, want allowed or limited", n, op.Key, op.Kind, op.Diag)
				return
			}
		}
	}()
	return done
}

// storeInt parses a stored integer field, failing the test on anything else.
func storeInt(t *testing.T, what, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("%s: stored %q: %v", what, s, err)
	}
	return n
}

// matchesStore checks rladmin inspect --json output r for key against the store snapshot: each
// bucket field, the index score, the marker expiries (the scores of mi:<enc>) and no quarantine.
func matchesStore(t *testing.T, snap storestate.State, id, key string, r driver.AdminResult) {
	t.Helper()
	enc := b64(key)
	k := snap[keys.BucketKey(id, enc)]
	for out, field := range map[string]string{"u": "u", "t_us": "t", "g": "g", "s": "s", "x_us": "x", "mc": "mc"} {
		if v, ok := r.Int(out); !ok || v != storeInt(t, key+" "+field, k.Fields[field]) {
			t.Errorf("%s: inspect %s = %v, store %s = %q", key, out, r.JSON[out], field, k.Fields[field])
		}
	}
	if v, ok := r.Int("ix_score"); !ok || v != storeInt(t, key+" ix", snap[keys.IndexKey(id)].Fields[enc]) {
		t.Errorf("%s: inspect ix_score %v, store %q", key, r.JSON["ix_score"], snap[keys.IndexKey(id)].Fields[enc])
	}
	var want []int64
	for _, score := range snap[keys.MarkerIndexKey(id, enc)].Fields {
		want = append(want, storeInt(t, key+" mi", score))
	}
	slices.Sort(want)
	var got []int64
	list, _ := r.JSON["marker_expiries"].([]any)
	for _, e := range list {
		n, ok := e.(json.Number)
		v, err := n.Int64()
		if !ok || err != nil {
			t.Fatalf("%s: marker_expiries %v", key, r.JSON["marker_expiries"])
		}
		got = append(got, v)
	}
	slices.Sort(got)
	if present, _ := r.Int("present"); present != 1 || !slices.Equal(got, want) || r.Str("quarantine") != "" {
		t.Errorf("%s: inspect present %d, marker expiries %v, quarantine %v; store: a bucket, %v, none", key, present, got, r.JSON["quarantine"], want)
	}
}

// TestT_ADM_1 (SPEC T-ADM-1; ADM-4, STA-13): rladmin status and inspect run in a loop while three
// instances serve (admTraffic): every run exits 0. Then, with the clock frozen: inspect of each
// key (LogInspect, whose snapshots go into the op log) shows exactly what the store holds, and so
// does status; the op log, snapshots included, matches the reference model; status and inspect
// run twice more give the same output and leave the store unchanged; no output of any run holds
// one of the store's RL_*_PASSWORD values.
func TestT_ADM_1(t *testing.T) {
	d := fastDriver(t, 3, nil)
	ks := []string{"adm-a", "adm-b", "adm-c", "adm-d"}
	var runs atomic.Int64
	var wg sync.WaitGroup
	done := admTraffic(t, d, ks, &runs, &wg)
	var outputs []string
	keep := func(r driver.AdminResult) driver.AdminResult {
		outputs = append(outputs, r.Stdout+r.Stderr)
		return r
	}
loop:
	for n := 0; ; n++ {
		select {
		case <-done:
			break loop
		default:
		}
		for _, args := range [][]string{{"status"}, {"inspect", "--key", ks[n%len(ks)]}} {
			adminCode(t, "during traffic", keep(d.RLAdmin(args...)), 0)
			runs.Add(1)
		}
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	snap := d.Dump()
	id := snap[keys.EpochKey].Fields["id"]
	for _, k := range ks {
		matchesStore(t, snap, id, k, keep(d.LogInspect(k)))
	}
	st := keep(d.RLAdmin("status"))
	rec := snap[keys.EpochKey].Fields
	if hwm, _ := st.Int("hwm_us"); st.Code != 0 || st.Str("epoch_id") != rec["id"] || st.Str("fingerprint") != rec["fp"] || st.Str("hold") != rec["hold"] ||
		hwm != storeInt(t, "hwm", rec["hwm"]) || !intIs(st, "tracked_buckets", storeInt(t, "nb", rec["nb"])) ||
		!intIs(st, "live_markers", storeInt(t, "nm", rec["nm"])) || !intIs(st, "quarantined_keys", 0) {
		t.Errorf("status %s, store record %v", st.Stdout, rec)
	}
	d.CheckRefModel(t)

	first := map[string]string{}
	for round := range 2 {
		for _, args := range [][]string{{"status"}, {"inspect", "--key", ks[0]}, {"inspect", "--key", ks[1]}, {"inspect", "--key", ks[2]}, {"inspect", "--key", ks[3]}} {
			r := keep(d.RLAdmin(args...))
			name := strings.Join(args, " ")
			if round == 0 {
				first[name] = r.Stdout
			} else if r.Stdout != first[name] {
				t.Errorf("%s again: %q, want %q", name, r.Stdout, first[name])
			}
		}
	}
	if now := d.Dump(); !reflect.DeepEqual(now, snap) {
		t.Error("the store changed during the repeated status and inspect runs")
	}
	pw := d.Store().PW
	for _, out := range outputs {
		for _, secret := range []string{pw.Lib, pw.Admin, pw.Fixture, pw.Ops} {
			if strings.Contains(out, secret) {
				t.Fatal("an rladmin output holds an RL_*_PASSWORD value")
			}
		}
	}
	if len(outputs) < 12 {
		t.Errorf("%d rladmin runs, want at least 12", len(outputs))
	}
}

// intIs reports whether r's integer field name is want.
func intIs(r driver.AdminResult, name string, want int64) bool {
	v, ok := r.Int(name)
	return ok && v == want
}
