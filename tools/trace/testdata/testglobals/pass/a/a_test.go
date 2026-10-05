package a

import (
	_ "embed"
	"testing"
)

var table = map[string]int{"a": 1} // a read-only table

const limit = 3

//go:embed data.txt
var data string

var counter int

func TestReads(t *testing.T) {
	_ = table["a"]
	_ = limit
	_ = data
	_ = counter
	counter := 0 // a local that shadows the package-level name
	counter++
	counter = 2
	_ = &counter
	for table := range 3 {
		_ = table
	}
	func(table map[string]int) {
		table["b"] = 2
	}(map[string]int{})
	if limit, ok := 1, true; ok {
		limit++
		_ = limit
	}
}
