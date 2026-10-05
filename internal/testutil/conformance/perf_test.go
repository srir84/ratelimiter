//go:build conformance

package conformance

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/testutil/storeproc"
)

const (
	warmUp = 500 * time.Millisecond
	window = 3 * time.Second
)

// noAutoRewrite turns automatic AOF rewrites off for a measurement, as for capacity
// benchmarks (DESIGN §A2); the tests rewrite between cases instead (rewriteAOF).
func noAutoRewrite(t *testing.T, ops *storeproc.Client) {
	t.Helper()
	configSet(t, ops, "auto-aof-rewrite-percentage", "0")
}

// row9Case is one row 9 case as .data/conformance.json records it.
type row9Case struct {
	Clients     int    `json:"clients"`
	Appendfsync string `json:"appendfsync"`
	loadStats
}

func TestC2_Row09_Throughput(t *testing.T) {
	s := start(t, storeproc.Durable)
	ops := conn(t, s, "rl_ops")
	noAutoRewrite(t, ops)
	clients := conns(t, s, "rl_lib", 600)
	sha := load(t, clients[0], "probe_write")
	batches := make([][][]string, len(clients))
	for i := range batches {
		batches[i] = [][]string{evalsha(sha, []string{"rl:conf:row09:" + strconv.Itoa(i)}, "f", "1")}
	}
	var cases []row9Case
	for _, c := range []struct {
		clients int
		fsync   string
	}{{50, "always"}, {600, "always"}, {50, "everysec"}} {
		configSet(t, ops, "appendfsync", c.fsync)
		rewriteAOF(t, ops)
		st := measure(t, clients[:c.clients], batches[:c.clients], warmUp, window)
		t.Logf("%d clients, appendfsync %s: %.0f ops/s, p50 %.0f µs, p99 %.0f µs", c.clients, c.fsync, st.OpsPerS, st.P50us, st.P99us)
		cases = append(cases, row9Case{Clients: c.clients, Appendfsync: c.fsync, loadStats: st})
	}
	record("row9", cases)
}

// row13Cell is one cell of the row 13 curve as .data/conformance.json records it. A
// maximum-size, depth-64 cell, the kind recommended_store_conns is computed from, is
// measured row13Samples times: Samples holds every run in order, and the cell's own
// figures are those of the median run (by ops/s).
type row13Cell struct {
	Conns    int    `json:"conns"`
	Depth    int    `json:"depth"`
	Size     string `json:"size"` // "small" (≈ 80 B) or "max" (acquire's shape, 256-byte key)
	CmdBytes int    `json:"cmd_bytes"`
	loadStats
	Samples []loadStats `json:"samples,omitempty"`
}

// row13Samples is how often each maximum-size, depth-64 cell is measured, so that one
// slow-fsync window cannot decide recommended_store_conns (plan rule, on the medians).
const row13Samples = 3

func TestC2_Row13_Curve(t *testing.T) {
	s := start(t, storeproc.Durable)
	ops := conn(t, s, "rl_ops")
	noAutoRewrite(t, ops)
	clients := conns(t, s, "rl_lib", 16)
	small, maxShape := load(t, clients[0], "probe_write"), load(t, clients[0], "probe_acquire_shape")
	ns := "rl:e:" + nsID + ":"
	command := func(size string, w int) []string {
		if size == "small" {
			return evalsha(small, []string{"rl:conf:c:" + strconv.Itoa(w)}, "f", "1")
		}
		// A 256-byte key encodes to the longest enc, 342 characters (DESIGN §A3.2).
		enc := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte('A' + w)}, 256))
		return evalsha(maxShape,
			[]string{"rl:epoch", "rl:ctl:time", ns + "ix", ns + "q", ns + "k:" + enc, ns + "mi:" + enc},
			nsID, "fedcba9876543210fedcba9876543210", enc,
			"1000000000", "1000000000", "100", "60000000", "100000", "1760000000000000", "1")
	}
	// run measures one cell: n connections, each sending depth copies of its command per
	// round trip, after an AOF rewrite.
	run := func(size string, n, depth int) loadStats {
		batches := make([][][]string, n)
		for w := range batches {
			cmd := command(size, w)
			batches[w] = make([][]string, depth)
			for i := range batches[w] {
				batches[w][i] = cmd
			}
		}
		rewriteAOF(t, ops)
		st := measure(t, clients[:n], batches, warmUp, window)
		t.Logf("%s (%d B) × %d connections × depth %d: %.0f ops/s, p50 %.0f µs, p99 %.0f µs",
			size, respBytes(batches[0][0]), n, depth, st.OpsPerS, st.P50us, st.P99us)
		return st
	}
	var cells []row13Cell
	for _, size := range [...]string{"small", "max"} {
		for _, n := range [...]int{2, 4, 8, 16} {
			for _, depth := range [...]int{1, 16, 64} {
				st := run(size, n, depth)
				cell := row13Cell{Conns: n, Depth: depth, Size: size, CmdBytes: respBytes(command(size, 0)), loadStats: st}
				if size == "max" && depth == 64 {
					cell.Samples = []loadStats{st}
				}
				cells = append(cells, cell)
			}
		}
	}
	// The further runs of the maximum-size, depth-64 cells go in rounds over the
	// connection counts, so a cell's runs lie at least three cells (over 10 s) apart and
	// one slow-sync window of a few seconds reaches at most one of them.
	for range row13Samples - 1 {
		for i := range cells {
			if c := &cells[i]; c.Samples != nil {
				c.Samples = append(c.Samples, run(c.Size, c.Conns, c.Depth))
			}
		}
	}
	rec := settleRow13(cells)
	for _, c := range cells {
		if c.Samples != nil {
			t.Logf("max × %d connections × depth 64: %s ops/s; median %.0f", c.Conns, opsList(c.Samples), c.OpsPerS)
		}
	}
	record("row13", cells)
	record("recommended_store_conns", rec)
	t.Logf("recommended_store_conns = %d (the smallest whose median maximum-size depth-64 throughput is ≥ 90 %% of the best median)", rec)
}

// settleRow13 gives each repeated cell the figures of its median run (by ops/s) and
// returns recommended_store_conns: the plan's rule (recommendedConns) on those medians.
func settleRow13(cells []row13Cell) int {
	for i := range cells {
		if c := &cells[i]; len(c.Samples) > 0 {
			c.loadStats = medianRun(c.Samples)
		}
	}
	return recommendedConns(cells)
}

// TestRow13Medians checks the rule on medians without a store: one run of the
// 16-connection cell slowed by a slow-sync window must not turn recommended_store_conns
// from 16 into 8, which that run alone would give.
func TestRow13Medians(t *testing.T) {
	cell := func(n int, runs ...float64) row13Cell {
		c := row13Cell{Conns: n, Depth: 64, Size: "max"}
		for _, r := range runs {
			c.Samples = append(c.Samples, loadStats{OpsPerS: r})
		}
		c.loadStats = c.Samples[0]
		return c
	}
	cells := []row13Cell{
		{Conns: 2, Depth: 64, Size: "small", loadStats: loadStats{OpsPerS: 32000}}, // not maximum-size
		{Conns: 16, Depth: 16, Size: "max", loadStats: loadStats{OpsPerS: 25000}},  // not depth 64
		cell(2, 3093, 3150, 3010),
		cell(4, 5717, 5650, 5790),
		cell(8, 10581, 10700, 10490),
		cell(16, 11000, 19115, 19480), // the first run met a slow-sync window
	}
	if rec := recommendedConns(cells); rec != 8 {
		t.Fatalf("on the first runs alone recommended_store_conns = %d, want 8", rec)
	}
	if rec := settleRow13(cells); rec != 16 {
		t.Fatalf("on the medians recommended_store_conns = %d, want 16", rec)
	}
	for i, want := range map[int]float64{2: 3093, 3: 5717, 4: 10581, 5: 19115, 0: 32000, 1: 25000} {
		if got := cells[i].OpsPerS; got != want {
			t.Errorf("cell %d (%s, %d connections, depth %d): %.0f ops/s, want %.0f", i, cells[i].Size, cells[i].Conns, cells[i].Depth, got, want)
		}
	}
}

// medianRun returns the run with the median throughput among an odd number of runs.
func medianRun(runs []loadStats) loadStats {
	s := slices.Clone(runs)
	slices.SortFunc(s, func(a, b loadStats) int { return cmp.Compare(a.OpsPerS, b.OpsPerS) })
	return s[len(s)/2]
}

// opsList formats the runs' throughputs, in run order, as "a / b / c".
func opsList(runs []loadStats) string {
	parts := make([]string, len(runs))
	for i, r := range runs {
		parts[i] = strconv.FormatFloat(r.OpsPerS, 'f', 0, 64)
	}
	return strings.Join(parts, " / ")
}

// recommendedConns is the smallest connection count whose maximum-size, depth-64
// throughput is at least 90 % of the best such cell (plan choice). The test gives each
// such cell its median run's throughput.
func recommendedConns(cells []row13Cell) int {
	best := 0.0
	for _, c := range cells {
		if c.Size == "max" && c.Depth == 64 {
			best = max(best, c.OpsPerS)
		}
	}
	rec := 0
	for _, c := range cells {
		if c.Size == "max" && c.Depth == 64 && c.OpsPerS >= 0.9*best && (rec == 0 || c.Conns < rec) {
			rec = c.Conns
		}
	}
	return rec
}

// respBytes is the size of cmd as a RESP array of bulk strings.
func respBytes(cmd []string) int {
	n := len("*\r\n") + len(strconv.Itoa(len(cmd)))
	for _, a := range cmd {
		n += len("$\r\n\r\n") + len(strconv.Itoa(len(a))) + len(a)
	}
	return n
}

func TestC2_Row14_NoWrites(t *testing.T) {
	s := start(t, storeproc.Durable)
	ops := conn(t, s, "rl_ops")
	noAutoRewrite(t, ops)
	reader := conn(t, s, "rl_lib")
	read, write := load(t, reader, "probe_read"), load(t, reader, "probe_write")
	readCmd := evalsha(read, []string{"rl:conf:row14:read"}, "f")
	p50 := func() time.Duration {
		lat := make([]time.Duration, 0, 500)
		for range 500 {
			t0 := time.Now()
			do(t, reader, readCmd...)
			lat = append(lat, time.Since(t0))
		}
		return percentile(lat, 50)
	}
	for range 20 { // warm-up
		do(t, reader, readCmd...)
	}
	idle := p50()

	writers := conns(t, s, "rl_lib", 16)
	batches := make([][][]string, len(writers))
	for i := range batches {
		batches[i] = [][]string{evalsha(write, []string{"rl:conf:row14:w" + strconv.Itoa(i)}, "f", "1")}
	}
	p := startPool(writers, batches, time.Time{}, time.Time{})
	halted := false
	defer func() {
		if !halted {
			_, _, _ = p.halt() // the test failed already; only the goroutines must end
		}
	}()
	time.Sleep(warmUp)
	loaded := p50()
	halted = true
	if _, _, err := p.halt(); err != nil {
		t.Fatalf("writers: %v", err)
	}
	record("row14", map[string]any{"idle_p50_us": micros(idle), "loaded_p50_us": micros(loaded), "writers": len(writers)})
	t.Logf("probe_read p50: idle %v; with %d writers %v", idle, len(writers), loaded)
}

func TestC2_ScanUnlink200k(t *testing.T) {
	const n = 200_000
	s := start(t, storeproc.Fast)
	prefix := "rl:e:" + nsID + ":k:"
	keys := make([]string, n)
	set := make([][]string, n)
	for i := range keys {
		keys[i] = prefix + strconv.Itoa(i)
		set[i] = []string{"SET", keys[i], "1"}
	}
	pipelined(t, conns(t, s, "rl_fixture", 4), set)

	lib := conn(t, s, "rl_lib")
	found, calls := scanAll(t, lib)
	record("scan_calls_200k", calls)
	t.Logf("SCAN MATCH rl:e:* COUNT 1000 over %d keys: %d calls", n, calls)
	if len(found) != n {
		t.Fatalf("SCAN found %d keys, want %d", len(found), n)
	}
	for _, k := range keys {
		if !found[k] {
			t.Fatalf("SCAN missed %s", k)
		}
	}

	unlink := load(t, lib, "probe_unlink")
	removed := int64(0)
	for b := 0; b < n; b += 1000 {
		removed += integer(t, "probe_unlink", do(t, lib, evalsha(unlink, keys[b:b+1000])...))
	}
	if removed != n {
		t.Fatalf("probe_unlink removed %d keys, want %d", removed, n)
	}
	if left, _ := scanAll(t, lib); len(left) != 0 {
		t.Fatalf("SCAN after UNLINK found %d keys, want 0", len(left))
	}
}

// scanAll runs SCAN <cursor> MATCH rl:e:* COUNT 1000 until the cursor returns to 0 and
// returns the distinct keys found and the number of calls.
func scanAll(t *testing.T, lib *storeproc.Client) (map[string]bool, int) {
	t.Helper()
	keys := map[string]bool{}
	cursor := "0"
	for calls := 1; ; calls++ {
		rep := do(t, lib, "SCAN", cursor, "MATCH", "rl:e:*", "COUNT", "1000")
		if rep.Kind != '*' || len(rep.Arr) != 2 || rep.Arr[1].Kind != '*' {
			t.Fatalf("SCAN reply %s", rep)
		}
		for _, k := range rep.Arr[1].Arr {
			keys[k.Str] = true
		}
		if cursor = rep.Arr[0].Str; cursor == "0" {
			return keys, calls
		}
		if calls > 1_000_000 {
			t.Fatal("SCAN did not return to cursor 0")
		}
	}
}
