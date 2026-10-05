package a

import (
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

type ints []int

var (
	nameRe = regexp.MustCompile(`^[a-z]+$`) // only read through its methods
	sorted = []int{1, 2, 3}
	words  = map[string]int{"a": 1}
	lockFn func(*sync.Mutex)   // a function type holds no mutex
	locker interface{ Lock() } // nor does an interface type, with no value
)

// TestReadOnly: reads of package-level tables, and mutable values that tests own, pass.
func TestReadOnly(t *testing.T) {
	_ = nameRe.MatchString("abc")
	_ = sort.SearchInts(sorted, 2)
	_ = slices.Contains(sorted, 2)
	_ = slices.Index[[]int](sorted, 2) // explicit type arguments: still a read
	_ = slices.Sorted(maps.Keys(words))
	local := map[string]int{}
	maps.Copy(local, words) // words is only the source
	names := []string{"b", "a"}
	sort.Strings(names)
	slices.Reverse(names)
	var mu sync.Mutex
	mu.Lock()
	mu.Unlock()
	var b strings.Builder
	b.WriteString("x")
	ch := make(chan int, 1)
	ch <- 1
	close(ch)
	_, _ = lockFn, locker
	// Locals that hide an import or a type: no standard-library call, no conversion.
	slices := struct{ Sort func([]int) }{Sort: func([]int) {}}
	slices.Sort(sorted)
	ints := func(x []int) []int { return append([]int(nil), x...) }
	ints(sorted)[0] = 4
	{
		any := func(x []int) []int { return append([]int(nil), x...) }
		any(sorted)[0] = 4 // a local any is a function, not the predeclared type
	}
}
