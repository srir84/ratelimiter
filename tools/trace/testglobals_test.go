package main

import (
	"strings"
	"testing"
)

func TestTestGlobals(t *testing.T) {
	root := testdataDir(t, "testglobals/fail")
	r, err := checkTestGlobals(t.Context(), testEnv(root), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Each write form is reported at its line, in a _test.go file and in plain files of
	// every test-helper tree (internal/testutil and harness/driver, fixtures, spectest,
	// e2e and pgtest): assignment, field, element and pointee writes, ++/--, &v, an
	// imported package's variable, a range assignment, delete, clear and copy, a write
	// inside a function literal, element writes through a reslice or a type assertion
	// (also by copy and clear), and each hook name written outside the hook's package.
	// So are writes through a conversion that keeps the storage (to a type expression, the
	// predeclared any, a type the package declares, also a generic one, or a sort slice
	// type: conv_test.go), a send, close, and each in-place sort, slices or maps function
	// the rule lists (sorting, reversing, deleting, compacting, replacing, inserting,
	// copying into a map), through a renamed or a dot import too, and with the function in
	// parentheses or given explicit type arguments (mutate_test.go, dot_test.go). So is a
	// call or method value of a pointer-receiver method on a package-level variable whose
	// type, by declaration or initial value and with one pointer removed, is declared in
	// its package or in a test-helper package: of the file's package, pkg.V of a
	// test-helper package declared in another file (harness/driver/registry.go), also
	// under a dot import (method_test.go, dotdriver_test.go), while value-receiver calls
	// and a local's pointer-receiver call there pass. And each package-level
	// variable a checked file declares that holds itself a sync or sync/atomic value, a
	// channel, a strings.Builder, a bytes.Buffer or a math/rand/v2 generator: by its type,
	// once one top-level pointer is removed, directly, in a value field of a struct type
	// (written out or declared in the package) or as a fixed-length array's elements, or
	// by the value form at the top of its initial value (state_test.go, rand_test.go,
	// helper/state.go), with hook names outside their packages (e2e/hooks.go). Each want
	// line is one problem.
	sameLines(t, "writes", problemLines(t, r.problems), wantLines(t, root), r.problems)
	if countContaining(r.problems, "os.Args") != 3 {
		t.Errorf("writes to os.Args (an imported package's variable) are not all reported:\n%s", strings.Join(r.problems, "\n"))
	}
	for _, want := range []string{
		"a/state_test.go:18:2: package-level variable mu holds sync.Mutex, which exists to be mutated",
		"a/state_test.go:23:2: package-level variable queue holds a channel,",
		"a/state_test.go:28:2: package-level variable cache holds sync.Mutex,",
		"a/state_test.go:32:2: package-level variable reg holds sync.Mutex (through type registry),",
		"a/state_test.go:55:2: package-level variable hitsByShard holds sync/atomic.Int64,",
		"a/state_test.go:56:2: package-level variable regArr holds sync.Mutex (through type registry),",
		"a/rand_test.go:8:2: package-level variable rng holds math/rand/v2.Rand,",
		"a/rand_test.go:11:2: package-level variable zipf holds math/rand/v2.Zipf,",
		"a/dot_test.go:9:5: package-level variable wg holds sync.WaitGroup,",
		"a/mutate_test.go:26:39: writes package-level variable list outside its declaration", // through sort.Reverse and sort.IntSlice,
		"a/method_test.go:36:14: writes package-level variable rec outside its declaration",  // a method value
		"a/method_test.go:42:2: writes package-level variable driver.Live outside its declaration",
		"a/dotdriver_test.go:12:2: writes package-level variable Live outside its declaration",
	} {
		if countContaining(r.problems, want) != 1 {
			t.Errorf("want one problem %q", want)
		}
	}
	// A read-only table, a constant, a //go:embed var, locals that shadow package-level
	// names, and the two named hooks pass (conformance's measurements also holds a
	// sync.Mutex); so does a helper package writing only locals. Reads of package-level
	// tables (a regexp's MatchString, sort.SearchInts, maps.Copy's source) pass, and so do
	// mutable values a test owns and locals that hide an import, a package type or any.
	// So do read-only tables of DESIGN's Result, which points at a Permit that holds an
	// atomic.Int32 (internal/core): pointers and slice or map element types are never
	// followed. A map of Permit values, a **Permit and a new(*Permit) pass there too: a
	// map's element type is never followed, and at most one top-level pointer is removed.
	// Calling a method of a type another package declares (regexp's MatchString) is a
	// read.
	r, err = checkTestGlobals(t.Context(), testEnv(testdataDir(t, "testglobals/pass")), nil)
	if err != nil || len(r.problems) > 0 {
		t.Fatalf("passing fixture: %v\n%s", err, strings.Join(r.problems, "\n"))
	}
}
