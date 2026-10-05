package conformance

import (
	"sync"
	"testing"
)

// measurements is the hook this package may write, whatever its type.
var measurements = struct {
	sync.Mutex
	m map[string]any
}{m: map[string]any{}}

func record(key string, value any) {
	measurements.Lock()
	defer measurements.Unlock()
	measurements.m[key] = value
}

func TestMain(m *testing.M) {
	record("go", "1.24")
	m.Run()
}
