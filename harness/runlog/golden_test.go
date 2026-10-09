package runlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestGoldenAPI_OneRealRequest checks the hand-written golden api.jsonl against DESIGN §B8 and
// IMPLEMENTATION_PLAN H1 step 8, not against any writer: one line per kind and event, each
// with exactly the §B8 fields, and the four request lines fitting one real request (an
// allowed request whose insert failed with no row, its release and its applied refund).
func TestGoldenAPI_OneRealRequest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "api.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	type rec = map[string]any
	byName := map[string]rec{}
	for i, raw := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var r rec
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("line %d is not a JSON object: %v", i+1, err)
		}
		name, _ := r["kind"].(string)
		if ev, ok := r["event"].(string); ok {
			name += "/" + ev
		}
		if _, dup := byName[name]; dup {
			t.Errorf("line %d: a second %s line", i+1, name)
		}
		byName[name] = r
	}

	fields := map[string][]string{
		"decision": {"deduction_id", "user_id", "decision_ns", "result"},
		"request": {"run_id", "epoch_id", "instance", "user_id", "request_id", "arrival_ns", "decision_ns",
			"deduction_id", "result", "existed", "http_status", "error", "limiter_ns", "db_ns", "total_ns"},
		"release":         {"deduction_id", "release_mono_ns"},
		"refund":          {"deduction_id", "result", "reason", "attempts", "credit_decision_ns", "release_mono_ns", "final_mono_ns"},
		"lifecycle/start": {"event", "max_in_flight", "queue_len", "max_release_delay_ns", "dependency_deadline_ns", "drain_timeout_ns", "nofile_limit"},
		"lifecycle/ready": {"event"},
		"lifecycle/stop":  {"event", "listener_drops", "log_drops"},
	}
	for name, want := range fields {
		r, ok := byName[name]
		if !ok {
			t.Errorf("no %s line", name)
			continue
		}
		want = append([]string{"kind"}, want...)
		sort.Strings(want)
		got := make([]string, 0, len(r))
		for k := range r {
			got = append(got, k)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s line has keys %v, want %v", name, got, want)
		}
	}
	if t.Failed() {
		return
	}

	num := func(name, key string) int64 {
		n, err := byName[name][key].(json.Number).Int64()
		if err != nil {
			t.Fatalf("%s %s: %v", name, key, err)
		}
		return n
	}
	str := func(name, key string) string { return byName[name][key].(string) }
	checks := []struct {
		what string
		ok   bool
	}{
		{"decision result allowed", str("decision", "result") == "allowed"},
		{"request: allowed, 503 database_unavailable, existed false", str("request", "result") == "allowed" &&
			num("request", "http_status") == 503 && str("request", "error") == "database_unavailable" &&
			byName["request"]["existed"] == false},
		{"refund applied with no reason", str("refund", "result") == "applied" && str("refund", "reason") == ""},
		{"one user_id", str("decision", "user_id") == str("request", "user_id")},
		{"one deduction_id", str("decision", "deduction_id") == str("request", "deduction_id") &&
			str("decision", "deduction_id") == str("release", "deduction_id") &&
			str("decision", "deduction_id") == str("refund", "deduction_id")},
		{"request decision_ns is the decision's", num("request", "decision_ns") == num("decision", "decision_ns")},
		{"budget times are whole µs", num("decision", "decision_ns")%1000 == 0 && num("refund", "credit_decision_ns")%1000 == 0},
		{"credit_decision_ns after decision_ns", num("refund", "credit_decision_ns") > num("decision", "decision_ns")},
		{"refund final after its release", num("refund", "final_mono_ns") > num("refund", "release_mono_ns")},
		{"total_ns covers limiter_ns + db_ns", num("request", "total_ns") >= num("request", "limiter_ns")+num("request", "db_ns")},
		{"start: queue length 0, nofile ≥ 1,024", num("lifecycle/start", "queue_len") == 0 && num("lifecycle/start", "nofile_limit") >= 1024},
		{"stop: drop counts 0", num("lifecycle/stop", "listener_drops") == 0 && num("lifecycle/stop", "log_drops") == 0},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("golden api.jsonl: want %s", c.what)
		}
	}
}

// TestGoldenGen_AttemptsAndSummary checks the hand-written golden gen.jsonl against DESIGN §B4
// "Record", §B8 and IMPLEMENTATION_PLAN H2 step 6, not against any writer: an initial attempt
// answered 200, an initial attempt answered 429, its retry (hint_source Retry-After) answered
// 200, an initial attempt that failed in transport (status 0, transport_error), in the order
// they finish, then the summary line. Each attempt line has exactly the §B4 fields and kind,
// null where a field does not apply (latency, monotonic ns, is a number on every attempt that
// was written, the transport failure's included: it runs to the failure); the summary has
// exactly the plan's fields; and the lines fit one real run: the retry repeats its 429's IDs,
// instance and planned time, waits at least hint + jitter after the 429's response, and the
// summary counts the three initial attempts.
func TestGoldenGen_AttemptsAndSummary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "gen.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	type rec = map[string]any
	var lines []rec
	for i, raw := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var r rec
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("line %d is not a JSON object: %v", i+1, err)
		}
		lines = append(lines, r)
	}
	if len(lines) != 5 {
		t.Fatalf("golden gen.jsonl has %d lines, want 4 attempts and the summary", len(lines))
	}
	keysOf := func(r rec) []string {
		ks := make([]string, 0, len(r))
		for k := range r {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	sorted := func(ks ...string) []string {
		sort.Strings(ks)
		return ks
	}
	attemptKeys := sorted("kind", "planned_ns", "sent_ns", "got_conn_wait_ns", "instance", "user", "request_id",
		"duplicate_of", "attempt", "hint_source", "hint_ms", "jitter_ns", "status", "error", "latency", "arrival_ns", "decision_ns")
	summaryKeys := sorted("kind", "planned", "launched", "unsent", "log_drops", "stopped_after_step", "excluded_step",
		"skipped_no_eligible")
	num := func(r rec, key string) int64 {
		t.Helper()
		v, ok := r[key].(json.Number)
		if !ok {
			t.Fatalf("%v: %s = %v, want a number", r["kind"], key, r[key])
		}
		n, err := v.Int64()
		if err != nil {
			t.Fatalf("%v: %s: %v", r["kind"], key, err)
		}
		return n
	}

	// The attempts by outcome, in the order they finish: the transport failure (planned
	// before the retry was due) ends long before the retry, which waits about a second.
	ok200, limited, failed, retry, summary := lines[0], lines[1], lines[2], lines[3], lines[4]
	for i, r := range lines[:4] {
		if got := keysOf(r); !reflect.DeepEqual(got, attemptKeys) {
			t.Errorf("attempt line %d has keys %v, want %v", i+1, got, attemptKeys)
		}
	}
	if got := keysOf(summary); !reflect.DeepEqual(got, summaryKeys) {
		t.Errorf("the summary line has keys %v, want %v", got, summaryKeys)
	}
	if t.Failed() {
		return
	}
	for _, c := range []struct {
		name          string
		r             rec
		kind          string
		status        int64
		errCode       any  // the error field: a code, or nil for none
		written       bool // sent_ns set: latency is a number, to the response or the failure
		hasArrival    bool
		hasDecision   bool
		attempt       int64
		hintSource    any
		hintMs, jitNs bool
	}{
		{"200", ok200, "initial", 200, nil, true, true, true, 1, nil, false, false},
		{"429", limited, "initial", 429, "rate_limited", true, false, true, 1, nil, false, false},
		{"transport failure", failed, "initial", 0, "transport_error", true, false, false, 1, nil, false, false},
		{"retry", retry, "retry", 200, nil, true, true, true, 2, "Retry-After", true, true},
	} {
		r := c.r
		if r["kind"] != c.kind || num(r, "status") != c.status || r["error"] != c.errCode || num(r, "attempt") != c.attempt ||
			r["hint_source"] != c.hintSource || r["duplicate_of"] != nil {
			t.Errorf("%s line %v: want kind %s, status %d, error %v, attempt %d, hint_source %v, duplicate_of null",
				c.name, r, c.kind, c.status, c.errCode, c.attempt, c.hintSource)
		}
		for key, present := range map[string]bool{
			"latency": c.written, "arrival_ns": c.hasArrival, "decision_ns": c.hasDecision,
			"hint_ms": c.hintMs, "jitter_ns": c.jitNs,
		} {
			if _, isNum := r[key].(json.Number); isNum != present || (!present && r[key] != nil) {
				t.Errorf("%s line: %s = %v, want a number: %v (else null)", c.name, key, r[key], present)
			}
		}
		// Request ID r%09d from the entry index i, instance i mod 3 + 1 (TRF-4), user u%06d.
		var i int64
		if _, err := fmt.Sscanf(r["request_id"].(string), "r%09d", &i); err != nil {
			t.Errorf("%s line: request_id %v: %v", c.name, r["request_id"], err)
		}
		if num(r, "instance") != i%3+1 || !regexp.MustCompile(`^u\d{6}$`).MatchString(r["user"].(string)) {
			t.Errorf("%s line: instance %v and user %v for entry %d, want instance %d and a u%%06d user",
				c.name, r["instance"], r["user"], i, i%3+1)
		}
		if num(r, "sent_ns") < num(r, "planned_ns") || num(r, "got_conn_wait_ns") < 0 {
			t.Errorf("%s line: sent_ns before planned_ns, or a negative got_conn_wait_ns", c.name)
		}
		if c.hasDecision && num(r, "decision_ns")%1000 != 0 {
			t.Errorf("%s line: decision_ns %d, want whole µs (budget time)", c.name, num(r, "decision_ns"))
		}
		if c.hasArrival && c.hasDecision && num(r, "decision_ns") <= num(r, "arrival_ns") {
			t.Errorf("%s line: decision_ns before arrival_ns", c.name)
		}
	}
	// The retry: the 429's operation again (TRF-5), after max(Retry-After 1 s, retry_after_ms
	// 200) = 1,000 ms from Retry-After, plus jitter in [0, 50 ms], from the 429's response.
	for _, key := range []string{"user", "request_id", "instance", "planned_ns"} {
		if retry[key] != limited[key] {
			t.Errorf("the retry's %s = %v, want the 429's %v", key, retry[key], limited[key])
		}
	}
	jitter := num(retry, "jitter_ns")
	if num(retry, "hint_ms") != 1000 || jitter < 0 || jitter > 50_000_000 {
		t.Errorf("the retry's hint_ms %d and jitter_ns %d, want 1,000 and 0 to 50,000,000", num(retry, "hint_ms"), jitter)
	}
	if due := num(limited, "sent_ns") + num(limited, "latency") + num(retry, "hint_ms")*1_000_000 + jitter; num(retry, "sent_ns") < due {
		t.Errorf("the retry was sent at %d ns, want at least %d: the 429's response + hint + jitter", num(retry, "sent_ns"), due)
	}
	if ok200["request_id"] == limited["request_id"] || limited["request_id"] == failed["request_id"] {
		t.Error("the three initial attempts share a request ID, want three operations")
	}
	// The summary (PRF-1): three planned operations, each launched and written (sent_ns), the
	// run not stopped, nothing dropped or skipped.
	if num(summary, "planned") != 3 || num(summary, "launched") != 3 || num(summary, "unsent") != 0 ||
		num(summary, "log_drops") != 0 || num(summary, "skipped_no_eligible") != 0 ||
		summary["stopped_after_step"] != nil || summary["excluded_step"] != nil {
		t.Errorf("summary %v: want planned 3, launched 3, unsent 0, log_drops 0, skipped_no_eligible 0, "+
			"stopped_after_step and excluded_step null", summary)
	}
}

// TestGoldenSamples_TwoTicks checks the hand-written golden samples.jsonl against DESIGN §B7,
// §B8 and IMPLEMENTATION_PLAN H3 step 7, not against any writer: the two ticks TestSampler_Tick
// produces. Each line has exactly the sample fields (each read's value beside its error); the
// one process of its pids.json with cpu and rss; one instance with its /metrics series (PRF-4's
// gauges, listener drops and cleanup counts among them) and, on the first tick, hitapi's
// /health body (DESIGN §A5.11 Health plus plan H1 step 9's counters, as a benchmark build
// serves it); and an error with a null value for each read the test leaves unavailable:
// rladmin status, INFO and the postmaster. The second tick is the same with a /health error.
func TestGoldenSamples_TwoTicks(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	type rec = map[string]any
	var lines []rec
	for i, raw := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var r rec
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("line %d is not a JSON object: %v", i+1, err)
		}
		lines = append(lines, r)
	}
	if len(lines) != 2 {
		t.Fatalf("golden samples.jsonl has %d lines, want the 2 ticks of TestSampler_Tick", len(lines))
	}
	keysOf := func(v any) []string {
		r, _ := v.(rec)
		ks := make([]string, 0, len(r))
		for k := range r {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	sorted := func(ks ...string) []string {
		sort.Strings(ks)
		return ks
	}
	isNum := func(v any) bool { _, ok := v.(json.Number); return ok }
	isText := func(v any) bool { s, ok := v.(string); return ok && s != "" }
	lineKeys := sorted("t_ns", "procs", "procs_error", "postgres", "postgres_error", "apis", "status", "status_error",
		"store", "store_error")
	procKeys := sorted("role", "instance", "pid", "cpu", "rss", "error")
	apiKeys := sorted("instance", "addr", "health", "health_error", "metrics", "metrics_error")
	healthKeys := sorted("status", "reason", "epoch_id", "clock_source", "instance", "listener_drops", "store_error",
		"fixtures", "log_drops", "in_flight", "nofile_limit", "log_queue_len")
	prf4 := []string{"rl_tracked_buckets", "rl_live_markers", "rl_quarantined_keys", "rl_pending_refunds",
		"rl_cleanup_lag_seconds", "rl_clock_lag_microseconds", "rl_listener_drops_total", "rl_cleanup_passes_total",
		"rl_cleanup_entries_total"}
	// PRF-4's labelled series, the cleanup counts, by their label (DESIGN §A5.11): one series
	// with that label at least. Each other PRF-4 metric is one series without labels.
	prf4Labels := map[string]string{"rl_cleanup_passes_total": "status", "rl_cleanup_entries_total": "action"}
	hasSeries := func(m rec, name string) bool {
		label, labelled := prf4Labels[name]
		if !labelled {
			_, ok := m[name]
			return ok
		}
		for k := range m {
			if strings.HasPrefix(k, name+"{"+label+`="`) && strings.HasSuffix(k, `"}`) {
				return true
			}
		}
		return false
	}

	var apis [2]rec
	for i, l := range lines {
		tick := i + 1
		if got := keysOf(l); !reflect.DeepEqual(got, lineKeys) {
			t.Fatalf("tick %d has keys %v, want %v", tick, got, lineKeys)
		}
		if !isNum(l["t_ns"]) || l["procs_error"] != nil {
			t.Errorf("tick %d: t_ns %v and procs_error %v, want wall-clock ns and null", tick, l["t_ns"], l["procs_error"])
		}
		procs, _ := l["procs"].([]any)
		if len(procs) != 1 {
			t.Fatalf("tick %d: procs %v, want its pids.json's one process", tick, l["procs"])
		}
		p, _ := procs[0].(rec)
		if got := keysOf(p); !reflect.DeepEqual(got, procKeys) || p["role"] != "hitapi" || p["instance"] != json.Number("1") ||
			!isNum(p["pid"]) || !isNum(p["cpu"]) || !isNum(p["rss"]) || p["error"] != nil {
			t.Errorf("tick %d: process %v, want hitapi 1 with a pid, cpu, rss and a null error", tick, p)
		}
		for _, read := range []string{"postgres", "status", "store"} {
			if l[read] != nil || !isText(l[read+"_error"]) {
				t.Errorf("tick %d: %s %v with error %v, want null and the unavailable read's error", tick, read, l[read], l[read+"_error"])
			}
		}
		list, _ := l["apis"].([]any)
		if len(list) != 1 {
			t.Fatalf("tick %d: apis %v, want one instance", tick, l["apis"])
		}
		apis[i], _ = list[0].(rec)
		a := apis[i]
		if got := keysOf(a); !reflect.DeepEqual(got, apiKeys) || a["instance"] != json.Number("1") || !isText(a["addr"]) ||
			a["metrics_error"] != nil {
			t.Errorf("tick %d: instance %v, want instance 1 with its address and a null metrics_error", tick, a)
		}
		m, _ := a["metrics"].(rec)
		for k, v := range m {
			if !isNum(v) {
				t.Errorf("tick %d: metrics %s = %v, want a number", tick, k, v)
			}
		}
		for _, k := range prf4 {
			if !hasSeries(m, k) {
				t.Errorf("tick %d: metrics lack %s (PRF-4)", tick, k)
			}
		}
	}
	if got := keysOf(apis[0]["health"]); !reflect.DeepEqual(got, healthKeys) || apis[0]["health_error"] != nil {
		t.Errorf("tick 1: health keys %v with error %v, want %v and null", got, apis[0]["health_error"], healthKeys)
	}
	if apis[1]["health"] != nil || !isText(apis[1]["health_error"]) {
		t.Errorf("tick 2: health %v with error %v, want null and /health's error", apis[1]["health"], apis[1]["health_error"])
	}
	if !reflect.DeepEqual(keysOf(apis[0]["metrics"]), keysOf(apis[1]["metrics"])) {
		t.Error("the two ticks' metrics have different series, want the same stub")
	}
	t1, _ := lines[0]["t_ns"].(json.Number).Int64()
	t2, _ := lines[1]["t_ns"].(json.Number).Int64()
	if t2 <= t1 {
		t.Errorf("t_ns %d then %d, want the second tick later", t1, t2)
	}
}
