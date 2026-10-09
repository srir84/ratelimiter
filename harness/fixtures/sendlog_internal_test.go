//go:build rltest

package fixtures

import (
	"strconv"
	"testing"
)

// TestSendLogBounded (IMPLEMENTATION_PLAN H4 step 4; AGENTS.md §1 Queues): the send log keeps
// its newest maxSends records; each older one gives its slot to a newer one and is counted as
// dropped, and the sequence numbers keep counting, so Since shows the drop as a gap.
func TestSendLogBounded(t *testing.T) {
	l := NewSendLog()
	if got := l.Since(0); got == nil || len(got) != 0 {
		t.Fatalf("Since(0) of an empty log = %#v, want an empty, non-nil slice", got)
	}
	const extra = 5
	for i := range maxSends + extra {
		l.ObserveSend("cmd"+strconv.Itoa(i+1), i)
	}
	got := l.Since(0)
	if len(got) != maxSends {
		t.Fatalf("Since(0) after %d records holds %d, want %d", maxSends+extra, len(got), maxSends)
	}
	first, last := got[0], got[len(got)-1]
	if first != (Send{Seq: extra + 1, Cmd: "cmd" + strconv.Itoa(extra+1), BytesWritten: extra}) {
		t.Errorf("the oldest record kept = %+v, want seq %d", first, extra+1)
	}
	if last != (Send{Seq: maxSends + extra, Cmd: "cmd" + strconv.Itoa(maxSends+extra), BytesWritten: maxSends + extra - 1}) {
		t.Errorf("the newest record = %+v, want seq %d", last, maxSends+extra)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq != got[i-1].Seq+1 {
			t.Fatalf("records %d and %d have seq %d and %d, want consecutive, oldest first", i-1, i, got[i-1].Seq, got[i].Seq)
		}
	}
	if d := l.Dropped(); d != extra {
		t.Errorf("Dropped() = %d, want %d", d, extra)
	}
	if got := l.Since(maxSends + extra - 1); len(got) != 1 || got[0].Seq != maxSends+extra {
		t.Errorf("Since(%d) = %+v, want only the newest record", maxSends+extra-1, got)
	}
}
