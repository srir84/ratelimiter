package a

import (
	"testing"

	"ratelimiter/harness/driver"
)

// recorder's record changes it (a pointer receiver); count only reads it. ring is
// generic.
type (
	recorder    struct{ evs []string }
	ring[T any] struct{ items []T }
)

func (r *recorder) record(ev string) { r.evs = append(r.evs, ev) }

func (r recorder) count() int { return len(r.evs) }

func (r *ring[T]) push(v T) { r.items = append(r.items, v) }

var (
	rec    recorder
	recp   = &recorder{}
	recn   = new(recorder)
	queue2 ring[string]
	regd   driver.Instances // of a type that a test-helper package declares
)

// TestMethods: a call or method value of a pointer-receiver method takes &v, a write to
// the package-level variable v: one of this package (by its declared type or by its
// initial value), one of a test-helper package (driver.Live), or one of a type that a
// test-helper package declares.
func TestMethods(t *testing.T) {
	rec.record("a")        // want
	listener := rec.record // want
	listener("b")
	recp.record("c")                      // want
	recn.record("f")                      // want
	queue2.push("g")                      // want
	(*recp).record("d")                   // want
	driver.Live.Add("x")                  // want
	regd.Add("y")                         // want
	_ = rec.count() + driver.Live.Count() // value receivers: reads
	local := recorder{}
	local.record("e") // a local
}
