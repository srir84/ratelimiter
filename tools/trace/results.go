package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// testOutcome collects every result one test got, across all result files.
type testOutcome struct {
	pass, fail, skip bool
}

// junitCase is one JUnit <testcase> as pytest writes it (--junitxml).
type junitCase struct {
	Classname string     `xml:"classname,attr"`
	Name      string     `xml:"name,attr"`
	File      string     `xml:"file,attr"`
	Failures  []junitMsg `xml:"failure"`
	Errors    []junitMsg `xml:"error"`
	Skipped   []junitMsg `xml:"skipped"`
}

type junitMsg struct {
	Message string `xml:"message,attr"`
}

// testResults holds the test results of a results directory: go test -json streams
// (*.json) and JUnit files (*.xml).
type testResults struct {
	goTests map[string]*testOutcome // package + " " + test (subtests as Name/Sub)
	goRan   map[string]bool         // packages with any event in any stream
	junit   []junitCase
}

// loadResults reads dir/*.json and dir/*.xml. A .json file that is not a go test -json
// stream (S7's marker-latency.json, for one) is ignored. A missing dir holds no results.
func loadResults(ctx context.Context, dir string) (*testResults, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	r := &testResults{goTests: map[string]*testOutcome{}, goRan: map[string]bool{}}
	streams, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	sort.Strings(streams)
	for _, p := range streams {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := r.addStream(ctx, p); err != nil {
			return nil, err
		}
	}
	junits, err := filepath.Glob(filepath.Join(dir, "*.xml"))
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	sort.Strings(junits)
	for _, p := range junits {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := r.addJUnit(ctx, p); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// addStream reads one go test -json stream. The file counts as a stream when its first
// non-empty line is a JSON object with an Action; unreadable lines after that are skipped.
func (r *testResults) addStream(ctx context.Context, path string) error {
	data, err := readFile(ctx, "", path)
	if err != nil {
		return err
	}
	first := true
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			Action  string
			Package string
			Test    string
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Action == "" {
			if first {
				return nil // not a go test -json stream
			}
			continue
		}
		first = false
		if ev.Package == "" {
			continue
		}
		r.goRan[ev.Package] = true
		if ev.Test == "" {
			continue
		}
		key := ev.Package + " " + ev.Test
		o := r.goTests[key]
		if o == nil {
			o = &testOutcome{}
			r.goTests[key] = o
		}
		switch ev.Action {
		case "pass":
			o.pass = true
		case "fail":
			o.fail = true
		case "skip":
			o.skip = true
		}
	}
	return nil
}

// addJUnit reads every <testcase> of one JUnit file, whatever its nesting. A done ctx
// stops it, with its error.
func (r *testResults) addJUnit(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	d := xml.NewDecoder(f)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "testcase" {
			continue
		}
		var c junitCase
		if err := d.DecodeElement(&c, &se); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		r.junit = append(r.junit, c)
	}
}

// goResult is what the streams say about one test (Name, or Name/Sub) of one package. A
// skip of any of its subtests, at any depth (a skipped table case), counts as a skip of
// the test, since the test's own pass does not show it: tests never call t.Skip
// (AGENTS.md §2).
func (r *testResults) goResult(pkg, test string) testOutcome {
	var o testOutcome
	if got := r.goTests[pkg+" "+test]; got != nil {
		o = *got
	}
	under := pkg + " " + test + "/"
	for key, sub := range r.goTests {
		if sub.skip && strings.HasPrefix(key, under) {
			o.skip = true
		}
	}
	return o
}

// pyResult is what the JUnit files say about test name in file, and whether any JUnit
// file has results for that file at all. A testcase belongs to the file when its file
// attribute names it or its classname is the file's module path (pytest's xunit2 form,
// a class name possibly appended); parametrized cases are name[…].
func (r *testResults) pyResult(file, name string) (testOutcome, bool) {
	module := strings.ReplaceAll(strings.TrimSuffix(file, ".py"), "/", ".")
	var o testOutcome
	ran := false
	for _, c := range r.junit {
		inFile := filepath.ToSlash(c.File) == file
		if c.File == "" {
			inFile = c.Classname == module || strings.HasPrefix(c.Classname, module+".")
		}
		if !inFile {
			continue
		}
		ran = true
		if c.Name != name && !strings.HasPrefix(c.Name, name+"[") {
			continue
		}
		switch {
		case len(c.Failures) > 0 || len(c.Errors) > 0:
			o.fail = true
		case len(c.Skipped) > 0:
			o.skip = true
		default:
			o.pass = true
		}
	}
	return o, ran
}
