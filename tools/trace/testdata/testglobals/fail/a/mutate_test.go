package a

import (
	"maps"
	"slices"
	"sort"
	srt "sort"
	"strings"
	"testing"
)

// TestInPlace writes package-level variables by sends, close, and standard-library
// functions that change their first argument in place.
func TestInPlace(t *testing.T) {
	events <- "x" // want
	select {
	case events <- "y": // want
	default:
	}
	close(events)                                                      // want
	sort.Strings(names)                                                // want
	sort.Ints(list)                                                    // want
	sort.Float64s(floats)                                              // want
	sort.Slice(list, func(i, j int) bool { return list[i] < list[j] }) // want
	sort.SliceStable(names, func(i, j int) bool { return i < j })      // want
	sort.Sort(sort.Reverse(sort.IntSlice(list)))                       // want
	sort.Stable(byLen(names))                                          // want
	srt.Strings(names)                                                 // want
	slices.Sort(list)                                                  // want
	slices.SortFunc(names, strings.Compare)                            // want
	slices.SortStableFunc(names, strings.Compare)                      // want
	slices.Reverse(list)                                               // want
	maps.Copy(table, map[string]int{"b": 2})                           // want
	maps.DeleteFunc(table, func(string, int) bool { return true })     // want
	// Parentheses and explicit type arguments around the function change nothing.
	slices.Sort[[]int](list)                                                        // want
	slices.SortFunc[[]string, string](names, strings.Compare)                       // want
	maps.DeleteFunc[map[string]int](table, func(string, int) bool { return false }) // want
	(sort.Strings)(names)                                                           // want
	(slices.Sort[[]int])(list)                                                      // want
	(close)(events)                                                                 // want
	(delete)(table, "k")                                                            // want
	_ = slices.Index[[]int](list, 1)                                                // a read
	maps.Copy(map[string]int{}, table)                                              // a read: table is only the source
	_ = slices.Index(list, 1)                                                       // a read
	// So do the slices functions that delete, compact, replace or insert elements in the
	// backing array, and maps.Insert.
	got := slices.Delete(shared, 0, 1)                   // want
	fast := slices.DeleteFunc(allCases, isSlow)          // want
	_ = slices.Compact(list)                             // want
	_ = slices.CompactFunc(names, strings.EqualFold)     // want
	_ = slices.Replace(list, 0, 1, 9)                    // want
	_ = slices.Insert(list, 0, 9)                        // want
	maps.Insert(table, maps.All(map[string]int{"c": 3})) // want
	_, _ = got, fast
}
