package helper

import "sync"

// A test-helper package's variables are checked in every file.
var (
	portsMu sync.Mutex             // want
	ports   = map[int]bool{}       // a table that nothing writes passes
	events  = make(chan string, 8) // want
)

// Next mutates the state through methods and a send.
func Next() bool {
	portsMu.Lock()
	defer portsMu.Unlock()
	events <- "next" // want
	return ports[1]
}
