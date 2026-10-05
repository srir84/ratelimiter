//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ratelimiter/internal/testutil/storeproc"
)

// measurements collects what the tests measure, by key, for .data/conformance.json, and
// counts the tests that started their store. It is the one mutable test global bin/trace
// testglobals allows here (plan choice); only record and startWith write it, and only
// from test goroutines (the suite never runs in parallel).
var measurements = struct {
	values  map[string]any
	started int
}{values: map[string]any{}}

// record keeps one measurement for .data/conformance.json.
func record(key string, value any) {
	measurements.values[key] = value
}

// resultKeys are the measurement keys of .data/conformance.json (plan choice); a key
// whose test did not record it is written as null.
var resultKeys = [...]string{
	"fsync", "row9", "row13", "recommended_store_conns", "row14", "scan_calls_200k", "crash", "reload_ms_a7",
}

func TestMain(m *testing.M) {
	code := m.Run()
	if unfiltered() && measurements.started > 0 {
		if err := writeResults(code == 0); err != nil {
			fmt.Fprintf(os.Stderr, "conformance: writing .data/conformance.json: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// unfiltered reports whether this run had no -run, -skip or -list: only a full suite run
// (make test-store) replaces .data/conformance.json, never test-store-quick, a -run, or a
// -list, which runs no test and still exits 0. TestMain also requires that a TestC2_*
// test started its store, so no other run that ran no test (-count=0) writes it either.
func unfiltered() bool {
	for _, name := range [...]string{"test.run", "test.skip", "test.list"} {
		if f := flag.Lookup(name); f == nil || f.Value.String() != "" {
			return false
		}
	}
	return true
}

// writeResults writes <module root>/.data/conformance.json atomically (temp file and
// rename); the orchestrator copies it into each run's context.json (DESIGN §B8).
func writeResults(passed bool) error {
	root, err := storeproc.ModuleRoot()
	if err != nil {
		return err
	}
	doc := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"os":           runtime.GOOS + "/" + runtime.GOARCH,
		"go":           runtime.Version(),
		"passed":       passed,
	}
	var product, version string
	bin, err := storeproc.Bin()
	if err == nil {
		product, version, err = storeproc.Version(context.Background(), bin)
	}
	if err != nil {
		return err
	}
	doc["store_bin"], doc["product"], doc["store_version"] = bin, product, version
	doc["eligible"] = eligible(product, version)
	for _, k := range resultKeys {
		doc[k] = nil
	}
	for k, v := range measurements.values {
		doc[k] = v
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(root, ".data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".conformance-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after the rename; cleans up a failed write
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "conformance.json"))
}

// eligible reports whether the store is the pinned product line, Valkey 9.1.x (§C2).
func eligible(product, version string) bool {
	return product == "Valkey" && strings.HasPrefix(version, "9.1.")
}
