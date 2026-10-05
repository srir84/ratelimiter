//go:build conformance

package conformance

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ratelimiter/internal/testutil/storeproc"
)

func TestC2_Row08_AOFMultiExec(t *testing.T) {
	s := start(t, storeproc.Durable)
	lib := conn(t, s, "rl_lib")
	key := "rl:conf:row08"
	// A fresh AOF writes SELECT 0 before its first command, inside that command's
	// MULTI; one earlier write makes the probe's entry exactly MULTI, HSET, HSET, EXEC.
	do(t, lib, evalsha(load(t, lib, "probe_write"), []string{"rl:conf:row08:first"}, "f", "1")...)
	do(t, lib, evalsha(load(t, lib, "probe_row08_two_hset"), []string{key})...)

	// appendfsync always: the AOF holds the effects before the reply.
	files, err := filepath.Glob(filepath.Join(s.Dir, "appendonlydir", "*.incr.aof"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no appendonlydir/*.incr.aof in %s (%v)", s.Dir, err)
	}
	var cmds [][]string
	for _, f := range files {
		cmds = append(cmds, aofCommands(t, f)...)
	}
	want := [][]string{{"MULTI"}, {"HSET", key, "a", "1"}, {"HSET", key, "b", "2"}, {"EXEC"}}
	for i := 0; i+len(want) <= len(cmds); i++ {
		if sameCommands(cmds[i:i+len(want)], want) {
			return
		}
	}
	t.Fatalf("AOF commands %q: want MULTI, HSET, HSET, EXEC for the probe", cmds)
}

// aofCommands reads an AOF file: a sequence of commands written as RESP arrays.
func aofCommands(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() // read-only
	r := bufio.NewReader(f)
	var cmds [][]string
	for {
		rep, err := storeproc.ReadReply(r)
		if errors.Is(err, io.EOF) {
			return cmds
		}
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if rep.Kind != '*' {
			t.Fatalf("%s: entry %s is not a command", path, rep)
		}
		cmd := make([]string, len(rep.Arr))
		for i, a := range rep.Arr {
			cmd[i] = a.Str
		}
		cmds = append(cmds, cmd)
	}
}

// sameCommands compares commands; command names ignore case.
func sameCommands(got, want [][]string) bool {
	for i := range want {
		if len(got[i]) != len(want[i]) || !strings.EqualFold(got[i][0], want[i][0]) {
			return false
		}
		for j := 1; j < len(want[i]); j++ {
			if got[i][j] != want[i][j] {
				return false
			}
		}
	}
	return true
}

// crashRound is one SIGKILL round of row 15, as .data/conformance.json records it.
type crashRound struct {
	Acked       int `json:"acked"`        // acknowledged writes
	Lost        int `json:"lost"`         // acknowledged writes missing after the restart
	InFlight    int `json:"in_flight"`    // writes sent without a reply when the store died
	KeptUnacked int `json:"kept_unacked"` // of those, present after the restart
}

// writerLog is one crash-test writer's history: seqs 1…acked were acknowledged, and
// inflight (0 for none) was sent without a reply.
type writerLog struct {
	key      string
	acked    int
	inflight int
	err      error
}

func TestC2_Row15_Crash(t *testing.T) {
	const writers = 32
	s := start(t, storeproc.Durable)
	var rounds []crashRound
	var earlier []writerLog
	for round := 1; round <= 3; round++ {
		sha := load(t, conn(t, s, "rl_lib"), "probe_write")
		clients := conns(t, s, "rl_lib", writers)
		logs := make([]writerLog, writers)
		var wg sync.WaitGroup
		for w := range logs {
			logs[w].key = fmt.Sprintf("rl:conf:crash:%d:%d", round, w)
			wg.Add(1)
			go func() {
				defer wg.Done()
				write(clients[w], sha, &logs[w])
			}()
		}
		time.Sleep(2 * time.Second)
		killed := s.PID
		s.Kill()
		wg.Wait()
		if _, err := os.Stat(s.RecordPath(killed)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("round %d: killed store %d still has a .out/procs record (%v)", round, killed, err)
		}
		for _, l := range logs {
			if l.err != nil {
				t.Fatalf("round %d: %s: %v", round, l.key, l.err)
			}
		}
		if err := s.Restart(true); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if _, err := os.Stat(s.RecordPath(s.PID)); err != nil {
			t.Fatalf("round %d: restarted store %d has no .out/procs record: %v", round, s.PID, err)
		}

		reader := conn(t, s, "rl_lib")
		hgetall := load(t, reader, "probe_hgetall")
		var r crashRound
		for _, l := range logs {
			lost, kept := checkWriter(t, reader, hgetall, l)
			r.Acked += l.acked
			r.Lost += lost
			if l.inflight > 0 {
				r.InFlight++
				r.KeptUnacked += kept
			}
		}
		for _, l := range earlier {
			if lost, _ := checkWriter(t, reader, hgetall, l); lost > 0 {
				t.Errorf("round %d: %d acknowledged writes of an earlier round to %s are gone", round, lost, l.key)
			}
		}
		t.Logf("round %d: %d acknowledged, %d lost, %d in flight, %d of them kept", round, r.Acked, r.Lost, r.InFlight, r.KeptUnacked)
		if r.Acked == 0 {
			t.Fatalf("round %d: no write was acknowledged in 2 s", round)
		}
		rounds = append(rounds, r)
		earlier = append(earlier, logs...)
	}
	record("crash", rounds)
	for i, r := range rounds {
		if r.Lost > 0 {
			t.Errorf("round %d: %d acknowledged writes lost after SIGKILL (DESIGN §A3.0 row 15, STA-16)", i+1, r.Lost)
		}
	}
}

// write sends probe_write with seqs 1, 2, … to l.key until the connection fails.
func write(c *storeproc.Client, sha string, l *writerLog) {
	for seq := 1; ; seq++ {
		l.inflight = seq
		rep, err := c.Do(evalsha(sha, []string{l.key}, "s:"+strconv.Itoa(seq), strconv.Itoa(seq))...)
		if err != nil {
			return // the store is gone; seq stays in flight
		}
		if rep.Kind != ':' || rep.Int != int64(seq) {
			l.err = fmt.Errorf("seq %d: reply %s, want :%d", seq, rep, seq)
			return
		}
		l.acked, l.inflight = seq, 0
	}
}

// checkWriter reads a writer's hash back: lost counts acknowledged seqs that are missing;
// kept is 1 when its in-flight seq is present. A script's writes come back all or none
// (row 8): the write counter n equals the number of seq fields.
func checkWriter(t *testing.T, c *storeproc.Client, hgetall string, l writerLog) (lost, kept int) {
	t.Helper()
	rep := do(t, c, evalsha(hgetall, []string{l.key})...)
	if rep.Kind != '*' || len(rep.Arr)%2 != 0 {
		t.Fatalf("HGETALL %s: reply %s", l.key, rep)
	}
	fields := make(map[string]string, len(rep.Arr)/2)
	for i := 0; i < len(rep.Arr); i += 2 {
		fields[rep.Arr[i].Str] = rep.Arr[i+1].Str
	}
	for seq := 1; seq <= l.acked; seq++ {
		if _, ok := fields["s:"+strconv.Itoa(seq)]; !ok {
			lost++
		}
	}
	if _, ok := fields["s:"+strconv.Itoa(l.inflight)]; l.inflight > 0 && ok {
		kept = 1
	}
	seqs := len(fields)
	if _, ok := fields["n"]; ok {
		seqs--
	}
	if n := fields["n"]; seqs > 0 && n != strconv.Itoa(seqs) {
		t.Errorf("%s: write counter %q but %d seq fields: a script's writes came back partly", l.key, n, seqs)
	}
	return lost, kept
}

func TestC2_Row16_Loading(t *testing.T) {
	ms, saw := loadingRound(t, 1)
	record("reload_ms_a7", ms)
	if saw {
		return
	}
	t.Logf("no LOADING reply while the §A7-sized dataset reloaded (%.1f ms); retrying with 4× (plan choice)", ms)
	if ms4, saw4 := loadingRound(t, 4); !saw4 {
		t.Fatalf("no LOADING reply while the 4× dataset reloaded (%.1f ms)", ms4)
	}
}

// loadingRound loads scale × the §A7-sized dataset into a fresh durable store as
// rl_fixture, shuts the store down (SHUTDOWN as rl_ops), restarts it without waiting
// and polls EVALSHA probe_write every 1 ms. Every reply before the first other one must
// be LOADING, and that one NOSCRIPT. It returns the reload time, from the restart to the
// first reply that is not LOADING, and whether any LOADING came.
func loadingRound(t *testing.T, scale int) (reloadMs float64, sawLoading bool) {
	t.Helper()
	s := start(t, storeproc.Durable)
	defer s.Stop()
	sha := load(t, conn(t, s, "rl_lib"), "probe_write")
	pipelined(t, conns(t, s, "rl_fixture", 16), a7Dataset(scale))
	if rep, err := conn(t, s, "rl_ops").Do("SHUTDOWN"); err == nil {
		t.Fatalf("SHUTDOWN replied %s, want the connection closed as the store exits", rep)
	}

	t0 := time.Now()
	if err := s.Restart(false); err != nil {
		t.Fatal(err)
	}
	lib := conn(t, s, "rl_lib")
	key := "rl:conf:row16:loading"
	loading := 0
	for {
		rep, err := lib.Do(evalsha(sha, []string{key}, "f", "1")...)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Kind == '-' && strings.HasPrefix(rep.Str, "LOADING") {
			loading++
			if time.Since(t0) > 2*time.Minute {
				t.Fatal("still LOADING after 2 minutes")
			}
			time.Sleep(time.Millisecond)
			continue
		}
		reloadMs = round1(float64(time.Since(t0).Microseconds()) / 1e3)
		if rep.Kind != '-' || !strings.HasPrefix(rep.Str, "NOSCRIPT") {
			t.Fatalf("first reply after %d LOADING: %s, want NOSCRIPT (the script cache starts empty)", loading, rep)
		}
		break
	}
	if got := keyType(t, conn(t, s, "rl_fixture"), key); got != "none" {
		t.Fatalf("the probe's key is %q after the reload, want none (refused before it ran)", got)
	}
	want := 150_000*scale + 1
	if got := keyspaceKeys(t, conn(t, s, "rl_ops")); got != want {
		t.Fatalf("%d keys after the restart, want %d (the dataset)", got, want)
	}
	t.Logf("scale %d: %d LOADING replies; reload %.1f ms", scale, loading, reloadMs)
	return reloadMs, loading > 0
}

// a7Dataset is scale × the DESIGN §A7 sizing as rl_fixture commands: 100,000 bucket
// hashes rl:e:<id>:k:<enc>, 50,000 marker fields with their mi:<enc> members, and
// 100,000 ix members (500 per ZADD), in 150,000 × scale + 1 keys.
func a7Dataset(scale int) [][]string {
	n := 100_000 * scale
	ns := "rl:e:" + nsID + ":"
	cmds := make([][]string, 0, n+n/2+n/500+1)
	var members []string
	for i := range n {
		enc := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "user-%09d", i))
		h := []string{"HSET", ns + "k:" + enc,
			"u", "1000000000", "t", "1760000000000000", "g", "1", "s", strconv.Itoa(i), "x", "1760000060000000", "mc", "0"}
		if i%2 == 0 {
			h[len(h)-1] = "1"
			h = append(h, "m:1:"+strconv.Itoa(i), "1760000060000000:1760000000000000")
			cmds = append(cmds, []string{"ZADD", ns + "mi:" + enc, "1760000060000000", "1:" + strconv.Itoa(i)})
		}
		cmds = append(cmds, h)
		members = append(members, "1760000002000000", enc)
		if len(members) == 1000 || i == n-1 {
			cmds = append(cmds, append([]string{"ZADD", ns + "ix"}, members...))
			members = members[:0]
		}
	}
	return cmds
}

// keyspaceKeys reads db0's key count from INFO keyspace; ops is an rl_ops connection.
func keyspaceKeys(t *testing.T, ops *storeproc.Client) int {
	t.Helper()
	db0 := info(t, ops, "keyspace")["db0"]
	for _, kv := range strings.Split(db0, ",") {
		if v, ok := strings.CutPrefix(kv, "keys="); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("INFO keyspace db0 %q", db0)
			}
			return n
		}
	}
	return 0
}

func TestC2_FsyncFloor(t *testing.T) {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	// The run disk (plan choice): .data/tmp/, removed again when the test ends.
	base := filepath.Join(root, ".data", "tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, fmt.Sprintf("fsync-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
		_ = os.Remove(base) // only when empty: another run may be using it
	})

	// On darwin, (*os.File).Sync is fcntl(F_FULLFSYNC) (Go's internal/poll), the call
	// Valkey's valkey_fsync makes for appendfsync always (DESIGN §A3.0).
	floor := syncP50(t, filepath.Join(dir, "fullfsync.bin"), func(f *os.File) error { return f.Sync() })
	plain := syncP50(t, filepath.Join(dir, "fsync.bin"), func(f *os.File) error { return syscall.Fsync(int(f.Fd())) })

	s := startWith(t, storeproc.Options{Profile: storeproc.Durable, Dir: filepath.Join(dir, "store")})
	lib := conn(t, s, "rl_lib")
	sha := load(t, lib, "probe_write")
	cmd := evalsha(sha, []string{"rl:conf:fsync"}, "f", "1")
	for range 20 { // warm-up
		do(t, lib, cmd...)
	}
	lat := make([]time.Duration, 0, 500)
	for range 500 {
		t0 := time.Now()
		do(t, lib, cmd...)
		lat = append(lat, time.Since(t0))
	}
	script := percentile(lat, 50)
	record("fsync", map[string]float64{
		"f_fullfsync_floor_p50_us": micros(floor),
		"plain_fsync_p50_us":       micros(plain),
		"durable_script_p50_us":    micros(script),
	})
	t.Logf("F_FULLFSYNC floor p50 %v, plain fsync p50 %v, durable single-client probe_write p50 %v (ratio %.2f)",
		floor, plain, script, float64(script)/float64(floor))
	if 2*script < floor {
		t.Fatalf("durable script p50 %v < 0.5 × the F_FULLFSYNC floor %v: the store's sync looks like plain fsync (DESIGN §C2)", script, floor)
	}
}

// syncP50 returns the p50 of 200 × (append 128 B, then sync) on a new file at path.
func syncP50(t *testing.T, path string, sync func(*os.File) error) time.Duration {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() // the data is synced already, or the test failed
	buf := make([]byte, 128)
	lat := make([]time.Duration, 0, 200)
	for i := range 205 {
		t0 := time.Now()
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
		if err := sync(f); err != nil {
			t.Fatal(err)
		}
		if i >= 5 { // the first syncs also write the new file's metadata
			lat = append(lat, time.Since(t0))
		}
	}
	return percentile(lat, 50)
}
