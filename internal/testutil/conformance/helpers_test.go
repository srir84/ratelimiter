//go:build conformance

package conformance

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ratelimiter/internal/testutil/storeproc"
)

// nsID is the epoch namespace of the rl:e:<id>:* keys the probes and fixtures use: any
// 32 hex digits. Every key matches ~rl:*, or ACL refusals would hide the behavior.
const nsID = "0123456789abcdef0123456789abcdef"

// start starts the test's own store with profile p (a fresh store per test, §C2).
func start(t *testing.T, p storeproc.Profile) *storeproc.Store {
	t.Helper()
	return startWith(t, storeproc.Options{Profile: p})
}

// startWith starts the test's own store with o. Every TestC2_* test starts its store
// through it, which counts the test as started: TestMain rewrites .data/conformance.json
// only after an unfiltered run in which at least one did.
func startWith(t *testing.T, o storeproc.Options) *storeproc.Store {
	t.Helper()
	measurements.started++
	return storeproc.Start(t, o)
}

// conn opens a connection as user, closed when the test ends.
func conn(t *testing.T, s *storeproc.Store, user string) *storeproc.Client {
	t.Helper()
	c, err := s.Conn(user)
	if err != nil {
		t.Fatalf("connecting as %s: %v", user, err)
	}
	t.Cleanup(func() { _ = c.Close() }) // the store may already be gone
	return c
}

// conns opens n connections as user.
func conns(t *testing.T, s *storeproc.Store, user string, n int) []*storeproc.Client {
	t.Helper()
	cs := make([]*storeproc.Client, n)
	for i := range cs {
		cs[i] = conn(t, s, user)
	}
	return cs
}

// probe returns the source of testdata/<name>.lua.
func probe(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", name+".lua"))
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

// load loads a probe with SCRIPT LOAD and returns its SHA.
func load(t *testing.T, c *storeproc.Client, name string) string {
	t.Helper()
	rep := do(t, c, "SCRIPT", "LOAD", probe(t, name))
	if rep.Kind != '$' || len(rep.Str) != 40 {
		t.Fatalf("SCRIPT LOAD %s: reply %s, want a 40-hex SHA", name, rep)
	}
	return rep.Str
}

// evalsha builds EVALSHA sha <len(keys)> keys... args....
func evalsha(sha string, keys []string, args ...string) []string {
	cmd := make([]string, 0, 3+len(keys)+len(args))
	cmd = append(cmd, "EVALSHA", sha, strconv.Itoa(len(keys)))
	cmd = append(cmd, keys...)
	return append(cmd, args...)
}

// do runs one command; an I/O error or an error reply fails the test.
func do(t *testing.T, c *storeproc.Client, args ...string) storeproc.Reply {
	t.Helper()
	rep, err := c.Do(args...)
	if err != nil {
		t.Fatalf("%s: %v", args[0], err)
	}
	if rep.Kind == '-' {
		t.Fatalf("%s: error reply %s", args[0], rep)
	}
	return rep
}

// refused runs one command that must get an error reply starting with prefix.
func refused(t *testing.T, c *storeproc.Client, prefix string, args ...string) storeproc.Reply {
	t.Helper()
	rep, err := c.Do(args...)
	if err != nil {
		t.Fatalf("%s: %v", args[0], err)
	}
	if rep.Kind != '-' || !strings.HasPrefix(rep.Str, prefix) {
		t.Fatalf("%s: reply %s, want an error starting %q", args[0], rep, prefix)
	}
	return rep
}

// integer returns an integer reply's value; any other reply fails the test.
func integer(t *testing.T, what string, rep storeproc.Reply) int64 {
	t.Helper()
	if rep.Kind != ':' {
		t.Fatalf("%s: reply %s, want an integer", what, rep)
	}
	return rep.Int
}

// info reads one INFO section; ops is an rl_ops connection.
func info(t *testing.T, ops *storeproc.Client, section string) map[string]string {
	t.Helper()
	rep := do(t, ops, "INFO", section)
	if rep.Kind != '$' || rep.Nil {
		t.Fatalf("INFO %s: reply %s", section, rep)
	}
	return storeproc.ParseInfo(rep.Str)
}

// configGet reads one setting; ops is an rl_ops connection.
func configGet(t *testing.T, ops *storeproc.Client, name string) string {
	t.Helper()
	rep := do(t, ops, "CONFIG", "GET", name)
	if rep.Kind != '*' || len(rep.Arr) != 2 || rep.Arr[0].Str != name {
		t.Fatalf("CONFIG GET %s: reply %s", name, rep)
	}
	return rep.Arr[1].Str
}

// configSet changes one setting; ops is an rl_ops connection.
func configSet(t *testing.T, ops *storeproc.Client, name, value string) {
	t.Helper()
	if rep := do(t, ops, "CONFIG", "SET", name, value); rep.Kind != '+' {
		t.Fatalf("CONFIG SET %s %s: reply %s", name, value, rep)
	}
}

// keyType returns TYPE key ("none" when absent); fx is an rl_fixture connection.
func keyType(t *testing.T, fx *storeproc.Client, key string) string {
	t.Helper()
	rep := do(t, fx, "TYPE", key)
	if rep.Kind != '+' {
		t.Fatalf("TYPE %s: reply %s", key, rep)
	}
	return rep.Str
}

// rewriteAOF rewrites the AOF (BGREWRITEAOF as rl_ops) and waits until no rewrite is in
// progress or scheduled, so a measurement starts from a small AOF with no rewrite under
// way (DESIGN §A2: capacity benchmarks rewrite between runs).
func rewriteAOF(t *testing.T, ops *storeproc.Client) {
	t.Helper()
	do(t, ops, "BGREWRITEAOF")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		p := info(t, ops, "persistence")
		if p["aof_rewrite_in_progress"] == "0" && p["aof_rewrite_scheduled"] == "0" {
			if p["aof_last_bgrewrite_status"] != "ok" {
				t.Fatalf("AOF rewrite status %q", p["aof_last_bgrewrite_status"])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("AOF rewrite still running after 2 minutes")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pipelined sends cmds over the clients in parallel, each client a contiguous share in
// pipelines of 256, and fails the test on an I/O error or any error reply.
func pipelined(t *testing.T, clients []*storeproc.Client, cmds [][]string) {
	t.Helper()
	errs := make([]error, len(clients))
	var wg sync.WaitGroup
	share := (len(cmds) + len(clients) - 1) / len(clients)
	for i, c := range clients {
		lo, hi := min(i*share, len(cmds)), min((i+1)*share, len(cmds))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := lo; b < hi; b += 256 {
				reps, err := c.Pipeline(cmds[b:min(b+256, hi)])
				if err != nil {
					errs[i] = err
					return
				}
				for j, rep := range reps {
					if rep.Kind == '-' {
						errs[i] = fmt.Errorf("%s: error reply %s", cmds[b+j][0], rep)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
	}
}

// percentile returns the p-th percentile (nearest rank) of samples; 0 for none.
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	rank := max(int(math.Ceil(p/100*float64(len(s)))), 1)
	return s[rank-1]
}

// micros is d in microseconds, rounded to 0.1 µs.
func micros(d time.Duration) float64 {
	return round1(float64(d.Nanoseconds()) / 1e3)
}

func round1(x float64) float64 {
	return math.Round(x*10) / 10
}

// loadStats is one measured load: throughput and latency over the measured window.
type loadStats struct {
	OpsPerS float64 `json:"ops_per_s"`
	P50us   float64 `json:"p50_us"`
	P99us   float64 `json:"p99_us"`
	Ops     int     `json:"ops"`
}

// pool runs one goroutine per client, each sending its batch with Pipeline until halt.
// It counts the commands whose batch completed inside [from, to), each with its batch's
// round trip as its latency.
type pool struct {
	stop    chan struct{}
	wg      sync.WaitGroup
	results []poolResult // results[i] is written only by worker i, read after wg.Wait
}

type poolResult struct {
	ops int
	lat []time.Duration
	err error
}

func startPool(clients []*storeproc.Client, batches [][][]string, from, to time.Time) *pool {
	p := &pool{stop: make(chan struct{}), results: make([]poolResult, len(clients))}
	for i, c := range clients {
		p.wg.Add(1)
		go p.run(&p.results[i], c, batches[i], from, to)
	}
	return p
}

func (p *pool) run(r *poolResult, c *storeproc.Client, batch [][]string, from, to time.Time) {
	defer p.wg.Done()
	for {
		select {
		case <-p.stop:
			return
		default:
		}
		t0 := time.Now()
		reps, err := c.Pipeline(batch)
		t1 := time.Now()
		if err != nil {
			r.err = err
			return
		}
		for _, rep := range reps {
			if rep.Kind == '-' {
				r.err = fmt.Errorf("%s: error reply %s", batch[0][0], rep)
				return
			}
		}
		if !t1.Before(from) && t1.Before(to) {
			r.ops += len(batch)
			for range batch {
				r.lat = append(r.lat, t1.Sub(t0))
			}
		}
	}
}

// halt stops the workers, waits for them and merges their results.
func (p *pool) halt() (ops int, lat []time.Duration, err error) {
	close(p.stop)
	p.wg.Wait()
	for _, r := range p.results {
		ops += r.ops
		lat = append(lat, r.lat...)
		if err == nil {
			err = r.err
		}
	}
	return ops, lat, err
}

// measure runs batches[i] on clients[i] for warm, then for window, and returns the
// window's throughput and latency.
func measure(t *testing.T, clients []*storeproc.Client, batches [][][]string, warm, window time.Duration) loadStats {
	t.Helper()
	from := time.Now().Add(warm)
	to := from.Add(window)
	p := startPool(clients, batches, from, to)
	time.Sleep(time.Until(to))
	ops, lat, err := p.halt()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return loadStats{
		OpsPerS: round1(float64(ops) / window.Seconds()),
		P50us:   micros(percentile(lat, 50)),
		P99us:   micros(percentile(lat, 99)),
		Ops:     ops,
	}
}
