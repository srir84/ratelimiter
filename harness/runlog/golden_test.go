package runlog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
