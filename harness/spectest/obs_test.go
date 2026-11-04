//go:build rltest

package spectest

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/limiter"
)

// a511Families are the metric families of DESIGN §A5.11's table, which carry SPEC API-1g's
// minimum set (transcribed from the table, not read from the code).
func a511Families() []string {
	return []string{
		"rl_slots_in_use", "rl_pending_refunds", "rl_refund_results_total", "rl_cleanup_passes_total",
		"rl_cleanup_entries_total", "rl_cleanup_stops_total", "rl_cleanup_lag_seconds",
		"rl_clock_lag_microseconds", "rl_listener_drops_total", "rl_integrity_hold",
		"rl_reclaim_keys_total", "rl_acquire_results_total", "rl_store_call_seconds",
		"rl_tracked_buckets", "rl_live_markers", "rl_quarantined_keys", "rl_store_connections",
	}
}

// seriesCount is the number of series in s, over all its families.
func seriesCount(s limiter.MetricsSnapshot) int {
	n := 0
	for _, f := range s.Families {
		n += len(f.Series)
	}
	return n
}

// Plan choices of T-OBS-1's load.
const (
	obsKeys    = 10_000 // distinct keys
	obsWorkers = 6      // load goroutines
	obsPause   = time.Millisecond
)

// TestT_OBS_1 (SPEC T-OBS-1; STA-13, STA-14, API-1g; plan S9 Tests (b)): 3 instances, D = 500 ms,
// a sentinel key acquired once. 100 distinct keys are acquired over the instances, and each
// instance's series count is taken. Then worker goroutines acquire the other 9,900 keys over the
// 3 instances, and once each key has had its first acquire they go on acquiring keys at random
// (a fixed seed), one every millisecond each, until the test is done: the load. With all 10,000
// keys in the store and the load running, the test makes 1,000 Health() and 1,000 Metrics()
// calls over the instances and 50 rladmin runs, status and inspect of the sentinel in turn,
// each exiting 0. Afterwards: every key's first acquire was allowed (later ones allowed or
// limited) and load acquires ran during the calls; the sentinel's inspect is unchanged (no
// allowance spent, no state changed); every §A5.11 metric family (API-1g's set) is present on
// each instance; each instance's series count at 10,000 keys, and after the calls, equals its
// count at 100; the status counts the 10,000 keys and the sentinel.
func TestT_OBS_1(t *testing.T) {
	d := s9Driver(t, 3, limiter.Closed, nil)
	const sentinel = "obs-sentinel"
	d.Done(allowed(t, d, 0, sentinel))
	before := d.Inspect(sentinel)
	key := func(n int) string { return "obs-" + strconv.Itoa(n) }
	for n := range 100 {
		d.Done(allowed(t, d, n%3, key(n)))
	}
	at100 := make([]int, 3)
	for i := range at100 {
		at100[i] = seriesCount(d.Limiter(i).Metrics())
	}

	var next atomic.Int64 // the next key without its first acquire
	next.Store(100)
	var acquires, firstDone, bad atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range obsWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(1, uint64(w)))
			for {
				if n := int(next.Add(1) - 1); n < obsKeys {
					op := d.Acquire(n%3, key(n))
					if op.Kind != limiter.Allowed {
						bad.Add(1)
					}
					d.Done(op)
					firstDone.Add(1)
					acquires.Add(1)
					continue
				}
				select {
				case <-stop:
					return
				case <-time.After(obsPause):
				}
				n := rng.IntN(obsKeys)
				op := d.Acquire(n%3, key(n))
				if op.Kind != limiter.Allowed && op.Kind != limiter.Limited {
					bad.Add(1)
				}
				d.Done(op)
				acquires.Add(1)
			}
		}()
	}
	// stopLoad stops the load and waits for its workers, once. Registered as a cleanup as soon as
	// they start, it also runs when the test fails before either call below (a FailNow, such as
	// RLAdmin's), and it runs before the driver's teardown, which s9Driver registered earlier.
	stopLoad := sync.OnceFunc(func() {
		close(stop)
		wg.Wait()
	})
	t.Cleanup(stopLoad)
	if !waitUntil(time.Now().Add(waitMax), func() bool { return firstDone.Load() == obsKeys-100 }) {
		stopLoad()
		t.Fatalf("%d of %d keys had their first acquire within %v", firstDone.Load()+100, obsKeys, waitMax)
	}
	at10k := make([]int, 3)
	for i := range at10k {
		at10k[i] = seriesCount(d.Limiter(i).Metrics())
	}
	c0 := acquires.Load()
	for k := range 50 { // 20 Health() and 20 Metrics() calls before each rladmin run
		for j := range 20 {
			l := d.Limiter((20*k + j) % 3)
			_ = l.Health()
			_ = l.Metrics()
		}
		args := []string{"status"}
		if k%2 == 1 {
			args = []string{"inspect", "--key", sentinel}
		}
		adminCode(t, "during the load", d.RLAdmin(args...), 0)
	}
	c1 := acquires.Load()
	stopLoad()

	if n := bad.Load(); n != 0 {
		t.Errorf("%d load acquires were neither allowed (a key's first) nor allowed or limited (later)", n)
	}
	t.Logf("load acquires: %d before the calls (10,000 keys), %d during them", c0, c1-c0)
	if c1 == c0 {
		t.Error("no load acquire ran during the Health, Metrics and rladmin calls")
	}
	if after := d.Inspect(sentinel); !sameBucket(after, before) || after.IxScore != before.IxScore {
		t.Errorf("the sentinel after the Health, Metrics and rladmin calls: %+v, want unchanged %+v", after, before)
	}
	for i := range 3 {
		s := d.Limiter(i).Metrics()
		var names []string
		for _, f := range s.Families {
			names = append(names, f.Name)
		}
		for _, want := range a511Families() {
			if !slices.Contains(names, want) {
				t.Errorf("instance %d has no metric family %s", i, want)
			}
		}
		if at10k[i] != at100[i] || seriesCount(s) != at100[i] {
			t.Errorf("instance %d: %d series at %d keys and %d after the calls, want %d as at 100 keys", i, at10k[i], obsKeys, seriesCount(s), at100[i])
		}
	}
	if st := d.Status(); st.NB != obsKeys+1 {
		t.Errorf("status: %d tracked buckets, want %d (the keys and the sentinel)", st.NB, obsKeys+1)
	}
}
