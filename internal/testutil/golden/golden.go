// Package golden checks written run-log lines against hand-written golden lines (DESIGN §C5,
// IMPLEMENTATION_PLAN H1 step 8): a written line pairs with the golden line that has the same
// values for the test's exact keys (by position when it names none); paired lines must have
// the same keys at every level and the same JSON type for each value (arrays element by
// element, of equal length); other values may differ. It is a test helper: production code
// never imports it.
package golden

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// line is one parsed JSON line and where it came from.
type line struct {
	n     int // 1-based: the golden file's line number, or the written line's index + 1
	value any
}

// Match checks every written line in got (one JSON value each; a trailing newline is
// allowed) against the golden file. With exact keys, a written line pairs with the one
// golden line whose top-level values for every exact key equal its own (a key absent from
// both counts as equal); with none, written line i pairs with golden line i. A written line
// with no partner, or with two or more, fails, and so does a paired line whose shape differs.
// Golden lines without a written partner are fine: one golden file serves several tests.
// Every failure is reported through t.Errorf.
func Match(t testing.TB, got [][]byte, goldenFile string, exact ...string) {
	t.Helper()
	data, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Errorf("golden: reading %s: %v", goldenFile, err)
		return
	}
	var golden []line
	for i, raw := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		v, err := parse(raw)
		if err != nil {
			t.Errorf("golden: %s line %d is not one JSON value: %v", goldenFile, i+1, err)
			return
		}
		golden = append(golden, line{n: i + 1, value: v})
	}
	for i, raw := range got {
		v, err := parse(raw)
		if err != nil {
			t.Errorf("golden: written line %d is not one JSON value (%v): %q", i+1, err, raw)
			continue
		}
		partners := pair(golden, i, v, exact)
		switch len(partners) {
		case 0:
			t.Errorf("golden: written line %d pairs with no line of %s (%s): %s",
				i+1, goldenFile, pairingRule(v, exact), raw)
			continue
		case 1:
		default:
			nums := make([]string, len(partners))
			for j, p := range partners {
				nums[j] = fmt.Sprint(p.n)
			}
			t.Errorf("golden: written line %d pairs with %d lines of %s (lines %s; %s): %s",
				i+1, len(partners), goldenFile, strings.Join(nums, ", "), pairingRule(v, exact), raw)
			continue
		}
		var diffs []string
		shape(&diffs, "", partners[0].value, v)
		for _, d := range diffs {
			t.Errorf("golden: written line %d differs from %s line %d: %s", i+1, goldenFile, partners[0].n, d)
		}
	}
}

// parse decodes one JSON value, keeping numbers as json.Number so their text survives.
func parse(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

// pair returns the golden lines written line i (value v) pairs with.
func pair(golden []line, i int, v any, exact []string) []line {
	if len(exact) == 0 {
		if i < len(golden) {
			return golden[i : i+1]
		}
		return nil
	}
	var out []line
	for _, g := range golden {
		same := true
		for _, k := range exact {
			gv, gok := field(g.value, k)
			wv, wok := field(v, k)
			if gok != wok || !reflect.DeepEqual(gv, wv) {
				same = false
				break
			}
		}
		if same {
			out = append(out, g)
		}
	}
	return out
}

// field returns v's top-level key k, and whether v is an object that has it.
func field(v any, k string) (any, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	fv, ok := obj[k]
	return fv, ok
}

// pairingRule describes how a written line looks for its partner, for failure messages.
func pairingRule(v any, exact []string) string {
	if len(exact) == 0 {
		return "by position"
	}
	parts := make([]string, len(exact))
	for i, k := range exact {
		fv, ok := field(v, k)
		if !ok {
			parts[i] = k + " absent"
			continue
		}
		b, err := json.Marshal(fv)
		if err != nil {
			parts[i] = fmt.Sprintf("%s=%v", k, fv)
			continue
		}
		parts[i] = k + "=" + string(b)
	}
	return "exact keys " + strings.Join(parts, ", ")
}

// shape appends to diffs every place where w's structure differs from g's: an object key
// missing on either side, a value of another JSON type, an array of another length. Values
// of the same type may differ.
func shape(diffs *[]string, path string, g, w any) {
	if gt, wt := kind(g), kind(w); gt != wt {
		*diffs = append(*diffs, fmt.Sprintf("%s is %s, golden has %s", where(path), wt, gt))
		return
	}
	switch gv := g.(type) {
	case map[string]any:
		wv, ok := w.(map[string]any)
		if !ok {
			return
		}
		for _, k := range sortedKeys(gv) {
			if _, ok := wv[k]; !ok {
				*diffs = append(*diffs, fmt.Sprintf("%s lacks key %q", where(path), k))
			}
		}
		for _, k := range sortedKeys(wv) {
			if _, ok := gv[k]; !ok {
				*diffs = append(*diffs, fmt.Sprintf("%s has extra key %q", where(path), k))
			}
		}
		for _, k := range sortedKeys(gv) {
			if wk, ok := wv[k]; ok {
				shape(diffs, path+"."+k, gv[k], wk)
			}
		}
	case []any:
		wv, ok := w.([]any)
		if !ok {
			return
		}
		if len(gv) != len(wv) {
			*diffs = append(*diffs, fmt.Sprintf("%s has %d elements, golden has %d", where(path), len(wv), len(gv)))
			return
		}
		for i := range gv {
			shape(diffs, fmt.Sprintf("%s[%d]", path, i), gv[i], wv[i])
		}
	}
}

// kind names a decoded value's JSON type.
func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a bool"
	case json.Number:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

// where names a path for messages: the line itself, or a key path such as .a.b[0].
func where(path string) string {
	if path == "" {
		return "the line"
	}
	return path
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
