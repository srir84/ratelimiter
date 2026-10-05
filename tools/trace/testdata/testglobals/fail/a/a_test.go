package a

import (
	"os"
	"testing"
)

var hits int

func TestWrites(t *testing.T) {
	counter = 1             // want
	cfg.n = 2               // want
	table["k"] = 3          // want
	list[0] = 4             // want
	*ptr = 5                // want
	counter++               // want
	hits--                  // want
	p := &counter           // want
	os.Args = nil           // want
	counter += len(os.Args) // want
	for counter = range 3 { // want
	}
	delete(table, "k")   // want
	clear(table)         // want
	copy(list, []int{1}) // want
	_ = p
}
