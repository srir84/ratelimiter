//go:build rltest

package fixtures

import "sync"

// maxSends is how many send records a SendLog keeps (plan choice; AGENTS.md §1 Queues): the
// newest ones, so a test that reads hitapi's GET /_test/sends right after a fault sees what
// was sent under it. At a few dozen bytes a record the ring stays near 200 KB.
const maxSends = 4096

// Send is one send-observer record (DESIGN §A5.4 test hook, §A5.12): its sequence number (1
// for the first write the log heard, then one more for each write, with no gap), the command
// (the script name for an EVALSHA, such as "acquire", else the command: "SCRIPT LOAD",
// "AUTH", "PING") and how many of its bytes that write sent. A record with BytesWritten above
// 0 is a command that was sent (§A5.4).
type Send struct {
	Seq          uint64 `json:"seq"`
	Cmd          string `json:"cmd"`
	BytesWritten int    `json:"bytes_written"`
}

// SendLog is a bounded send observer (IMPLEMENTATION_PLAN H4 step 4): limitertest's
// WithSendObserver takes ObserveSend, and hitapi's GET /_test/sends reads it (Since), so a
// test can show which commands were sent under FLT-2. It keeps the newest maxSends records in
// a ring: once the ring is full, each new record takes the oldest one's slot, and that record
// is dropped and counted (Dropped). Since the sequence numbers have no gap, a reader also sees
// a drop as a gap before the first record it gets. It is safe for concurrent use.
type SendLog struct {
	mu      sync.Mutex
	ring    []Send // at most maxSends records, oldest at start once the ring is full
	start   int    // the oldest record's index (0 until the ring is full)
	seq     uint64 // the last sequence number given
	dropped uint64 // records that gave their slot to a newer one
}

// NewSendLog returns an empty send log.
func NewSendLog() *SendLog { return &SendLog{ring: make([]Send, 0, maxSends)} }

// ObserveSend records one write of a command, as the limiter's send observer hears it: it runs
// on the writing goroutine and waits only for the log's own lock.
func (l *SendLog) ObserveSend(cmd string, bytesWritten int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	s := Send{Seq: l.seq, Cmd: cmd, BytesWritten: bytesWritten}
	if len(l.ring) < maxSends {
		l.ring = append(l.ring, s)
		return
	}
	l.ring[l.start] = s
	l.start = (l.start + 1) % maxSends
	l.dropped++
}

// Since returns the records kept whose sequence number is above seq, oldest first; an empty
// slice, never nil, when there is none.
func (l *SendLog) Since(seq uint64) []Send {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Send{}
	n := len(l.ring)
	for i := range n {
		if s := l.ring[(l.start+i)%n]; s.Seq > seq {
			out = append(out, s)
		}
	}
	return out
}

// Dropped returns how many records gave their slot to newer ones.
func (l *SendLog) Dropped() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}
