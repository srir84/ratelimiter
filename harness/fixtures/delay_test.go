//go:build rltest

package fixtures_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/srir84/ratelimiter/harness/fixtures"
	"github.com/srir84/ratelimiter/internal/scripts"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/internal/store/resp"
)

// arrival is one call that reached a countingAdapter, and when.
type arrival struct {
	name string
	at   time.Time
}

// countingAdapter is a store adapter that answers every call at once, sent and ok, and records
// each call that reached it (the script name, or the Do command).
type countingAdapter struct {
	mu    sync.Mutex
	calls []arrival
}

func (c *countingAdapter) record(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, arrival{name: name, at: time.Now()})
}

func (c *countingAdapter) Call(_ context.Context, req store.Request) store.Response {
	c.record(req.Script.Name)
	return store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
}

func (c *countingAdapter) CallBatch(ctx context.Context, reqs []store.Request) []store.Response {
	out := make([]store.Response, len(reqs))
	for i, r := range reqs {
		out[i] = c.Call(ctx, r)
	}
	return out
}

func (c *countingAdapter) Do(_ context.Context, _ store.Kind, args ...string) (resp.Value, store.Response) {
	c.record(args[0])
	return resp.Value{Kind: resp.Simple, Str: "OK"}, store.Response{Outcome: store.OK, Sent: true, SentCommands: 1}
}

func (c *countingAdapter) ReadyConns(store.Kind) int { return 1 }

func (c *countingAdapter) Close() {}

// since returns the calls after the first n.
func (c *countingAdapter) since(n int) []arrival {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls[n:])
}

func (c *countingAdapter) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func names(as []arrival) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.name
	}
	return out
}

// TestDelayPositions (IMPLEMENTATION_PLAN H4 step 6; DESIGN §B6): a before_send delay (At
// empty or before_send) whose ctx ends first returns cancelled without calling the store
// adapter, for Call, Do and a CallBatch request alike; an after_reply delay calls the adapter
// at once and holds the return Delay, or until ctx ends; in a CallBatch the after_reply
// requests go out with the batch, which returns once the longest of their delays has passed,
// not their sum. Arm refuses a position the mode does not take.
func TestDelayPositions(t *testing.T) {
	w := fixtures.NewStoreWrapper()
	inner := &countingAdapter{}
	a := w.Wrap(inner)
	req := func(name string) store.Request { return store.Request{Script: scripts.Script{Name: name}} }
	arm := func(f fixtures.Fault) {
		t.Helper()
		if err := w.Arm(f); err != nil {
			t.Fatalf("Arm(%+v): %v", f, err)
		}
	}
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}

	for _, f := range []fixtures.Fault{
		{Mode: fixtures.Delay, Delay: time.Second, At: "during_send"},
		{Mode: fixtures.Refuse, At: fixtures.BeforeSend},
		{Mode: fixtures.DropReply, At: fixtures.AfterReply},
	} {
		if err := w.Arm(f); err == nil {
			t.Errorf("Arm(%+v) = nil, want a refusal: the mode takes no such position", f)
		}
	}
	if n := w.Armed(); n != 0 {
		t.Fatalf("%d faults armed after the refusals, want 0", n)
	}

	// before_send, the ctx ends first: cancelled, the adapter never called.
	arm(fixtures.Fault{Mode: fixtures.Delay, Match: "acquire", Count: 1, Delay: 2 * time.Second})
	arm(fixtures.Fault{Mode: fixtures.Delay, Match: "SCAN", Count: 1, Delay: 2 * time.Second, At: fixtures.BeforeSend})
	start := time.Now()
	if r := a.Call(short(), req("acquire")); r.Outcome != store.Cancelled || r.Sent {
		t.Errorf("Call under a 2 s before_send delay with a 100 ms ctx = %+v, want cancelled, not sent", r)
	}
	if _, r := a.Do(short(), store.Maintenance, "SCAN", "0"); r.Outcome != store.Cancelled || r.Sent {
		t.Errorf("Do under a 2 s before_send delay with a 100 ms ctx = %+v, want cancelled, not sent", r)
	}
	if el := time.Since(start); el >= 2*time.Second {
		t.Errorf("the two cancelled calls took %v, want each to end at its 100 ms ctx", el)
	}
	if got := names(inner.since(0)); len(got) != 0 {
		t.Errorf("the cancelled calls reached the adapter: %v, want none", got)
	}

	// after_reply: the adapter at once, the return Delay later.
	const delay = 500 * time.Millisecond
	arm(fixtures.Fault{Mode: fixtures.Delay, Match: "refund", Count: 1, Delay: delay, At: fixtures.AfterReply})
	n := inner.len()
	start = time.Now()
	r := a.Call(t.Context(), req("refund"))
	el := time.Since(start)
	got := inner.since(n)
	if r.Outcome != store.OK || !r.Sent || el < delay {
		t.Errorf("Call under an after_reply delay = %+v after %v, want ok, sent, after at least %v", r, el, delay)
	}
	if len(got) != 1 || got[0].name != "refund" || got[0].at.Sub(start) >= delay {
		t.Errorf("under an after_reply delay the adapter got %v, want the refund at once, before the delay", got)
	}

	// after_reply, the ctx ends first: the reply comes back at the ctx's end.
	arm(fixtures.Fault{Mode: fixtures.Delay, Match: "refund", Count: 1, Delay: 5 * time.Second, At: fixtures.AfterReply})
	n = inner.len()
	start = time.Now()
	if r := a.Call(short(), req("refund")); r.Outcome != store.OK || !r.Sent || time.Since(start) >= 2*time.Second {
		t.Errorf("Call under a 5 s after_reply delay with a 100 ms ctx = %+v after %v, want ok, sent, at the ctx's end", r, time.Since(start))
	}
	if got := names(inner.since(n)); !slices.Equal(got, []string{"refund"}) {
		t.Errorf("the adapter got %v, want the refund", got)
	}

	// CallBatch: after_reply requests go out with the batch, which waits the longest delay.
	arm(fixtures.Fault{Mode: fixtures.Delay, Match: "refund", Count: 1, Delay: delay, At: fixtures.AfterReply})
	arm(fixtures.Fault{Mode: fixtures.Delay, Match: "hold", Count: 1, Delay: delay, At: fixtures.AfterReply})
	n = inner.len()
	start = time.Now()
	rs := a.CallBatch(t.Context(), []store.Request{req("refund"), req("hold"), req("read_epoch")})
	el = time.Since(start)
	got = inner.since(n)
	for i, r := range rs {
		if r.Outcome != store.OK || !r.Sent {
			t.Errorf("batch response %d = %+v, want ok, sent", i, r)
		}
	}
	if !slices.Equal(names(got), []string{"refund", "hold", "read_epoch"}) || got[2].at.Sub(start) >= delay {
		t.Errorf("the batch with after_reply delays reached the adapter as %v, want the three requests at once", got)
	}
	if el < delay || el >= 2*delay-50*time.Millisecond {
		t.Errorf("the batch with two %v after_reply delays returned after %v, want the longest delay, not their sum", delay, el)
	}

	// CallBatch: a before_send delayed request whose ctx ends first is cancelled, never sent.
	arm(fixtures.Fault{Mode: fixtures.Delay, Match: "acquire", Count: 1, Delay: 2 * time.Second})
	n = inner.len()
	rs = a.CallBatch(short(), []store.Request{req("acquire"), req("read_epoch")})
	if len(rs) != 2 || rs[0].Outcome != store.Cancelled || rs[0].Sent || rs[1].Outcome != store.OK {
		t.Errorf("a batch with a 2 s before_send delay and a 100 ms ctx = %+v, want the delayed request cancelled, not sent, the other ok", rs)
	}
	if got := names(inner.since(n)); !slices.Equal(got, []string{"read_epoch"}) {
		t.Errorf("that batch reached the adapter with %v, want only read_epoch", got)
	}
}
