//go:build rltest

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/dbclient"
	"ratelimiter/harness/pgtest"
	"ratelimiter/harness/runlog"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/storeproc"
)

// The orchestrator runs these tests make (IMPLEMENTATION_PLAN H3; plan choices for the bounds).
const (
	// orchWait bounds one bin/test/orchestrator run of an e2e scenario: a 10 s schedule, the
	// stack's start (the store, PostgreSQL, the epoch, at most 30 s for the instances to be
	// ready) and its graceful stop (the instances drain at once on loopback).
	orchWait = 3 * time.Minute
	// orchStopWait bounds the orchestrator's own teardown after the SIGTERM a test sends it once
	// orchWait has passed; past it the test kills it, and checkChildrenGone kills what it left.
	orchStopWait = 2 * time.Minute
	// orchStorePort is the run's store port (DESIGN §B1).
	orchStorePort = 6390
	// minSamples is the fewest samples.jsonl lines a run of a 10 s schedule writes: one a second
	// from before hitgen starts until the instances have stopped (plan H3 Tests).
	minSamples = 8
	// orchPIDFile is the running orchestrator's pid file, relative to the repository root (plan
	// H3 step 4).
	orchPIDFile = ".data/orchestrator.pid"
)

// The hits row each finished-run test puts in before its run (plan choice): a key outside
// every scenario's users (u%06d), so no run writes it and only the run's own TRUNCATE hits
// (HARNESS §4 step 1; plan H3 step 3) removes it, whatever earlier tests left in the table.
const (
	sentinelUser    = "e2e-sentinel"
	sentinelRequest = "before-the-run"
)

// aclUser is one user line of a run's rendered store/acl.conf: `user <name> on ><password> …`.
var aclUser = regexp.MustCompile(`^user (\S+) on >(\S+) `)

// runIDForm is a run ID, YYYYMMDD-HHMMSS-<name>-<seed> (plan H3 step 2): one path element, so
// a run directory's cleanup removes nothing but runs/<id>.
var runIDForm = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[^/]+-[0-9]+$`)

// orchRun is one bin/test/orchestrator run a test started in the repository root, where it
// finds harness.toml, bin/, deploy/, runs/ and .data/ (the harness PostgreSQL on 5433 and the
// latest conformance result). Its cleanup removes what the run left (plan choice): runs/<id>/,
// its runs/index.jsonl line (and the file, when the run created it), and runs/ when the run
// created it and nothing else is in it; a test that checks the index line does so before.
type orchRun struct {
	root  string
	code  int              // the orchestrator's exit code
	out   []byte           // its stdout and stderr: JSON log lines
	recs  []map[string]any // out's JSON log records, in order
	runID string           // the run ID its "run directory ready" line names; "" when it refused in preflight
	dir   string           // <root>/runs/<runID>; "" without a run ID

	indexPath    string
	indexBefore  []byte // runs/index.jsonl as it was before the run
	indexExisted bool
	runsExisted  bool
}

// runOrchestrator runs bin/test/orchestrator with args in the repository root, with PATH and
// HOME as its whole environment (each child's environment starts from it), its output saved in
// <t.TempDir()>/orchestrator.out and recorded in .out/procs/<pid> while it runs. It waits up to
// orchWait for the orchestrator to exit; past that it sends SIGTERM (the orchestrator stops its
// children in order), waits up to orchStopWait, then kills it, and fails the test. Once it has
// exited, and before any check of its output, it deals with what the run left: it takes the
// run ID from the "run directory ready" line, skipping lines that are not JSON (a crashed
// orchestrator's Go panic trace), and every process the run's pids.json lists must be gone
// (checkChildrenGone kills any left); when the orchestrator did not exit cleanly (stopped by
// the test, ended by a signal, or printing lines that are not JSON), a .data/orchestrator.pid
// naming it goes (removeStalePIDFile). Output lines that are not JSON objects then fail the
// test: the orchestrator logs only through its slog JSON logger.
func runOrchestrator(t *testing.T, args ...string) *orchRun {
	t.Helper()
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin", "test", "orchestrator")
	if err := checkBinary(bin); err != nil {
		t.Fatal(err)
	}
	r := &orchRun{root: root, indexPath: filepath.Join(root, "runs", "index.jsonl")}
	switch _, err := os.Stat(filepath.Join(root, "runs")); {
	case err == nil:
		r.runsExisted = true
	case !errors.Is(err, fs.ErrNotExist):
		t.Fatal(err)
	}
	switch data, err := os.ReadFile(r.indexPath); {
	case err == nil:
		r.indexBefore, r.indexExisted = data, true
	case !errors.Is(err, fs.ErrNotExist):
		t.Fatal(err)
	}
	t.Cleanup(func() { r.cleanup(t) })

	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	c, err := startProcessIn(t, root, t.TempDir(), root, bin, args, env, "orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	exited := c.exitedWithin(orchWait)
	if !exited {
		c.signal(t, syscall.SIGTERM)
		if !c.exitedWithin(orchStopWait) {
			c.kill(t)
		}
	}
	c.checkRecord(t)
	out, readErr := os.ReadFile(c.Out)
	r.out = out
	var notJSON []string
	r.recs, notJSON = splitLog(out)
	id := runIDOf(r.recs)
	if runIDForm.MatchString(id) {
		r.runID, r.dir = id, filepath.Join(root, "runs", id)
		r.checkChildrenGone(t)
	}
	code, codeErr := exitCode(c.waitErr)
	if !exited || codeErr != nil || readErr != nil || len(notJSON) > 0 {
		r.removeStalePIDFile(t, c.PID)
	}

	if readErr != nil {
		t.Fatal(readErr)
	}
	if id != r.runID {
		t.Errorf("the orchestrator's \"run directory ready\" line names run_id %q, want a run ID, YYYYMMDD-HHMMSS-<name>-<seed>", id)
	}
	if len(notJSON) > 0 {
		t.Errorf("the orchestrator printed %d lines that are not JSON objects, the first %q, want only its JSON log records; its output:\n%s",
			len(notJSON), notJSON[0], r.out)
	}
	if !exited {
		t.Fatalf("the orchestrator %s did not exit within %v; its output:\n%s", strings.Join(args, " "), orchWait, r.out)
	}
	if codeErr != nil {
		t.Fatalf("the orchestrator %s: %v; its output:\n%s", strings.Join(args, " "), codeErr, r.out)
	}
	r.code = code
	return r
}

// splitLog splits the orchestrator's output into its JSON log records, in order (it logs only
// through its slog JSON logger), and the lines that are not JSON objects, such as a Go panic's
// trace.
func splitLog(out []byte) (recs []map[string]any, notJSON []string) {
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec == nil {
			notJSON = append(notJSON, line)
			continue
		}
		recs = append(recs, rec)
	}
	return recs, notJSON
}

// runIDOf returns the run_id of the first "run directory ready" record, "" when there is none.
func runIDOf(recs []map[string]any) string {
	for _, rec := range recs {
		if rec["msg"] == "run directory ready" {
			id, _ := rec["run_id"].(string)
			return id
		}
	}
	return ""
}

// refusals returns the reasons of the orchestrator's "refusing the run" lines, sorted, and
// fails the test on one without a reason or a detail.
func (r *orchRun) refusals(t *testing.T) []string {
	t.Helper()
	var reasons []string
	for _, rec := range r.recs {
		if rec["msg"] != "refusing the run" {
			continue
		}
		reason, _ := rec["reason"].(string)
		detail, _ := rec["detail"].(string)
		if reason == "" || detail == "" {
			t.Errorf("a refusal line without a reason or a detail: %v", rec)
		}
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	return reasons
}

// checkChildrenGone fails the test for each process runs/<id>/pids.json lists that still runs
// once the orchestrator has exited, and kills it with its process group: each child leads its
// own (plan H3 step 4), so a live PID that leads no group of its own names another process.
func (r *orchRun) checkChildrenGone(t *testing.T) {
	t.Helper()
	path := filepath.Join(r.dir, runlog.PIDsFile)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return // the run ended before its first child started
	}
	pids, err := runlog.ReadPIDs(path)
	if err != nil {
		t.Errorf("run %s: %v", r.runID, err)
		return
	}
	for _, p := range pids.Procs {
		if pg, err := syscall.Getpgid(p.PID); err == nil && pg == p.PID {
			_ = syscall.Kill(-p.PID, syscall.SIGKILL) // the group's leader, and what it started
			t.Errorf("run %s: %s (pid %d) still ran after the orchestrator exited; killed", r.runID, p.Role, p.PID)
		}
	}
}

// removeStalePIDFile removes .data/orchestrator.pid when it names pid, an orchestrator that
// has exited without exiting cleanly (the test stopped it, or it crashed), which may not have
// removed it itself.
func (r *orchRun) removeStalePIDFile(t *testing.T, pid int) {
	t.Helper()
	path := filepath.Join(r.root, filepath.FromSlash(orchPIDFile))
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(pid) {
		if err := os.Remove(path); err != nil {
			t.Errorf("removing the killed orchestrator's pid file: %v", err)
		}
	}
}

// cleanup removes what the run left: its directory, its runs/index.jsonl line (the file too
// when the run created it), and runs/ when the run created it and it is empty.
func (r *orchRun) cleanup(t *testing.T) {
	if r.dir != "" {
		if err := os.RemoveAll(r.dir); err != nil {
			t.Errorf("removing %s: %v", r.dir, err)
		}
	}
	if r.runID != "" {
		r.dropIndexLine(t)
	}
	if !r.runsExisted {
		_ = os.Remove(filepath.Join(r.root, "runs")) // only an empty directory goes
	}
}

// dropIndexLine removes the run's line from runs/index.jsonl, and the file when the run
// created it and no other line is in it.
func (r *orchRun) dropIndexLine(t *testing.T) {
	data, err := os.ReadFile(r.indexPath)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Errorf("restoring runs/index.jsonl: %v", err)
		return
	}
	var keep []byte
	dropped := false
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		var rec struct {
			RunID string `json:"run_id"`
		}
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &rec) == nil && rec.RunID == r.runID {
			dropped = true
			continue
		}
		keep = append(keep, line...)
	}
	switch {
	case !dropped:
	case len(keep) == 0 && !r.indexExisted:
		err = os.Remove(r.indexPath)
	default:
		err = os.WriteFile(r.indexPath, keep, 0o600)
	}
	if err != nil {
		t.Errorf("restoring runs/index.jsonl: %v", err)
	}
}

// testdataPath is testdata/<name>, absolute: the orchestrator runs in the repository root.
func testdataPath(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// readJSON decodes the JSON file at path into v.
func readJSON(t testing.TB, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// orchAPIRecord is the part of an api-N.jsonl line these tests read (DESIGN §B8).
type orchAPIRecord struct {
	Kind          string `json:"kind"`
	Event         string `json:"event"`
	HTTPStatus    int    `json:"http_status"`
	Existed       *bool  `json:"existed"`
	ListenerDrops *int64 `json:"listener_drops"`
	LogDrops      *int64 `json:"log_drops"`
}

// orchGenRecord is the part of a gen.jsonl line these tests read (DESIGN §B4 "Record"):
// planned_ns and sent_ns are offsets from hitgen's t0, sent_ns null for an attempt never
// written; planned_ns is the entry's on every attempt, a retry's included; latency is null on
// an attempt never written, hint_ms and jitter_ns on a first attempt.
type orchGenRecord struct {
	Kind      string  `json:"kind"`
	PlannedNs int64   `json:"planned_ns"`
	SentNs    *int64  `json:"sent_ns"`
	Instance  int     `json:"instance"`
	RequestID string  `json:"request_id"`
	Attempt   int     `json:"attempt"`
	HintMs    *int64  `json:"hint_ms"`
	JitterNs  *int64  `json:"jitter_ns"`
	Status    int     `json:"status"`
	Error     *string `json:"error"`
	Latency   *int64  `json:"latency"`
	Unsent    *int64  `json:"unsent"`
	LogDrops  *int64  `json:"log_drops"`
}

// unsentCheck checks the gen.jsonl attempts a run's faults left unsent (sent_ns null), in
// place of checkFinishedRun's rule for a run without faults, the summary's unsent 0: it gets
// gen.jsonl's lines, the summary last.
type unsentCheck func(t *testing.T, gen []orchGenRecord)

// orchHealth is the part of an instance's /health these tests read, as samples.jsonl records
// it (DESIGN §B7, §B8).
type orchHealth struct {
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	ClockSource string `json:"clock_source"`
	Fixtures    *bool  `json:"fixtures"`
}

// orchSample is the part of a samples.jsonl line these tests read (DESIGN §B7, §B8): each
// process's RSS and error, the postmaster's PID, each instance's /health (null when the read
// failed), rladmin status --json, and the store's INFO persistence and INFO clients.
type orchSample struct {
	Procs []struct {
		Role     string  `json:"role"`
		Instance int     `json:"instance"`
		RSS      *int64  `json:"rss"`
		Error    *string `json:"error"`
	} `json:"procs"`
	Postgres *struct {
		PID int `json:"pid"`
	} `json:"postgres"`
	APIs []struct {
		Instance int         `json:"instance"`
		Health   *orchHealth `json:"health"`
	} `json:"apis"`
	Status json.RawMessage `json:"status"`
	Store  *struct {
		Persistence map[string]string `json:"persistence"`
		Clients     map[string]string `json:"clients"`
	} `json:"store"`
}

// sampleGaps lists what a samples.jsonl line lacks of a tick whose every read worked (DESIGN
// §B7; plan H3 step 7): the store's INFO persistence and INFO clients, neither empty; rladmin
// status --json, a JSON object; the postmaster's PID; and an RSS without an error for each of
// the run's processes while hitgen sends: valkey-server, hitapi 1…instances, sampler, hitgen.
func sampleGaps(s orchSample, instances int) []string {
	var gaps []string
	if s.Store == nil || len(s.Store.Persistence) == 0 || len(s.Store.Clients) == 0 {
		gaps = append(gaps, "store persistence and clients")
	}
	var status map[string]any
	if json.Unmarshal(s.Status, &status) != nil || status == nil {
		gaps = append(gaps, "status object")
	}
	if s.Postgres == nil || s.Postgres.PID <= 0 {
		gaps = append(gaps, "postgres pid")
	}
	read := map[string]bool{} // by role, and instance for hitapi: every entry with an RSS, no error
	for _, p := range s.Procs {
		name := p.Role
		if name == "hitapi" {
			name += " " + strconv.Itoa(p.Instance)
		}
		ok, seen := read[name]
		read[name] = (ok || !seen) && p.RSS != nil && *p.RSS > 0 && p.Error == nil
	}
	want := []string{"valkey-server"}
	for n := 1; n <= instances; n++ {
		want = append(want, "hitapi "+strconv.Itoa(n))
	}
	for _, name := range append(want, "sampler", "hitgen") {
		if !read[name] {
			gaps = append(gaps, name+" rss")
		}
	}
	return gaps
}

// insertSentinel puts the sentinel row (sentinelUser, sentinelRequest) in hits through
// harness/dbclient (Client.Hit) before a finished run, PostgreSQL started first unless it runs
// (pgtest.Ensure), and returns its client of harness.toml's db_dsn, closed at cleanup. The row
// may be there already; either way it is there when the run starts.
func insertSentinel(t *testing.T) *dbclient.Client {
	t.Helper()
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(filepath.Join(root, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Ensure(t)
	db, err := dbclient.Open(t.Context(), f.API.DBDSN)
	if err != nil {
		t.Fatalf("opening the database client: %v", err)
	}
	t.Cleanup(db.Close)
	ctx, cancel := context.WithTimeout(t.Context(), helperWait)
	defer cancel()
	if res := db.Hit(ctx, sentinelUser, sentinelRequest, 1, nil); res.Class != dbclient.Committed {
		t.Fatalf("putting the sentinel row (%s, %s) in hits: %v: %v", sentinelUser, sentinelRequest, res.Class, res.Err)
	}
	return db
}

// checkFinishedRun checks a run that must have finished (IMPLEMENTATION_PLAN H3 Tests, "Pass
// when"), started after insertSentinel put the sentinel row in hits through db: exit 0 with no
// refusal; the run directory holds config.toml, context.json, schedule.jsonl, gen.jsonl,
// api-1…N.jsonl, samples.jsonl, db_rows.json and store/; db_rows.json equals the api-N.jsonl
// request records with http_status 200 and existed = false, of which there is at least one;
// the sentinel row is gone (the run's TRUNCATE hits); each instance wrote one stop record, with
// listener_drops 0 and log_drops 0; gen.jsonl ends with its summary, log_drops 0 and unsent 0
// (when unsent is not nil, it checks the attempts the run's faults left unsent instead);
// samples.jsonl has at least minSamples lines, one of them with every read (sampleGaps);
// context.json's schedule_sha256 is the SHA-256 of runs/<id>/schedule.jsonl and its
// postgres_settings hold the DESIGN §B10 settings, each set, neither with a read error (plan H3
// step 5); and runs/index.jsonl gained exactly one line, {scenario, baseline, run_id}, naming
// this run.
func checkFinishedRun(t *testing.T, r *orchRun, db *dbclient.Client, scenario string, baseline bool, instances int, unsent unsentCheck) {
	t.Helper()
	if r.code != 0 || r.dir == "" {
		t.Fatalf("the orchestrator exited %d (run %q), want 0 after a finished run; its output:\n%s", r.code, r.runID, r.out)
	}
	if reasons := r.refusals(t); len(reasons) > 0 {
		t.Errorf("refusals %q in a finished run", reasons)
	}
	files := []string{"config.toml", "context.json", "schedule.jsonl", "gen.jsonl", "samples.jsonl", "db_rows.json"}
	for n := 1; n <= instances; n++ {
		files = append(files, fmt.Sprintf("api-%d.jsonl", n))
	}
	for _, name := range files {
		if info, err := os.Stat(filepath.Join(r.dir, name)); err != nil || !info.Mode().IsRegular() {
			t.Errorf("runs/%s/%s: %v, want a file", r.runID, name, err)
		}
	}
	if info, err := os.Stat(filepath.Join(r.dir, "store")); err != nil || !info.IsDir() {
		t.Errorf("runs/%s/store: %v, want a directory", r.runID, err)
	}
	if t.Failed() {
		t.FailNow()
	}

	var created int64
	for n := 1; n <= instances; n++ {
		stops := 0
		for _, rec := range readJSONLines[orchAPIRecord](t, filepath.Join(r.dir, fmt.Sprintf("api-%d.jsonl", n))) {
			switch {
			case rec.Kind == "request" && rec.HTTPStatus == 200 && rec.Existed != nil && !*rec.Existed:
				created++
			case rec.Kind == "lifecycle" && rec.Event == "stop":
				stops++
				if rec.ListenerDrops == nil || *rec.ListenerDrops != 0 || rec.LogDrops == nil || *rec.LogDrops != 0 {
					t.Errorf("api-%d.jsonl's stop record: listener_drops %s, log_drops %s; want 0 and 0", n,
						nullable(rec.ListenerDrops), nullable(rec.LogDrops))
				}
			}
		}
		if stops != 1 {
			t.Errorf("api-%d.jsonl has %d stop records, want 1", n, stops)
		}
	}
	var rows struct {
		DBRows *int64 `json:"db_rows"`
	}
	readJSON(t, filepath.Join(r.dir, "db_rows.json"), &rows)
	if created == 0 || rows.DBRows == nil || *rows.DBRows != created {
		t.Errorf("db_rows.json db_rows = %s, want %d (more than 0): the api-N.jsonl request records with http_status 200 and existed = false",
			nullable(rows.DBRows), created)
	}
	ctx, cancel := context.WithTimeout(t.Context(), helperWait)
	left, err := db.CountKey(ctx, sentinelUser, sentinelRequest)
	cancel()
	switch {
	case err != nil:
		t.Errorf("CountKey(%s, %s) after the run: %v", sentinelUser, sentinelRequest, err)
	case left != 0:
		t.Errorf("hits holds the sentinel row (%s, %s) put in before the run, want it gone: the run empties the table with TRUNCATE hits before it starts (HARNESS §4 step 1)",
			sentinelUser, sentinelRequest)
	}

	gen := readJSONLines[orchGenRecord](t, filepath.Join(r.dir, "gen.jsonl"))
	if len(gen) == 0 || gen[len(gen)-1].Kind != "summary" {
		t.Fatalf("gen.jsonl (%d lines) does not end with its summary", len(gen))
	}
	if s := gen[len(gen)-1]; s.LogDrops == nil || *s.LogDrops != 0 {
		t.Errorf("gen.jsonl summary: log_drops %s, want 0", nullable(s.LogDrops))
	}
	if unsent != nil {
		unsent(t, gen)
	} else if s := gen[len(gen)-1]; s.Unsent == nil || *s.Unsent != 0 {
		t.Errorf("gen.jsonl summary: unsent %s, want 0", nullable(s.Unsent))
	}
	samples := readJSONLines[orchSample](t, filepath.Join(r.dir, "samples.jsonl"))
	if len(samples) < minSamples {
		t.Errorf("samples.jsonl has %d lines, want at least %d", len(samples), minSamples)
	}
	var lacks []string
	for i, s := range samples {
		gaps := sampleGaps(s, instances)
		if len(gaps) == 0 {
			lacks = nil
			break
		}
		lacks = append(lacks, fmt.Sprintf("line %d: %s", i+1, strings.Join(gaps, ", ")))
	}
	if len(lacks) > 0 || len(samples) == 0 {
		t.Errorf("no samples.jsonl line has every read, want one with the store's persistence and clients, status a JSON object, postgres.pid > 0 and procs valkey-server, hitapi 1 to %d, sampler and hitgen, each with its rss and no error; each line lacks:\n%s",
			instances, strings.Join(lacks, "\n"))
	}

	var rc struct {
		ScheduleSHA256   *string           `json:"schedule_sha256"`
		PostgresSettings map[string]string `json:"postgres_settings"`
		ReadErrors       map[string]string `json:"read_errors"`
	}
	readJSON(t, filepath.Join(r.dir, "context.json"), &rc)
	schedule, err := os.ReadFile(filepath.Join(r.dir, "schedule.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(schedule)
	if want := hex.EncodeToString(sum[:]); rc.ScheduleSHA256 == nil || *rc.ScheduleSHA256 != want {
		t.Errorf("context.json schedule_sha256 %s, want %s, the SHA-256 of runs/%s/schedule.jsonl", nullable(rc.ScheduleSHA256), want, r.runID)
	}
	for _, key := range []string{"max_connections", "statement_timeout", "fsync", "synchronous_commit"} { // DESIGN §B10
		if rc.PostgresSettings[key] == "" {
			t.Errorf("context.json postgres_settings %v has no %s, want each DESIGN §B10 setting with its value", rc.PostgresSettings, key)
		}
	}
	for _, key := range []string{"schedule_sha256", "postgres_settings"} {
		if msg, ok := rc.ReadErrors[key]; ok {
			t.Errorf("context.json read_errors has %s: %q, want none", key, msg)
		}
	}

	after, err := os.ReadFile(r.indexPath)
	if err != nil {
		t.Fatalf("runs/index.jsonl after the run: %v", err)
	}
	added, ok := bytes.CutPrefix(after, r.indexBefore)
	want := map[string]any{"scenario": scenario, "baseline": baseline, "run_id": r.runID}
	var got map[string]any
	if !ok || bytes.Count(added, []byte("\n")) != 1 || !bytes.HasSuffix(added, []byte("\n")) ||
		json.Unmarshal(added, &got) != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("runs/index.jsonl gained %q (it held %q before), want the one line %v", added, r.indexBefore, want)
	}
}

// aofCommands returns the commands of the AOF in a run's store directory, in order, each as
// its arguments: the incremental files its appendonlydir manifest lists (the base file of a
// fresh store holds no command).
func aofCommands(t *testing.T, storeDir string) [][]string {
	t.Helper()
	dir := filepath.Join(storeDir, "appendonlydir")
	manifest, err := os.ReadFile(filepath.Join(dir, "appendonly.aof.manifest"))
	if err != nil {
		t.Fatal(err)
	}
	var cmds [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(manifest)), "\n") {
		f := strings.Fields(line)
		kv := map[string]string{}
		for i := 0; i+1 < len(f); i += 2 {
			kv[f[i]] = f[i+1]
		}
		if kv["type"] != "i" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, kv["file"]))
		if err != nil {
			t.Fatal(err)
		}
		rd := resp.NewReader(bytes.NewReader(data))
		for {
			v, err := rd.ReadValue()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil || v.Kind != resp.Array || len(v.Elems) == 0 {
				t.Fatalf("%s: command %d is %s (%v), want an array of arguments", kv["file"], len(cmds)+1, v, err)
			}
			args := make([]string, len(v.Elems))
			for i, e := range v.Elems {
				if e.Kind != resp.Bulk || e.Null {
					t.Fatalf("%s: command %d's argument %d is %s, want a bulk string", kv["file"], len(cmds)+1, i, e)
				}
				args[i] = e.Str
			}
			cmds = append(cmds, args)
		}
	}
	return cmds
}

// TestDesign_OrchestratorTestRun (IMPLEMENTATION_PLAN H3 step 9 and Tests; DESIGN §B5, §B6
// "Controlled time"; HARNESS §4): bin/test/orchestrator runs testdata/orch-test.toml, a
// controlled-clock test run of three instances, end to end: exit 0 and a finished run's
// evidence (checkFinishedRun: the run files; db_rows equal to the 200 / existed = false request
// records, and the sentinel row put in before the run gone; stop records and the hitgen summary
// without drops; at least 8 samples, one with every read; context.json's schedule hash and
// PostgreSQL settings; one runs/index.jsonl line {"orch-test", false, run_id}). The store's AOF
// shows rl:ctl:time set before the epoch was started, and the epoch record took its time from
// it (clock controlled, origin = the time set, which context.json records in ns; HARNESS §4
// step 2); that time is the run's now truncated to µs, after context.json's start_ns - 1000
// and at most its end_ns (plan H3 step 3). None of the four passwords the run generated
// appears in any file under runs/<id>/ but store/acl.conf, the children's saved output
// (runs/<id>/*.stderr) among them, nor in the orchestrator's own output (AGENTS.md §1
// Secrets).
func TestDesign_OrchestratorTestRun(t *testing.T) {
	db := insertSentinel(t)
	r := runOrchestrator(t, "--scenario", testdataPath(t, "orch-test.toml"))
	checkFinishedRun(t, r, db, "orch-test", false, 3, nil)

	clockAt, epochAt := -1, -1
	var clockUs string
	var epoch map[string]string
	for i, c := range aofCommands(t, filepath.Join(r.dir, "store")) {
		if len(c) < 2 {
			continue // MULTI, EXEC
		}
		name := strings.ToUpper(c[0])
		if c[1] == "rl:ctl:time" && name == "SET" && len(c) == 3 && clockAt < 0 {
			clockAt, clockUs = i, c[2]
		}
		if c[1] == "rl:epoch" && epochAt < 0 {
			epochAt = i
		}
		if c[1] == "rl:epoch" && name == "HSET" && epoch == nil {
			epoch = map[string]string{}
			for j := 2; j+1 < len(c); j += 2 {
				epoch[c[j]] = c[j+1]
			}
		}
	}
	if clockAt < 0 || epochAt < 0 || clockAt > epochAt {
		t.Errorf("the AOF sets rl:ctl:time at command %d and first writes rl:epoch at command %d, want the clock set first (-1: none)", clockAt, epochAt)
	}
	if epoch["clock"] != "controlled" || epoch["origin"] != clockUs {
		t.Errorf("the epoch record %v, want clock controlled and origin %s, the time set before it", epoch, clockUs)
	}
	var rc struct {
		ControlledClockNs *int64 `json:"controlled_clock_ns"`
		StartNs           *int64 `json:"start_ns"`
		EndNs             *int64 `json:"end_ns"`
	}
	readJSON(t, filepath.Join(r.dir, "context.json"), &rc)
	if us, err := strconv.ParseInt(clockUs, 10, 64); err != nil || rc.ControlledClockNs == nil || *rc.ControlledClockNs != us*1000 {
		t.Errorf("context.json controlled_clock_ns %s, want %s µs × 1000, the value the AOF holds", nullable(rc.ControlledClockNs), clockUs)
	}
	// The time set is the run's now truncated to µs (plan H3 step 3), taken after start_ns.
	if c := rc.ControlledClockNs; c == nil || rc.StartNs == nil || rc.EndNs == nil || *c <= *rc.StartNs-1000 || *c > *rc.EndNs {
		t.Errorf("context.json controlled_clock_ns %s, start_ns %s, end_ns %s; want start_ns - 1000 < controlled_clock_ns <= end_ns: the run's now, truncated to µs",
			nullable(c), nullable(rc.StartNs), nullable(rc.EndNs))
	}

	acl, err := os.ReadFile(filepath.Join(r.dir, "store", "acl.conf"))
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	pws := map[string]string{}
	for _, line := range strings.Split(string(acl), "\n") {
		if m := aclUser.FindStringSubmatch(line); m != nil {
			users = append(users, m[1])
			pws[m[1]] = m[2]
		}
	}
	if want := []string{"rl_lib", "rl_admin", "rl_fixture", "rl_ops"}; !slices.Equal(users, want) {
		t.Fatalf("store/acl.conf defines the users %q, want %q (a test run's four)", users, want)
	}
	for user, pw := range pws {
		if b, err := hex.DecodeString(pw); err != nil || len(b) != 32 {
			t.Errorf("%s's password is not 32 bytes in hex", user)
		}
	}
	checked := 0
	err = filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == filepath.Join(r.dir, "store", "acl.conf") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		for user, pw := range pws {
			if bytes.Contains(data, []byte(pw)) {
				t.Errorf("%s holds %s's password", strings.TrimPrefix(path, r.root+string(filepath.Separator)), user)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 10 {
		t.Errorf("checked %d files under runs/%s for the passwords, want every run file (at least 10)", checked, r.runID)
	}
	for user, pw := range pws {
		if bytes.Contains(r.out, []byte(pw)) {
			t.Errorf("the orchestrator's output holds %s's password", user)
		}
	}
}

// startOrchestratorPiped starts bin/test/orchestrator with args in the repository root as
// runOrchestrator does (PATH and HOME as its whole environment, recorded in .out/procs/<pid>
// until it is reaped), but with its stderr, where it logs, on a pipe: it returns the child and
// the pipe's read end, which the caller closes. Its stdout goes to /dev/null: it logs only to
// stderr.
func startOrchestratorPiped(t *testing.T, root string, args ...string) (*child, *os.File) {
	t.Helper()
	bin := filepath.Join(root, "bin", "test", "orchestrator")
	if err := checkBinary(bin); err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), bin, args...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	cmd.Stderr = pw
	err = cmd.Start()
	_ = pw.Close() // the child holds its own descriptor
	if err != nil {
		_ = pr.Close()
		t.Fatalf("starting %s: %v", bin, err)
	}
	c := &child{
		Cmdline: strings.Join(append([]string{bin}, args...), " "),
		PID:     cmd.Process.Pid,
		cmd:     cmd,
		record:  filepath.Join(root, ".out", "procs", strconv.Itoa(cmd.Process.Pid)),
		done:    make(chan struct{}),
	}
	if err := writeRecord(c.record, c.Cmdline); err != nil {
		_ = cmd.Process.Kill() // unrecorded, it must not outlive this call
		_ = cmd.Wait()
		_ = pr.Close()
		t.Fatal(errors.Join(err, removeRecord(c.record)))
	}
	go func() {
		c.waitErr = cmd.Wait()
		c.recErr = removeRecord(c.record)
		close(c.done)
	}()
	return c, pr
}

// TestDesign_OrchestratorStderrGone (IMPLEMENTATION_PLAN H3 step 4: "On any failure or SIGINT:
// SIGTERM the groups, wait, SIGKILL, shut the store down, exit 1", "Remove
// .data/orchestrator.pid at every exit"): bin/test/orchestrator runs testdata/orch-test.toml
// with its stderr, where it logs, on a pipe. Once hitgen is sending, the pipe's reader goes, as
// tee does when Ctrl-C ends `make run … 2>&1 | tee log`, and the orchestrator gets SIGINT. Its
// log lines then fail with EPIPE instead of killing it with SIGPIPE, so it tears the run down:
// it exits 1, every process runs/<id>/pids.json lists is gone, .data/orchestrator.pid is
// removed, context.json's outcome is failed, and runs/index.jsonl gets no line for the run.
func TestDesign_OrchestratorStderrGone(t *testing.T) {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	r := &orchRun{root: root, indexPath: filepath.Join(root, "runs", "index.jsonl")}
	switch _, err := os.Stat(filepath.Join(root, "runs")); {
	case err == nil:
		r.runsExisted = true
	case !errors.Is(err, fs.ErrNotExist):
		t.Fatal(err)
	}
	switch data, err := os.ReadFile(r.indexPath); {
	case err == nil:
		r.indexBefore, r.indexExisted = data, true
	case !errors.Is(err, fs.ErrNotExist):
		t.Fatal(err)
	}
	t.Cleanup(func() { r.cleanup(t) })

	c, pr := startOrchestratorPiped(t, root, "--scenario", testdataPath(t, "orch-test.toml"))
	var read []string // the lines read before the reader went; the reading goroutine's until readDone
	sending, readDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			read = append(read, sc.Text())
			var rec struct {
				Msg string `json:"msg"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Msg == "hitgen is sending: t0" {
				close(sending)
				return
			}
		}
	}()
	wait := time.NewTimer(orchWait)
	defer wait.Stop()
	sent := false
	select {
	case <-sending:
		sent = true
	case <-c.done:
	case <-wait.C:
	}
	_ = pr.Close() // the reader goes: the orchestrator's next log line meets a pipe with no reader
	<-readDone
	if sent {
		c.signal(t, syscall.SIGINT)
	} else {
		c.signal(t, syscall.SIGTERM)
	}
	exited := c.exitedWithin(orchStopWait)
	if !exited {
		c.kill(t)
	}
	c.checkRecord(t)
	recs, _ := splitLog([]byte(strings.Join(read, "\n")))
	if id := runIDOf(recs); runIDForm.MatchString(id) {
		r.runID, r.dir = id, filepath.Join(root, "runs", id)
		r.checkChildrenGone(t)
	}
	pidPath := filepath.Join(root, filepath.FromSlash(orchPIDFile))
	if data, err := os.ReadFile(pidPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf(".data/orchestrator.pid after the orchestrator exited: %q (%v), want it removed", data, err)
		r.removeStalePIDFile(t, c.PID)
	}
	if !sent {
		t.Fatalf("the orchestrator did not log \"hitgen is sending: t0\" within %v; the lines it logged:\n%s", orchWait, strings.Join(read, "\n"))
	}
	if !exited {
		t.Fatalf("the orchestrator did not exit within %v of SIGINT", orchStopWait)
	}
	if code, err := exitCode(c.waitErr); err != nil || code != 1 {
		t.Errorf("the orchestrator's exit: code %d, %v; want exit 1 after its teardown (a SIGPIPE death leaves the children running)", code, err)
	}
	if r.runID == "" {
		t.Fatalf("no \"run directory ready\" line among the lines read:\n%s", strings.Join(read, "\n"))
	}
	var rc struct {
		Outcome string `json:"outcome"`
		Error   string `json:"error"`
	}
	readJSON(t, filepath.Join(r.dir, "context.json"), &rc)
	if rc.Outcome != "failed" {
		t.Errorf("context.json outcome %q, error %q; want failed: the run was stopped", rc.Outcome, rc.Error)
	}
	if index, err := os.ReadFile(r.indexPath); err == nil && bytes.Contains(index, []byte(`"run_id":"`+r.runID+`"`)) {
		t.Errorf("runs/index.jsonl names the stopped run %s, want no line for it", r.runID)
	}
}

// TestDesign_OrchestratorBaselineBench (IMPLEMENTATION_PLAN H3 Tests; DESIGN §B5 "Construction",
// "Benchmark guard"; HARNESS HAR-24): bin/test/orchestrator runs testdata/orch-baseline.toml, a
// benchmark scenario, with --baseline: it launches bin/bench/hitapi (a benchmark run's bin
// directory), passes the benchmark guard, its /health check of the running instances included
// (no refusal), and exits 0 after a finished run (checkFinishedRun, its index line
// {"orch-baseline", true, run_id}). Every instance's /health, as samples.jsonl records it,
// reports fixtures: false and clock_source store, which only a bench build reports: a bin/test
// hitapi builds its limiter through limitertest.New (fixtures: true). context.json labels the
// run a benchmark baseline run, its settings with limiter_enabled = false (HAR-17).
func TestDesign_OrchestratorBaselineBench(t *testing.T) {
	db := insertSentinel(t)
	r := runOrchestrator(t, "--scenario", testdataPath(t, "orch-baseline.toml"), "--baseline")
	checkFinishedRun(t, r, db, "orch-baseline", true, 3, nil)

	read := map[int]int{}
	for i, s := range readJSONLines[orchSample](t, filepath.Join(r.dir, "samples.jsonl")) {
		for _, a := range s.APIs {
			if a.Health == nil {
				continue
			}
			read[a.Instance]++
			if a.Health.Fixtures == nil || *a.Health.Fixtures || a.Health.ClockSource != "store" {
				t.Errorf("sample %d: instance %d's /health shows fixtures %s, clock_source %q; want false and store (a bench build)",
					i+1, a.Instance, nullable(a.Health.Fixtures), a.Health.ClockSource)
			}
		}
	}
	for n := 1; n <= 3; n++ {
		if read[n] == 0 {
			t.Errorf("no sample read instance %d's /health", n)
		}
	}
	var rc struct {
		RunKind  string         `json:"run_kind"`
		RunLabel string         `json:"run_label"`
		Baseline bool           `json:"baseline"`
		Settings map[string]any `json:"settings"`
	}
	readJSON(t, filepath.Join(r.dir, "context.json"), &rc)
	api, _ := rc.Settings["api"].(map[string]any)
	if rc.RunKind != "bench" || rc.RunLabel != "benchmark" || !rc.Baseline || api["limiter_enabled"] != false {
		t.Errorf("context.json run_kind %q, run_label %q, baseline %v, [api] %v; want bench, benchmark, true, limiter_enabled false",
			rc.RunKind, rc.RunLabel, rc.Baseline, api)
	}
}

// TestT_HAR_1 (HARNESS §8 T-HAR-1: HAR-12, HOST-5; IMPLEMENTATION_PLAN H3 Tests): bin/test/
// orchestrator runs testdata/orch-test.toml with --instance-config 3=testdata/har1-mismatch.toml,
// so instance 3 starts with a config file whose shared settings differ from the others'
// (failure_mode open where the run's config.toml has closed). That instance refuses: every
// gen.jsonl attempt routed to it is 503 limiter_config_error; it reports not ready: every
// samples.jsonl read of its /health shows status not_ready, reason fingerprint_mismatch. The
// others serve: instances 1 and 2 answer only 200 or 429, some 200, and their /health shows
// ready. The run itself finishes (checkFinishedRun).
func TestT_HAR_1(t *testing.T) {
	db := insertSentinel(t)
	r := runOrchestrator(t, "--scenario", testdataPath(t, "orch-test.toml"),
		"--instance-config", "3="+testdataPath(t, "har1-mismatch.toml"))
	checkFinishedRun(t, r, db, "orch-test", false, 3, nil)

	attempts, ok := map[int]int{}, map[int]int{}
	for i, g := range readJSONLines[orchGenRecord](t, filepath.Join(r.dir, "gen.jsonl")) {
		if g.Kind == "summary" {
			continue
		}
		attempts[g.Instance]++
		switch g.Instance {
		case 3:
			if g.Status != 503 || g.Error == nil || *g.Error != "limiter_config_error" {
				t.Errorf("gen.jsonl line %d: instance 3 answered %d %s, want 503 limiter_config_error", i+1, g.Status, nullable(g.Error))
			}
		case 1, 2:
			if g.Status != 200 && g.Status != 429 {
				t.Errorf("gen.jsonl line %d: instance %d answered %d %s, want 200 or 429", i+1, g.Instance, g.Status, nullable(g.Error))
			}
			if g.Status == 200 {
				ok[g.Instance]++
			}
		default:
			t.Errorf("gen.jsonl line %d: an attempt to instance %d, want 1 to 3", i+1, g.Instance)
		}
	}
	if attempts[3] == 0 || ok[1] == 0 || ok[2] == 0 {
		t.Errorf("attempts per instance %v, 200s %v; want attempts to instance 3 and 200s from instances 1 and 2", attempts, ok)
	}

	read, ready := map[int]int{}, map[int]int{}
	for i, s := range readJSONLines[orchSample](t, filepath.Join(r.dir, "samples.jsonl")) {
		for _, a := range s.APIs {
			if a.Health == nil {
				continue
			}
			read[a.Instance]++
			if a.Health.Status == "ready" {
				ready[a.Instance]++
			}
			if a.Instance == 3 && (a.Health.Status != "not_ready" || a.Health.Reason != "fingerprint_mismatch") {
				t.Errorf("sample %d: instance 3's /health shows status %q, reason %q; want not_ready, fingerprint_mismatch",
					i+1, a.Health.Status, a.Health.Reason)
			}
		}
	}
	if read[3] == 0 || ready[1] == 0 || ready[2] == 0 {
		t.Errorf("/health reads per instance %v, ready ones %v; want instance 3 read, and instances 1 and 2 ready", read, ready)
	}
}

// TestT_FIX_2 (HARNESS §8 T-FIX-2: HAR-24; IMPLEMENTATION_PLAN H3 Tests; DESIGN §B5 "Benchmark
// guard"): bin/test/orchestrator refuses to launch a benchmark run, exit 2 with a "refusing the
// run" line naming each reason: (1) a bench scenario with [policy] clock_source = "controlled"
// (an override, and the controlled clock); (2) one with a [[faults]] entry (a fault wrapper
// armed through the control endpoint); (3) a --bin-dir copy of bin/bench whose hitapi is
// bin/test/hitapi, built with -tags=rltest (fault wrappers and the control endpoint compiled
// in). Each launch is a baseline run, which needs no frozen thresholds, so the guard's are its
// only refusals. Each refusal comes before the store starts: nothing listens on 6390, and the
// scenario has no runs/<id>/store/appendonlydir.
func TestT_FIX_2(t *testing.T) {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	const load = "[[steps]]\nrate_per_s = 30\nduration_s = 10\n\n[users]\ncount = 50\n"
	benchScenario := func(name, tables string) string {
		path := filepath.Join(dir, name+".toml")
		text := "name = \"" + name + "\"\nseed = 1\nrun_kind = \"bench\"\ninstances = 3\n\n" + tables + load
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	binDir := filepath.Join(dir, "bin-dir")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{"hitapi": filepath.Join(root, "bin", "test", "hitapi"), "rladmin": filepath.Join(root, "bin", "bench", "rladmin")} {
		if err := checkBinary(src); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, name), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct {
		name     string // the scenario's name
		tables   string // its tables before the load
		binDir   bool   // --bin-dir the copy with bin/test/hitapi
		want     []string
		inDetail string // in the refusal's detail
	}{
		{name: "fix2-clock", tables: "[policy]\nclock_source = \"controlled\"\n\n",
			want: []string{"bench_clock_controlled", "bench_clock_override"}, inDetail: "clock"},
		{name: "fix2-faults", tables: "[[faults]]\nat_s = 1\ncmd = '{\"target\":\"db\",\"mode\":\"fail_before\",\"count\":1}'\n\n",
			want: []string{"bench_faults"}, inDetail: "[[faults]]"},
		{name: "fix2-bin-dir", binDir: true, want: []string{"bench_binary_rltest"}, inDetail: filepath.Join(binDir, "hitapi")},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"--scenario", benchScenario(c.name, c.tables), "--baseline"}
			if c.binDir {
				args = append(args, "--bin-dir", binDir)
			}
			r := runOrchestrator(t, args...)
			if r.code != 2 {
				t.Errorf("exit code %d, want 2: a refused launch", r.code)
			}
			if got := r.refusals(t); !slices.Equal(got, c.want) {
				t.Errorf("refusals %q, want %q", got, c.want)
			}
			named := false
			for _, rec := range r.recs {
				if detail, _ := rec["detail"].(string); rec["msg"] == "refusing the run" && strings.Contains(detail, c.inDetail) {
					named = true
				}
			}
			if !named {
				t.Errorf("no refusal's detail names %q; the orchestrator's output:\n%s", c.inDetail, r.out)
			}
			var d net.Dialer
			if conn, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(orchStorePort))); err == nil {
				_ = conn.Close() // only the connection's success mattered
				t.Errorf("127.0.0.1:%d accepts connections after the refusal, want no store started", orchStorePort)
			}
			aof, err := filepath.Glob(filepath.Join(root, "runs", "*-"+c.name+"-1", "store", "appendonlydir"))
			if err != nil || len(aof) > 0 {
				t.Errorf("store directories %q (%v) after the refusal, want none: no valkey-server started", aof, err)
			}
		})
	}
}

// orchFaultRecord is a context.json faults entry (IMPLEMENTATION_PLAN H3 step 3, H4 step 7).
type orchFaultRecord struct {
	AtS           float64           `json:"at_s"`
	Cmd           string            `json:"cmd"`
	Instance      *int              `json:"instance"`
	StartNs       int64             `json:"start_ns"`
	EndNs         int64             `json:"end_ns"`
	OutageStartNs *int64            `json:"outage_start_ns"`
	OutageNs      *int64            `json:"outage_ns"`
	Targets       []json.RawMessage `json:"targets"`
	Error         string            `json:"error"`
}

// orchChildRecord is a context.json children entry (plan H3 step 4, H4 step 7).
type orchChildRecord struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	PID    int    `json:"pid"`
	Exit   string `json:"exit"`
	Early  bool   `json:"exited_before_stop"`
	Killed bool   `json:"killed"`
	Fault  string `json:"fault"`
	Error  string `json:"error"`
}

// TestDesign_OrchestratorFaultctl (IMPLEMENTATION_PLAN H4 step 7; DESIGN §B4 [[faults]], §B5
// "Faults", §B6): bin/test/orchestrator runs testdata/orch-faults.toml, whose two faults run
// faultctl inside the orchestrator: api kill 2 --restart at 1 s and store down 4s at 3 s. The
// run finishes, exit 0, with a finished run's evidence (checkFinishedRun: among it each
// instance's one stop record, the restarted hitapi 2's included), except that the attempts
// the kill leaves unsent, refused while hitapi 2 restarts, are accepted, exactly those
// (checkKillUnsent). context.json records both faults without an error, each with its
// command's own start_ns and end_ns: the kill with instance 2 and no outage_start_ns or
// outage_ns; the store down with no instance and an outage_ns of more than the commanded 4,000
// ms, at most 5,000 ms (DESIGN §B6: from SHUTDOWN to the first PING) from its outage_start_ns,
// inside its start_ns and end_ns within the wall clock's adjustment meanwhile
// (checkOutageBounds). Each restarted process is the orchestrator's child: pids.json ends with
// the restarted hitapi 2 and store, each under a PID other than the one it replaced;
// context.json's children show the killed hitapi 2 (signal: killed) and the shut-down store
// (exit 0) each with its fault and not as an early exit, and the run's end stops the restarted
// ones: hitapi 2 by SIGTERM, exit 0, neither killed nor early, in context.json's children; the
// store by SHUTDOWN, exit 0, in the orchestrator's "child stopped" line (the run shuts its
// store down after it wrote context.json and ran the report, plan H3 step 3). api-2.jsonl
// holds the pre_kill line, read_ok:true with both drop counts 0, and the restarted process's
// stop record after it; samples.jsonl shows the sampler reached both restarted processes
// under their new PIDs (an rss, no error).
func TestDesign_OrchestratorFaultctl(t *testing.T) {
	const (
		killCmd = "faultctl api kill 2 --restart"
		downCmd = "faultctl store down 4s"
		downFor = 4 * time.Second // the store down's commanded duration
	)
	db := insertSentinel(t)
	before := time.Now()
	r := runOrchestrator(t, "--scenario", testdataPath(t, "orch-faults.toml"))
	adj := clockAdjustment(before, time.Now())
	checkFinishedRun(t, r, db, "orch-faults", false, 3, func(t *testing.T, gen []orchGenRecord) { checkKillUnsent(t, r, killCmd, gen) })

	var rc struct {
		Faults   []orchFaultRecord `json:"faults"`
		Children []orchChildRecord `json:"children"`
	}
	readJSON(t, filepath.Join(r.dir, "context.json"), &rc)
	if len(rc.Faults) != 2 {
		t.Fatalf("context.json faults %+v, want the two [[faults]] entries", rc.Faults)
	}
	kill, down := rc.Faults[0], rc.Faults[1]
	if kill.Cmd != killCmd || kill.AtS != 1 || kill.Error != "" || kill.Instance == nil || *kill.Instance != 2 ||
		kill.StartNs <= 0 || kill.EndNs < kill.StartNs || kill.OutageNs != nil || kill.OutageStartNs != nil || len(kill.Targets) != 0 {
		t.Errorf("the api kill's record %+v, want at_s 1, instance 2, start_ns <= end_ns, outage_ns and outage_start_ns null, no targets, no error", kill)
	}
	if down.Cmd != downCmd || down.AtS != 3 || down.Error != "" || down.Instance != nil || down.OutageNs == nil ||
		*down.OutageNs <= int64(downFor) || *down.OutageNs > int64(5*time.Second) || down.EndNs <= down.StartNs {
		t.Errorf("the store down's record %+v (outage_ns %s), want at_s 3, no instance, an outage_ns of more than 4,000 ms, at most 5,000 ms, "+
			"between start_ns and end_ns, no error", down, nullable(down.OutageNs))
	}
	checkOutageBounds(t, down, downFor, adj)

	pids, err := runlog.ReadPIDs(filepath.Join(r.dir, runlog.PIDsFile))
	if err != nil {
		t.Fatal(err)
	}
	final := map[string]int{}
	for _, p := range pids.Procs {
		final[p.Role+" "+strconv.Itoa(p.Instance)] = p.PID
	}
	for _, c := range []struct {
		name, role, key, fault, endedExit string
		storeEnd                          bool // the restarted process's end is in the log only
	}{
		{name: "hitapi-2", role: "hitapi", key: "hitapi 2", fault: killCmd, endedExit: "signal: killed"},
		{name: "valkey-server", role: "valkey-server", key: "valkey-server 0", fault: downCmd, endedExit: "exit 0", storeEnd: true},
	} {
		var recs []orchChildRecord
		for _, ch := range rc.Children {
			if ch.Name == c.name {
				recs = append(recs, ch)
			}
		}
		if c.storeEnd { // the run's SHUTDOWN of the restarted store, as the orchestrator logged it
			for _, l := range r.recs {
				if l["msg"] == "child stopped" && l["name"] == c.name && l["pid"] == float64(final[c.key]) {
					recs = append(recs, orchChildRecord{Name: c.name, Role: c.role, PID: final[c.key], Exit: fmt.Sprint(l["exit"])})
				}
			}
		}
		if len(recs) != 2 {
			t.Errorf("the ends of %s: %+v, want two: the process the fault ended, then the one that replaced it, stopped by the run's end", c.name, recs)
			continue
		}
		ended, restarted := recs[0], recs[1]
		if ended.Fault != c.fault || ended.Exit != c.endedExit || ended.Early || ended.Killed || ended.Error != "" {
			t.Errorf("%s's ended process %+v, want exit %q, fault %q, not early, not killed", c.name, ended, c.endedExit, c.fault)
		}
		if restarted.Fault != "" || restarted.Exit != "exit 0" || restarted.Early || restarted.Killed || restarted.Error != "" ||
			restarted.PID == ended.PID || final[c.key] != restarted.PID {
			t.Errorf("%s's restarted process %+v (pids.json: %d), want the PID pids.json names, other than %d, exit 0 at the run's end, not early, not killed, no fault",
				c.name, restarted, final[c.key], ended.PID)
		}
		var sampled bool
		for _, s := range readJSONLines[faultSample](t, filepath.Join(r.dir, "samples.jsonl")) {
			for _, p := range s.Procs {
				if p.Role == c.role && p.PID == restarted.PID && p.RSS != nil && *p.RSS > 0 && p.Error == nil {
					sampled = true
				}
			}
		}
		if !sampled {
			t.Errorf("no samples.jsonl line has %s's restarted process %d with an rss and no error: the sampler never reached it", c.name, restarted.PID)
		}
	}

	type lifecycle struct {
		Kind          string `json:"kind"`
		Event         string `json:"event"`
		ListenerDrops *int64 `json:"listener_drops"`
		LogDrops      *int64 `json:"log_drops"`
		ReadOK        *bool  `json:"read_ok"`
	}
	preKill, stopsAfter := -1, 0
	for i, l := range readJSONLines[lifecycle](t, filepath.Join(r.dir, "api-2.jsonl")) {
		switch {
		case l.Kind == "lifecycle" && l.Event == "pre_kill":
			preKill = i
			if l.ReadOK == nil || !*l.ReadOK || l.ListenerDrops == nil || *l.ListenerDrops != 0 || l.LogDrops == nil || *l.LogDrops != 0 {
				t.Errorf("api-2.jsonl's pre_kill line %+v, want read_ok true, listener_drops 0, log_drops 0", l)
			}
		case l.Kind == "lifecycle" && l.Event == "stop" && preKill >= 0:
			stopsAfter++
		}
	}
	if preKill < 0 || stopsAfter != 1 {
		t.Errorf("api-2.jsonl: pre_kill line at %d, %d stop records after it; want the pre_kill line, then the restarted process's one stop record", preKill, stopsAfter)
	}
}

// checkOutageBounds checks the context.json record of a store fault commanded for d
// (IMPLEMENTATION_PLAN H4 step 7; DESIGN §B5 "Faults", §B6): start_ns and end_ns are the
// command's own times, and the outage it measured, outage_ns long (from SHUTDOWN or SIGSTOP to
// the first PING answered) from outage_start_ns, lasts more than d and at most 5 s and lies
// inside them. More than d: it ends at the first PING answered after SIGCONT or the restart, d
// after it began, so an outage_ns that was not measured, d itself among them, fails. Inside:
// start_ns <= outage_start_ns and outage_start_ns + outage_ns <= end_ns, each within |adj|,
// the wall clock's adjustment the test saw around the run (clockAdjustment): outage_ns is a
// monotonic length and the others wall-clock readings, so a step or slew of the system clock
// meanwhile moves them apart by that much.
func checkOutageBounds(t *testing.T, rec orchFaultRecord, d time.Duration, adj int64) {
	t.Helper()
	if rec.OutageNs == nil || rec.OutageStartNs == nil {
		t.Errorf("the store fault's record %+v has no outage_ns or outage_start_ns", rec)
		return
	}
	from, length, slack := *rec.OutageStartNs, *rec.OutageNs, max(adj, -adj)
	if length <= int64(d) || from < rec.StartNs-slack || from+length > rec.EndNs+slack || length > int64(5*time.Second) {
		t.Errorf("%s: an outage of outage_ns %d from outage_start_ns %d, start_ns %d, end_ns %d (the wall clock adjusted by %d ns meanwhile); "+
			"want more than the commanded %v and at most 5 s, inside the command's times: start_ns <= outage_start_ns and "+
			"outage_start_ns + outage_ns <= end_ns, within that adjustment",
			rec.Cmd, length, from, rec.StartNs, rec.EndNs, adj, d)
	}
}

// clockAdjustment is how far the wall clock moved beyond the monotonic clock between before
// and after, two time.Now readings: the system clock's steps and slews in between (timed on
// macOS slews it by up to a few ms a second and sometimes steps it by tens of ms).
func clockAdjustment(before, after time.Time) int64 {
	return (after.UnixNano() - before.UnixNano()) - after.Sub(before).Nanoseconds()
}

// killUnsentMargin is how far outside the api kill's [start_ns, end_ns] an attempt the kill
// left unsent may be made (plan choice): hitgen launches an attempt up to a few ms after its
// planned time (a retry, after its timer's), and t0 comes from hitgen's "sending" line, written
// just before its sender takes t0.
const killUnsentMargin = 50 * time.Millisecond

// checkKillUnsent checks the attempts orch-faults' api kill leaves unsent (an unsentCheck):
// while hitapi 2 is killed and restarting (tens of ms), a connect to it is refused, and hitgen
// logs that attempt with sent_ns null and error transport_error. Each gen.jsonl attempt with
// sent_ns null, a retry included, must be such a one: to instance 2, error transport_error,
// made while context.json's record of killCmd ran, t0 plus its time (attemptTime) in
// [start_ns, end_ns] give or take killUnsentMargin (t0: the time of hitgen's "sending" line in
// runs/<id>/hitgen.stderr, wall clock). The summary's unsent (planned − written) counts the
// entries whose first attempt was never written: hitgen counts an entry written by its first
// attempt alone (cmd/hitgen sender.go), so a refused retry is not in it, and unsent must equal
// the number of first attempts among those lines. A run whose connects all found hitapi 2 has
// none.
func checkKillUnsent(t testing.TB, r *orchRun, killCmd string, gen []orchGenRecord) {
	t.Helper()
	var rc struct {
		Faults []orchFaultRecord `json:"faults"`
	}
	readJSON(t, filepath.Join(r.dir, "context.json"), &rc)
	var kill *orchFaultRecord
	for i := range rc.Faults {
		if rc.Faults[i].Cmd == killCmd {
			kill = &rc.Faults[i]
		}
	}
	if kill == nil || kill.StartNs <= 0 || kill.EndNs < kill.StartNs {
		t.Fatalf("context.json faults %+v, want %q with its start_ns and end_ns", rc.Faults, killCmd)
	}
	t0 := hitgenT0(t, r)
	from, to := kill.StartNs-int64(killUnsentMargin), kill.EndNs+int64(killUnsentMargin)
	attempts := gen[:len(gen)-1]
	unsent, unsentFirst := 0, int64(0)
	var made []time.Duration // each unsent attempt's time, from the kill's start_ns
	for _, g := range attempts {
		if g.SentNs != nil {
			continue
		}
		unsent++
		if g.Attempt == 1 {
			unsentFirst++
		}
		off, err := attemptTime(attempts, g)
		if err != nil {
			t.Errorf("gen.jsonl attempt %d of %s (instance %d, error %s) was never written, and %v",
				g.Attempt, g.RequestID, g.Instance, nullable(g.Error), err)
			continue
		}
		at := t0 + off
		made = append(made, time.Duration(at-kill.StartNs))
		if g.Instance != 2 || g.Error == nil || *g.Error != "transport_error" || at < from || at > to {
			t.Errorf("gen.jsonl attempt %d of %s (instance %d, error %s) was never written, made at %d ns; want only attempts to instance 2 with error transport_error made while %q ran, in [%d, %d] ns",
				g.Attempt, g.RequestID, g.Instance, nullable(g.Error), at, killCmd, from, to)
		}
	}
	if s := gen[len(gen)-1]; s.Unsent == nil || *s.Unsent != unsentFirst {
		t.Errorf("gen.jsonl summary unsent %s, want %d: the first attempts with sent_ns null, those the kill left unsent (a retry never written is not counted)",
			nullable(s.Unsent), unsentFirst)
	}
	t.Logf("%q ran %v and left %d attempts unsent (%d first attempts), made %v after its start",
		killCmd, time.Duration(kill.EndNs-kill.StartNs), unsent, unsentFirst, made)
}

// attemptTime is when hitgen made attempt g, as an offset from its t0 (DESIGN §B4 "Sending",
// "Retries"): a first attempt at its planned_ns; a retry its hint_ms plus its jitter_ns after
// the attempt before it ended, at that attempt's sent_ns + latency, the moment hitgen's sender
// schedules a retry from (cmd/hitgen sender.go). planned_ns cannot time a retry: it is the
// entry's, earlier than the retry by at least the hint. The attempt before is the same entry's
// (same request ID and planned_ns; a duplicate entry reuses an earlier one's request ID),
// numbered one less, and must be written and answered 429; the error says what gen.jsonl lacks.
func attemptTime(attempts []orchGenRecord, g orchGenRecord) (int64, error) {
	if g.Attempt == 1 {
		return g.PlannedNs, nil
	}
	if g.HintMs == nil || g.JitterNs == nil {
		return 0, fmt.Errorf("the retry's hint_ms %s and jitter_ns %s: want both", nullable(g.HintMs), nullable(g.JitterNs))
	}
	for _, p := range attempts {
		if p.RequestID != g.RequestID || p.PlannedNs != g.PlannedNs || p.Attempt != g.Attempt-1 {
			continue
		}
		if p.Status != http.StatusTooManyRequests || p.SentNs == nil || p.Latency == nil {
			return 0, fmt.Errorf("its attempt %d is not a written attempt answered 429: status %d, sent_ns %s, latency %s",
				p.Attempt, p.Status, nullable(p.SentNs), nullable(p.Latency))
		}
		return *p.SentNs + *p.Latency + *g.HintMs*int64(time.Millisecond) + *g.JitterNs, nil
	}
	return 0, fmt.Errorf("gen.jsonl has no attempt %d of the same entry before it", g.Attempt-1)
}

// hitgenT0 is the wall-clock time, in ns, of hitgen's "sending" line in runs/<id>/hitgen.stderr:
// hitgen writes it just before its sender takes t0, from which gen.jsonl's offsets count.
func hitgenT0(t testing.TB, r *orchRun) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, "hitgen.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var rec struct {
			Time string `json:"time"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Msg != "sending" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, rec.Time)
		if err != nil {
			t.Fatalf("hitgen's sending line %s: %v", line, err)
		}
		return at.UnixNano()
	}
	t.Fatalf("runs/%s/hitgen.stderr has no sending line:\n%s", r.runID, data)
	return 0
}

// errRecorder is a testing.TB that keeps a checker's Errorf reports instead of failing the
// test (TestCheckKillUnsent); anything else, Fatalf included, reaches the real test.
type errRecorder struct {
	testing.TB
	errs []string
}

func (r *errRecorder) Helper() {}

func (r *errRecorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// TestCheckKillUnsent (IMPLEMENTATION_PLAN H4 step 7; DESIGN §B4 "Record", "Retries"):
// checkKillUnsent, TestDesign_OrchestratorFaultctl's unsent check, given a synthetic run
// directory (context.json's api kill from 1,000 to 1,120 ms after hitgen's t0, hitgen.stderr's
// "sending" line at t0) and gen.jsonl, accepts a retry the kill refused with the summary's
// unsent 0, as hitgen counts first attempts only: the retry is made at 1,062 ms (its first
// attempt, planned at 800 ms, answered 429 by 802 ms, plus hint_ms 250 and jitter_ns 10 ms),
// inside the window although its planned_ns is not; and with a first attempt refused at
// 1,100 ms besides, unsent 1. Each rule then fails alone, with one report: the summary leaving
// out that first attempt or counting the retry; a refused retry made after the kill and its
// margin, one to instance 1, one that timed out, one without a 429 before it.
func TestCheckKillUnsent(t *testing.T) {
	const killCmd = "faultctl api kill 2 --restart"
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ms := int64(time.Millisecond)
	ctxJSON, err := json.Marshal(map[string]any{"faults": []map[string]any{{
		"at_s": 1, "cmd": killCmd, "instance": 2, "start_ns": t0.UnixNano() + 1000*ms, "end_ns": t0.UnixNano() + 1120*ms,
		"outage_start_ns": nil, "outage_ns": nil, "targets": []any{}, "error": "",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	stderr := `{"time":"` + t0.Add(-time.Second).Format(time.RFC3339Nano) + `","level":"INFO","msg":"schedule ready"}` + "\n" +
		`{"time":"` + t0.Format(time.RFC3339Nano) + `","level":"INFO","msg":"sending"}` + "\n"
	// The written attempts every case has: a 200 on instance 1; on instance 2 and on instance
	// 1, a first attempt answered 429 (ended at 802 ms and 812 ms).
	written := []string{
		`{"kind":"initial","planned_ns":500000000,"sent_ns":500100000,"instance":1,"request_id":"r000000000","attempt":1,"hint_ms":null,"jitter_ns":null,"status":200,"error":null,"latency":2000000}`,
		`{"kind":"initial","planned_ns":800000000,"sent_ns":800200000,"instance":2,"request_id":"r000000001","attempt":1,"hint_ms":null,"jitter_ns":null,"status":429,"error":"rate_limited","latency":1800000}`,
		`{"kind":"initial","planned_ns":810000000,"sent_ns":810100000,"instance":1,"request_id":"r000000002","attempt":1,"hint_ms":null,"jitter_ns":null,"status":429,"error":"rate_limited","latency":1900000}`,
	}
	const (
		retryIn      = `{"kind":"retry","planned_ns":800000000,"sent_ns":null,"instance":2,"request_id":"r000000001","attempt":2,"hint_ms":250,"jitter_ns":10000000,"status":0,"error":"transport_error","latency":null}`
		firstIn      = `{"kind":"initial","planned_ns":1100000000,"sent_ns":null,"instance":2,"request_id":"r000000003","attempt":1,"hint_ms":null,"jitter_ns":null,"status":0,"error":"transport_error","latency":null}`
		retryLate    = `{"kind":"retry","planned_ns":800000000,"sent_ns":null,"instance":2,"request_id":"r000000001","attempt":2,"hint_ms":400,"jitter_ns":10000000,"status":0,"error":"transport_error","latency":null}`
		retryInst1   = `{"kind":"retry","planned_ns":810000000,"sent_ns":null,"instance":1,"request_id":"r000000002","attempt":2,"hint_ms":250,"jitter_ns":0,"status":0,"error":"transport_error","latency":null}`
		retryTimeout = `{"kind":"retry","planned_ns":800000000,"sent_ns":null,"instance":2,"request_id":"r000000001","attempt":2,"hint_ms":250,"jitter_ns":10000000,"status":0,"error":"timeout","latency":null}`
		retryNo429   = `{"kind":"retry","planned_ns":500000000,"sent_ns":null,"instance":2,"request_id":"r000000000","attempt":2,"hint_ms":500,"jitter_ns":10000000,"status":0,"error":"transport_error","latency":null}`
	)
	for _, c := range []struct {
		name    string
		unsent  []string // the attempts never written
		summary int      // the summary's unsent
		reports int
	}{
		{"a retry refused in the kill", []string{retryIn}, 0, 0},
		{"and a first attempt refused in the kill", []string{retryIn, firstIn}, 1, 0},
		{"the summary leaving out the first attempt", []string{retryIn, firstIn}, 0, 1},
		{"the summary counting the retry", []string{retryIn}, 1, 1},
		{"a retry made after the kill", []string{retryLate}, 0, 1},
		{"a retry to instance 1", []string{retryInst1}, 0, 1},
		{"a retry that timed out", []string{retryTimeout}, 0, 1},
		{"a retry without a 429 before it", []string{retryNo429}, 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			lines := append(slices.Clone(written), c.unsent...)
			lines = append(lines, fmt.Sprintf(`{"kind":"summary","unsent":%d,"log_drops":0}`, c.summary))
			for name, data := range map[string]string{
				"context.json":  string(ctxJSON),
				"hitgen.stderr": stderr,
				"gen.jsonl":     strings.Join(lines, "\n") + "\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			rec := &errRecorder{TB: t}
			checkKillUnsent(rec, &orchRun{runID: "unit", dir: dir}, killCmd, readJSONLines[orchGenRecord](t, filepath.Join(dir, "gen.jsonl")))
			if len(rec.errs) != c.reports {
				t.Errorf("%d reports, want %d: %q", len(rec.errs), c.reports, rec.errs)
			}
		})
	}
}

// faultSample is the part of a samples.jsonl line TestDesign_OrchestratorFaultctl reads: each
// process pids.json named at that tick, with its PID (DESIGN §B7).
type faultSample struct {
	Procs []struct {
		Role  string  `json:"role"`
		PID   int     `json:"pid"`
		RSS   *int64  `json:"rss"`
		Error *string `json:"error"`
	} `json:"procs"`
}

// TestDesign_OrchestratorStorePause (IMPLEMENTATION_PLAN H4 step 7 and Watch out; DESIGN §B6
// FLT-2, §B7): bin/test/orchestrator runs testdata/orch-pause.toml, whose fault runs faultctl
// store pause 4.5s inside the orchestrator at 2 s, and the run and its sampler outlive the
// paused store. The run finishes, exit 0, with a finished run's evidence (checkFinishedRun:
// the sampler exited 0 after its SIGTERM, among the rest). context.json records the pause
// without an error, no instance, the command's own start_ns and end_ns, and an outage_ns of
// more than the commanded 4,500 ms, at most 5,000 ms, from its outage_start_ns, inside
// start_ns and end_ns within the wall clock's adjustment meanwhile (checkOutageBounds); the
// store was not restarted (pids.json names the process the "store started" line named).
// samples.jsonl goes on through the pause: a line whose tick started inside the outage (from
// outage_start_ns for outage_ns) shows the store's read failing (store_error), and a later
// line reads the store again.
func TestDesign_OrchestratorStorePause(t *testing.T) {
	const (
		pauseCmd = "faultctl store pause 4.5s"
		pauseFor = 4500 * time.Millisecond // its commanded duration
	)
	db := insertSentinel(t)
	before := time.Now()
	r := runOrchestrator(t, "--scenario", testdataPath(t, "orch-pause.toml"))
	adj := clockAdjustment(before, time.Now())
	checkFinishedRun(t, r, db, "orch-pause", false, 1, nil)

	var rc struct {
		Faults []orchFaultRecord `json:"faults"`
	}
	readJSON(t, filepath.Join(r.dir, "context.json"), &rc)
	if len(rc.Faults) != 1 {
		t.Fatalf("context.json faults %+v, want the one [[faults]] entry", rc.Faults)
	}
	pause := rc.Faults[0]
	if pause.Cmd != pauseCmd || pause.AtS != 2 || pause.Error != "" || pause.Instance != nil || pause.OutageNs == nil || pause.OutageStartNs == nil ||
		*pause.OutageNs <= int64(pauseFor) || *pause.OutageNs > int64(5*time.Second) || pause.EndNs <= pause.StartNs {
		t.Fatalf("the store pause's record %+v (outage_ns %s), want at_s 2, no instance, an outage_ns of more than 4,500 ms, at most 5,000 ms, "+
			"between start_ns and end_ns, no error", pause, nullable(pause.OutageNs))
	}
	checkOutageBounds(t, pause, pauseFor, adj)
	outageFrom, outageTo := *pause.OutageStartNs, *pause.OutageStartNs+*pause.OutageNs

	pids, err := runlog.ReadPIDs(filepath.Join(r.dir, runlog.PIDsFile))
	if err != nil {
		t.Fatal(err)
	}
	var started float64
	for _, l := range r.recs {
		if l["msg"] == "store started" {
			started, _ = l["pid"].(float64)
		}
	}
	for _, p := range pids.Procs {
		if p.Role == "valkey-server" && float64(p.PID) != started {
			t.Errorf("pids.json names the store as process %d, the \"store started\" line %v: want the same process, not restarted", p.PID, started)
		}
	}

	type sample struct {
		TNs        int64   `json:"t_ns"`
		StoreError *string `json:"store_error"`
		Store      *struct {
			Persistence map[string]string `json:"persistence"`
		} `json:"store"`
	}
	failedInside, readAfter := false, false
	for _, s := range readJSONLines[sample](t, filepath.Join(r.dir, "samples.jsonl")) {
		switch {
		case s.TNs >= outageFrom && s.TNs < outageTo && s.StoreError != nil:
			failedInside = true
		case s.TNs > outageTo && s.StoreError == nil && s.Store != nil && len(s.Store.Persistence) > 0:
			readAfter = true
		}
	}
	if !failedInside || !readAfter {
		t.Errorf("samples.jsonl: a store read failing in a tick inside the pause %v, the store read again after it %v; want both: the sampler outlives the paused store",
			failedInside, readAfter)
	}
}
