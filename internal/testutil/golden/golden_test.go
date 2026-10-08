package golden

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recorder is a testing.TB that keeps Match's failures instead of failing the test. Match
// calls only Helper and Errorf; anything else would reach the real test.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// goldenLines is the golden file of most cases.
var goldenLines = []string{
	`{"kind":"decision","user_id":"alice","decision_ns":1000,"result":"allowed"}`,
	`{"kind":"request","error":"database_unavailable","http_status":503,"existed":false,"meta":{"a":1,"b":[1,2]},"list":[{"x":1},{"y":"s"}]}`,
	`{"kind":"lifecycle","event":"stop","listener_drops":0,"log_drops":0}`,
	`{"kind":"dup","n":1}`,
	`{"kind":"dup","n":2}`,
}

func writeGolden(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "golden.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func toBytes(lines []string) [][]byte {
	out := make([][]byte, len(lines))
	for i, l := range lines {
		out[i] = []byte(l)
	}
	return out
}

func TestMatch(t *testing.T) {
	decision := goldenLines[0]
	request := goldenLines[1]
	cases := []struct {
		name  string
		got   []string
		exact []string
		fail  bool
	}{
		// Passing: values of the same type may differ, except the exact keys.
		{"identical lines", []string{decision, request, goldenLines[2]}, []string{"kind"}, false},
		{"changed number", []string{`{"kind":"decision","user_id":"alice","decision_ns":2000,"result":"allowed"}`}, []string{"kind"}, false},
		{"changed other string", []string{`{"kind":"decision","user_id":"bob","decision_ns":1000,"result":"limited"}`}, []string{"kind"}, false},
		{"changed nested values", []string{`{"kind":"request","error":"x","http_status":200,"existed":true,"meta":{"a":7,"b":[3,4]},"list":[{"x":9},{"y":"t"}]}`}, []string{"kind"}, false},
		{"key order and trailing newline", []string{`{"result":"allowed","decision_ns":1,"user_id":"u","kind":"decision"}` + "\n"}, []string{"kind"}, false},

		// Failing: the shape differs.
		{"renamed key", []string{`{"kind":"decision","user":"alice","decision_ns":1000,"result":"allowed"}`}, []string{"kind"}, true},
		{"missing key", []string{`{"kind":"decision","user_id":"alice","decision_ns":1000}`}, []string{"kind"}, true},
		{"extra key", []string{`{"kind":"decision","user_id":"alice","decision_ns":1000,"result":"allowed","extra":1}`}, []string{"kind"}, true},
		{"nested renamed key", []string{strings.Replace(request, `"b":[1,2]`, `"c":[1,2]`, 1)}, []string{"kind"}, true},
		{"nested missing key", []string{strings.Replace(request, `,"b":[1,2]`, ``, 1)}, []string{"kind"}, true},
		{"nested extra key", []string{strings.Replace(request, `"b":[1,2]`, `"b":[1,2],"c":0`, 1)}, []string{"kind"}, true},
		{"key renamed inside an array element", []string{strings.Replace(request, `{"y":"s"}`, `{"z":"s"}`, 1)}, []string{"kind"}, true},
		{"string for a number", []string{`{"kind":"decision","user_id":"alice","decision_ns":"1000","result":"allowed"}`}, []string{"kind"}, true},
		{"null for a bool", []string{strings.Replace(request, `"existed":false`, `"existed":null`, 1)}, []string{"kind"}, true},
		{"bool for a number", []string{strings.Replace(request, `"http_status":503`, `"http_status":true`, 1)}, []string{"kind"}, true},
		{"array for an object", []string{strings.Replace(request, `"meta":{"a":1,"b":[1,2]}`, `"meta":[1,2]`, 1)}, []string{"kind"}, true},
		{"number for a string in an array", []string{strings.Replace(request, `{"y":"s"}`, `{"y":5}`, 1)}, []string{"kind"}, true},
		{"shorter nested array", []string{strings.Replace(request, `"b":[1,2]`, `"b":[1]`, 1)}, []string{"kind"}, true},
		{"longer array", []string{strings.Replace(request, `{"y":"s"}]`, `{"y":"s"},{"x":2}]`, 1)}, []string{"kind"}, true},
		{"not an object", []string{`["kind","decision"]`}, []string{"kind"}, true},

		// Failing: the pairing.
		{"changed exact-key value", []string{`{"kind":"decisions","user_id":"alice","decision_ns":1000,"result":"allowed"}`}, []string{"kind"}, true},
		{"changed second exact key", []string{`{"kind":"decision","user_id":"alice","decision_ns":1000,"result":"limited"}`}, []string{"kind", "result"}, true},
		{"two golden lines pair", []string{`{"kind":"dup","n":1}`}, []string{"kind"}, true},
		{"exact key absent from the golden line", []string{`{"kind":"lifecycle","event":"stop","listener_drops":0,"log_drops":0}`, `{"kind":"decision","event":"x","user_id":"alice","decision_ns":1000,"result":"allowed"}`}, []string{"kind", "event"}, true},
		{"not JSON", []string{`{"kind":`}, []string{"kind"}, true},
		{"two JSON values", []string{decision + decision}, []string{"kind"}, true},
		{"empty line", []string{""}, []string{"kind"}, true},

		// No exact keys: lines pair by position.
		{"by position", []string{`{"kind":"other","user_id":"x","decision_ns":5,"result":"y"}`, request}, nil, false},
		{"by position, swapped", []string{request, decision}, nil, true},
		{"by position, more written lines than golden", append(append([]string{}, goldenLines...), decision), nil, true},
	}
	path := writeGolden(t, goldenLines)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &recorder{TB: t}
			Match(r, toBytes(c.got), path, c.exact...)
			switch {
			case c.fail && len(r.errs) == 0:
				t.Errorf("Match passed, want a failure")
			case !c.fail && len(r.errs) > 0:
				t.Errorf("Match failed, want a pass:\n%s", strings.Join(r.errs, "\n"))
			}
		})
	}

	t.Run("an exact key absent from both pairs", func(t *testing.T) {
		r := &recorder{TB: t}
		Match(r, toBytes([]string{decision}), path, "kind", "event")
		if len(r.errs) > 0 {
			t.Errorf("Match failed, want the decision line to pair with the golden one (neither has event):\n%s",
				strings.Join(r.errs, "\n"))
		}
	})
	t.Run("every bad line fails on its own", func(t *testing.T) {
		r := &recorder{TB: t}
		Match(r, toBytes([]string{`{"kind":"nope"}`, decision, `{"kind":"dup","n":3}`}), path, "kind")
		if len(r.errs) != 2 {
			t.Errorf("%d failures, want 2 (no partner, two partners):\n%s", len(r.errs), strings.Join(r.errs, "\n"))
		}
	})
	t.Run("missing golden file", func(t *testing.T) {
		r := &recorder{TB: t}
		Match(r, toBytes([]string{decision}), filepath.Join(t.TempDir(), "absent.jsonl"), "kind")
		if len(r.errs) == 0 {
			t.Error("Match passed with no golden file")
		}
	})
	t.Run("golden line that is not JSON", func(t *testing.T) {
		r := &recorder{TB: t}
		Match(r, toBytes([]string{decision}), writeGolden(t, []string{decision, `{"kind":`}), "kind")
		if len(r.errs) == 0 {
			t.Error("Match passed with a broken golden line")
		}
	})
}
