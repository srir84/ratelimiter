package a

import (
	"bytes"
	"strings"
	s "sync"
	"sync/atomic"
)

type registry struct {
	mu   s.Mutex
	seen map[string]bool
}

// Package-level variables of a _test.go file that hold a value which exists to be
// mutated, whatever writes them.
var (
	mu    s.Mutex                     // want
	rw    = &s.RWMutex{}              // want
	once  = new(s.Once)               // want
	hits2 atomic.Int64                // want
	last  atomic.Pointer[string]      // want
	queue chan string                 // want
	done  = make(chan struct{})       // want
	logb  strings.Builder             // want
	out   bytes.Buffer                // want
	buf   = bytes.NewBufferString("") // want
	cache struct {                    // want
		s.Mutex
		m map[string]int
	}
	reg      registry                         // want
	locks             = map[string]*s.Mutex{} // passes: its elements point at the mutexes; pass/internal/core's permitsByKey tests the map rule alone
	anyCh    any      = make(chan int)        // want
	cond              = s.NewCond(&s.Mutex{}) // want
	pool              = []any{&s.WaitGroup{}} // passes: only a value form at the top level counts
	nameList          = []string{"a"}         // a plain table passes
)

// Only what a variable holds itself counts: one top-level pointer is removed and value
// fields are read, but a pointer field, an element type or a type argument is never
// followed.
var (
	regp   *registry             // want
	nested struct{ r registry }  // want (the registry's mutex is held by value)
	byRef  struct{ mu *s.Mutex } // passes: a pointer field
	regs   []registry            // passes: a slice's element type
	held   vec[s.Mutex]          // passes: a type argument
)

// A fixed-length array holds its elements by value, so its element type is followed; a
// slice's is not.
var (
	shards      [4]s.Mutex             // want
	hitsByShard [4]atomic.Int64        // want
	regArr      [2]registry            // want (through type registry)
	lockArr     = [...]s.Mutex{{}, {}} // want
	rows        [2][]s.Mutex           // passes: an array of slices holds only their headers
)
