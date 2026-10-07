package store

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
)

// testCleanup is the Cleanup of the maintenance tests: an epoch, the test limits' policy
// values and the §A5.8 pipelines in flight (CleanupPipelines 4).
func testCleanup() Cleanup {
	return Cleanup{
		EpochID: "0123456789abcdef0123456789abcdef", FP: "ffffffffffffffffffffffffffffffff",
		FloorHwm: tNow - 5, FloorGen: 3, CapUnits: 20_000_000_000, RateTokens: 10, PushUs: lifeUs, Pipelines: 4,
	}
}

// members returns n budget key encodings, and each one's index.
func members(t *testing.T, n int) ([]string, map[string]int) {
	t.Helper()
	out, index := make([]string, n), make(map[string]int, n)
	for i := range out {
		enc, err := keys.Encode(fmt.Sprintf("key-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		out[i], index[enc] = enc, i
	}
	return out, index
}

// removedReply answers a cleanup_entry step like an E_REMOVED one whose markers_removed is
// the member's index (below 1,000), so each response can be matched to its member.
func removedReply(index map[string]int) fakeresp.Handler {
	return func(_, argv []string) resp.Value {
		i, ok := index[argv[2]]
		if len(argv) != 8 || !ok || i >= 1000 {
			return resp.Value{Kind: resp.Error, Str: "ERR test: unknown member"}
		}
		return rep(30, tNow, "", i)
	}
}

// checkRemoved fails the test unless every response is OK with the member's own reply.
func checkRemoved(t *testing.T, rs []Response, n int) {
	t.Helper()
	if len(rs) != n {
		t.Fatalf("%d responses for %d members", len(rs), n)
	}
	for i, r := range rs {
		if r.Outcome != OK || r.Reply.Code != 30 || r.Reply.Elems[3].Int != int64(i) {
			t.Errorf("member %d: %v (code %d, elems %v, store error %q), want ok with its own reply", i, r.Outcome, r.Reply.Code, r.Reply.Elems, r.StoreError)
		}
	}
}

// evalshaWrites returns a tap intercept that records, for every write holding EVALSHA, how
// many EVALSHA commands it carries (a pipeline is written in one Write, pipeConn.write).
func evalshaWrites(mu *sync.Mutex, counts *[]int) func(*tapConn, []byte) (bool, int, error) {
	return func(_ *tapConn, p []byte) (bool, int, error) {
		if n := bytes.Count(p, []byte("$7\r\nEVALSHA\r\n")); n > 0 {
			mu.Lock()
			*counts = append(*counts, n)
			mu.Unlock()
		}
		return false, 0, nil
	}
}

// batchAdapter is an Adapter whose CallBatch answers every request with an E_SKIPPED reply
// after hold, and records each pipeline's size and the most pipelines in flight at once. It
// answers nothing else.
type batchAdapter struct {
	hold time.Duration

	mu             sync.Mutex
	sizes          []int
	inFlight, most int
}

func (b *batchAdapter) Call(context.Context, Request) Response { return Response{Outcome: Unavailable} }

func (b *batchAdapter) CallBatch(_ context.Context, reqs []Request) []Response {
	b.mu.Lock()
	b.sizes = append(b.sizes, len(reqs))
	b.inFlight++
	b.most = max(b.most, b.inFlight)
	b.mu.Unlock()
	time.Sleep(b.hold)
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	out := make([]Response, len(reqs))
	for i := range out {
		out[i] = Response{Outcome: OK, Sent: true, SentCommands: 1, Reply: Reply{Code: 33, TUs: tNow, Reason: "gone"}}
	}
	return out
}

func (b *batchAdapter) Do(context.Context, Kind, ...string) (resp.Value, Response) {
	return resp.Value{}, Response{Outcome: Unavailable}
}

func (b *batchAdapter) ReadyConns(Kind) int { return 0 }

func (b *batchAdapter) Close() {}

// TestCleanupEntriesBatches (plan S7 step 4, L1; DESIGN §A5.8): CleanupEntries splits n steps
// into ⌈n / 100⌉ pipelines of 100, the last one holding the rest, with no empty pipeline (none
// at all for n = 0), each step's response in its place; with CleanupPipelines 1 (or a value
// below 1, taken as 1) one pipeline is in flight at a time, and with 4 never more than 4.
func TestCleanupEntriesBatches(t *testing.T) {
	for _, c := range []struct {
		n, inFlight int
		sizes       []int // sorted
		most        int   // the most pipelines in flight at once, at most
	}{
		{1, 4, []int{1}, 1},
		{99, 4, []int{99}, 1},
		{100, 4, []int{100}, 1},
		{101, 4, []int{1, 100}, 2},
		{200, 4, []int{100, 100}, 2},
		{250, 1, []int{50, 100, 100}, 1},
		{250, 0, []int{50, 100, 100}, 1},
		{1000, 4, []int{100, 100, 100, 100, 100, 100, 100, 100, 100, 100}, 4},
		{0, 4, nil, 0},
	} {
		t.Run(fmt.Sprintf("%d steps, %d in flight", c.n, c.inFlight), func(t *testing.T) {
			b := &batchAdapter{hold: 5 * time.Millisecond}
			ms, _ := members(t, c.n)
			cl := testCleanup()
			cl.Pipelines = c.inFlight
			rs := CleanupEntries(t.Context(), b, cl, ms)
			if len(rs) != c.n {
				t.Fatalf("%d responses for %d steps", len(rs), c.n)
			}
			for i, r := range rs {
				if r.Outcome != OK || r.Reply.Code != 33 {
					t.Errorf("step %d: %v, code %d; want its ok reply", i, r.Outcome, r.Reply.Code)
				}
			}
			b.mu.Lock()
			defer b.mu.Unlock()
			slices.Sort(b.sizes)
			if !slices.Equal(b.sizes, c.sizes) || b.most > c.most {
				t.Errorf("pipelines of %v, %d in flight at most; want %v, at most %d", b.sizes, b.most, c.sizes, c.most)
			}
		})
	}
}

// TestCleanupCallSecondsObserved (plan S7 step 4, L1; DESIGN §A5.11): each cleanup_entry step
// that CleanupEntries sends is observed once in rl_store_call_seconds{script="cleanup_entry"}.
func TestCleanupCallSecondsObserved(t *testing.T) {
	e := newEnv(t)
	ms, index := members(t, 150)
	e.srv.Load(scripts.CleanupEntry.Text)
	e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index))
	e.start(withConns(1, 1), withDeadline(5*time.Second))
	e.warm(1, 1)
	count := func() uint64 {
		for _, f := range e.reg.Snapshot().Families {
			if f.Name != "rl_store_call_seconds" {
				continue
			}
			for _, s := range f.Series {
				if s.LabelValue == "cleanup_entry" {
					return s.Count
				}
			}
		}
		t.Fatal(`no rl_store_call_seconds{script="cleanup_entry"} series`)
		return 0
	}
	before := count()
	checkRemoved(t, CleanupEntries(t.Context(), e.a, testCleanup(), ms), 150)
	if got := count() - before; got != 150 {
		t.Errorf(`rl_store_call_seconds{script="cleanup_entry"} count rose by %d after 150 steps, want 150`, got)
	}
}

// TestCleanupPipelineLimits (plan S7, L1; DESIGN §A5.4, §A5.8): CleanupEntries writes its steps
// in pipelines of at most 100 commands, on the maintenance connection only, with at most 4 in
// flight: 1,000 members go out as 10 writes of 100 and all succeed, in order; and with the fake
// store withholding every reply until 401 commands have arrived, exactly 400 arrive, so never
// more than 400 are outstanding, and once those fail (the deadline D passes) no further
// pipeline starts: the other 400 members are unavailable, nothing sent.
func TestCleanupPipelineLimits(t *testing.T) {
	t.Run("1,000 members", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 1000)
		e.srv.Load(scripts.CleanupEntry.Text)
		e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index))
		var mu sync.Mutex
		var writes []int
		var taps tapList
		e.start(withConns(1, 1), withDeadline(5*time.Second), withTap(&taps, evalshaWrites(&mu, &writes)))
		e.warm(1, 1)
		rs := CleanupEntries(t.Context(), e.a, testCleanup(), ms)
		checkRemoved(t, rs, 1000)
		mu.Lock()
		defer mu.Unlock()
		if len(writes) != 10 || slices.Max(writes) != CleanupPipeline || slices.Min(writes) != CleanupPipeline {
			t.Errorf("EVALSHA per write %v, want 10 writes of 100", writes)
		}
		conns := map[int]bool{}
		for _, c := range e.srv.Commands() {
			if fakeresp.Label(c.Args) == "EVALSHA" {
				conns[c.Conn] = true
			}
		}
		if len(conns) != 1 {
			t.Errorf("the steps came on connections %v, want one, the maintenance connection", conns)
		}
	})
	t.Run("at most 400 outstanding", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 800)
		e.srv.Load(scripts.CleanupEntry.Text)
		e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index))
		e.srv.HoldReplies(401)
		e.start(withConns(1, 1))
		e.warm(1, 1)
		rs := CleanupEntries(t.Context(), e.a, testCleanup(), ms)
		if n := e.srv.Count("EVALSHA"); n != 400 {
			t.Errorf("%d EVALSHA arrived while every reply was withheld, want 400 (CleanupPipelines × 100)", n)
		}
		sent := 0
		for i, r := range rs {
			switch {
			case r.Sent && r.Outcome == OutcomeUnknown:
				sent++
			case !r.Sent && r.Outcome == Unavailable:
			default:
				t.Errorf("member %d: %v, Sent %v; want outcome_unknown (sent, no reply) or unavailable (never sent)", i, r.Outcome, r.Sent)
			}
		}
		if sent != 400 {
			t.Errorf("%d members sent, want 400", sent)
		}
	})
}

// TestCleanupPipelineDepth (plan S7, L1; DESIGN §A5.8): the fake store withholds every reply
// on a connection until 400 commands, the §A5.8 depth (CleanupPipelines × 100), have arrived
// on it, or fails them after 2 s; CleanupEntries with 400 members gets all 400 replies, so it
// wrote them all before reading any. With TestCleanupPipelineLimits this pins the depth at
// 400.
func TestCleanupPipelineDepth(t *testing.T) {
	e := newEnv(t)
	ms, index := members(t, 400)
	e.srv.Load(scripts.CleanupEntry.Text)
	e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index))
	e.srv.HoldReplies(400)
	e.start(withConns(1, 1), withDeadline(5*time.Second))
	e.warm(1, 1)
	checkRemoved(t, CleanupEntries(t.Context(), e.a, testCleanup(), ms), 400)
}

// noScript is the store's reply to EVALSHA of an unknown script (§A3.0 row 7).
const noScript = "NOSCRIPT No matching script. Please use EVAL."

// resentAfterLoad returns the members of the EVALSHA commands the fake store received after
// its first SCRIPT LOAD, in arrival order.
func resentAfterLoad(t *testing.T, srv *fakeresp.Server) []string {
	t.Helper()
	var out []string
	loaded := false
	for _, c := range srv.Commands() {
		switch fakeresp.Label(c.Args) {
		case "SCRIPT LOAD":
			loaded = true
		case "EVALSHA":
			if loaded {
				out = append(out, c.Args[3+6+2]) // EVALSHA sha 6 k1…k6 epoch fp member …
			}
		}
	}
	return out
}

// TestCleanupNoscriptResendOnce (plan S7, L1; DESIGN §A5.4): steps answered NOSCRIPT did
// nothing, so CleanupEntries loads the script once and resends exactly those steps once: on a
// store that does not know the script, all 250 (3 pipelines) come back NOSCRIPT, one SCRIPT
// LOAD follows and the 250 are resent once each, every response then ok with NoScript set and
// SentCommands 2 (also with one pipeline in flight, where a resent step's result of each code
// 30–33 lets the next resend pipeline start); when only 30 steps get NOSCRIPT, exactly those
// 30 are resent, after one SCRIPT LOAD, and the others are sent once; when none does, there
// is no SCRIPT LOAD and each step is sent once.
func TestCleanupNoscriptResendOnce(t *testing.T) {
	t.Run("every step", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 250)
		e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index)) // unknown until SCRIPT LOAD
		e.start(withConns(1, 1), withDeadline(5*time.Second))
		e.warm(1, 1)
		rs := CleanupEntries(t.Context(), e.a, testCleanup(), ms)
		checkRemoved(t, rs, 250)
		for i, r := range rs {
			if !r.NoScript || !r.Sent || r.SentCommands != 2 {
				t.Errorf("member %d: NoScript %v, Sent %v, SentCommands %d; want true, true, 2", i, r.NoScript, r.Sent, r.SentCommands)
			}
		}
		if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 1 || ev != 500 {
			t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 1 and 500 (each step twice)", l, ev)
		}
		if got := resentAfterLoad(t, e.srv); len(got) != 250 {
			t.Errorf("%d steps resent after the load, want 250", len(got))
		}
	})
	// With one pipeline in flight, a resent step's result of each code 30–33 lets the next
	// resend pipeline start, so all 250 steps (3 pipelines) are resent once.
	t.Run("every step, one pipeline in flight", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 250)
		e.srv.Handle(scripts.CleanupEntry.SHA, func(_, argv []string) resp.Value { // unknown until SCRIPT LOAD
			i, ok := 0, false
			if len(argv) == 8 {
				i, ok = index[argv[2]]
			}
			switch {
			case !ok:
				return resp.Value{Kind: resp.Error, Str: "ERR test: unknown member"}
			case i%4 == 1:
				return rep(31, tNow, "", 0)
			case i%4 == 2:
				return rep(32, tNow, "malformed_bucket", 0)
			case i%4 == 3:
				return rep(33, tNow, "not_due", 0)
			}
			return rep(30, tNow, "", 0)
		})
		e.start(withConns(1, 1), withDeadline(5*time.Second))
		e.warm(1, 1)
		cl := testCleanup()
		cl.Pipelines = 1
		for i, r := range CleanupEntries(t.Context(), e.a, cl, ms) {
			if r.Outcome != OK || r.Reply.Code != int64(30+i%4) || !r.NoScript || r.SentCommands != 2 {
				t.Errorf("member %d: %v, code %d, NoScript %v, SentCommands %d; want ok with code %d, true, 2", i, r.Outcome, r.Reply.Code, r.NoScript, r.SentCommands, 30+i%4)
			}
		}
		if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 1 || ev != 500 {
			t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 1 and 500 (each step twice)", l, ev)
		}
	})
	t.Run("30 steps", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 250)
		e.srv.Load(scripts.CleanupEntry.Text)
		e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index))
		e.srv.ReplyError("EVALSHA", noScript, 30)
		e.start(withConns(1, 1), withDeadline(5*time.Second))
		e.warm(1, 1)
		rs := CleanupEntries(t.Context(), e.a, testCleanup(), ms)
		checkRemoved(t, rs, 250)
		var want []string
		for i, r := range rs {
			switch {
			case r.NoScript && r.SentCommands == 2:
				want = append(want, ms[i])
			case !r.NoScript && r.SentCommands == 1:
			default:
				t.Errorf("member %d: NoScript %v, SentCommands %d; want a NOSCRIPT step sent twice or another sent once", i, r.NoScript, r.SentCommands)
			}
		}
		if len(want) != 30 {
			t.Errorf("%d steps came back NOSCRIPT, want 30", len(want))
		}
		got := resentAfterLoad(t, e.srv)
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("resent after the load: %v, want exactly the NOSCRIPT steps %v", got, want)
		}
		if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 1 || ev != 280 {
			t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 1 and 280", l, ev)
		}
	})
	t.Run("no step", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 250)
		e.srv.Load(scripts.CleanupEntry.Text)
		e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index))
		e.start(withConns(1, 1), withDeadline(5*time.Second))
		e.warm(1, 1)
		rs := CleanupEntries(t.Context(), e.a, testCleanup(), ms)
		checkRemoved(t, rs, 250)
		for i, r := range rs {
			if r.NoScript || !r.Sent || r.SentCommands != 1 {
				t.Errorf("member %d: NoScript %v, Sent %v, SentCommands %d; want false, true, 1", i, r.NoScript, r.Sent, r.SentCommands)
			}
		}
		if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 0 || ev != 250 {
			t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 0 and 250 (each step once)", l, ev)
		}
	})
}

// TestCleanupNoscriptTwiceUnknown (plan S7, L1; DESIGN §A5.4): a resend answered NOSCRIPT
// again is outcome_unknown, with no third send, and stops the pass: with one pipeline in
// flight, a second NOSCRIPT for the first resend pipeline leaves the later ones unsent, their
// steps keeping their NOSCRIPT response (outcome_unknown, sent once), so 250 steps make 350
// EVALSHA; so is every NOSCRIPT step when the SCRIPT LOAD fails, or loads a script with
// another SHA, and then nothing is resent.
func TestCleanupNoscriptTwiceUnknown(t *testing.T) {
	cases := []struct {
		name        string
		n, inFlight int // steps, and CleanupPipelines
		prep        func(srv *fakeresp.Server)
		evalshas    int
		storeErr    string // the response's StoreError, "" for none
		raw         bool   // the response carries the load's reply
		resent      int    // the first members, each sent twice; the others are sent once
	}{
		{name: "NOSCRIPT twice", n: 100, inFlight: 4, prep: func(srv *fakeresp.Server) {
			srv.Load(scripts.CleanupEntry.Text)
			srv.ReplyError("EVALSHA", noScript, 200)
		}, evalshas: 200, storeErr: noScript, resent: 100},
		{name: "NOSCRIPT twice stops the resend", n: 250, inFlight: 1, prep: func(srv *fakeresp.Server) {
			srv.Load(scripts.CleanupEntry.Text)
			srv.ReplyError("EVALSHA", noScript, 500)
		}, evalshas: 350, storeErr: noScript, resent: 100},
		{name: "SCRIPT LOAD fails", n: 150, inFlight: 4, prep: func(srv *fakeresp.Server) {
			srv.ReplyError("SCRIPT LOAD", "ERR test: load refused", 1)
		}, evalshas: 150, storeErr: "ERR test: load refused"},
		{name: "SCRIPT LOAD returns another SHA", n: 150, inFlight: 4, prep: func(srv *fakeresp.Server) {
			srv.ReplyValue("SCRIPT LOAD", resp.Value{Kind: resp.Bulk, Str: "0000000000000000000000000000000000000000"}, 1)
		}, evalshas: 150, storeErr: noScript, raw: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ms, index := members(t, c.n)
			e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index))
			c.prep(e.srv)
			e.start(withConns(1, 1), withDeadline(5*time.Second))
			e.warm(1, 1)
			cl := testCleanup()
			cl.Pipelines = c.inFlight
			rs := CleanupEntries(t.Context(), e.a, cl, ms)
			if len(rs) != c.n {
				t.Fatalf("%d responses for %d steps", len(rs), c.n)
			}
			for i, r := range rs {
				sent := 1
				if i < c.resent {
					sent = 2
				}
				if r.Outcome != OutcomeUnknown || !r.NoScript || !r.Sent || r.SentCommands != sent ||
					r.StoreError != c.storeErr || (r.Raw != "") != c.raw {
					t.Errorf("member %d: %v, NoScript %v, Sent %v, SentCommands %d, StoreError %q, Raw %q; want outcome_unknown, true, true, %d, %q, a raw reply %v",
						i, r.Outcome, r.NoScript, r.Sent, r.SentCommands, r.StoreError, r.Raw, sent, c.storeErr, c.raw)
				}
			}
			if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 1 || ev != c.evalshas {
				t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 1 and %d", l, ev, c.evalshas)
			}
		})
	}
}

// pausedBatches is an Adapter that passes every call on to the embedded one, pausing around
// its CallBatch calls: before[n] before the n-th of them (counted from 1) goes on, after[n]
// after it returns. The maps are only read.
type pausedBatches struct {
	Adapter
	before, after map[int]time.Duration

	mu sync.Mutex
	n  int
}

func (p *pausedBatches) CallBatch(ctx context.Context, reqs []Request) []Response {
	p.mu.Lock()
	p.n++
	n := p.n
	p.mu.Unlock()
	time.Sleep(p.before[n])
	rs := p.Adapter.CallBatch(ctx, reqs)
	time.Sleep(p.after[n])
	return rs
}

// TestCleanupNoscriptDeadline (plan S7 step 4, L1; DESIGN §A5.4; SPEC API-22): a NOSCRIPT
// reload and resend stay within the deadline of the call they continue, min(ctx deadline,
// start + D) from the first send, never a deadline of their own (D = 300 ms; one connection,
// which the fake store answers in order). With EVALSHA answered 200 ms after its write and
// SCRIPT LOAD 280 ms after its own (at 480 ms: after the deadline, within the load's write + D),
// the step ends at its deadline, 300–450 ms after the call began, outcome_unknown with its
// NOSCRIPT response, sent once; with SCRIPT LOAD answered at 150 ms and the resend 250 ms after
// its write (at 400 ms: after the deadline, within the resend's write + D), it ends at its
// deadline outcome_unknown, sent twice; a reload and resend that fit (each command answered
// 40 ms after its write) succeed before it. cleanup_entry (CleanupEntries' pipeline), and
// due_entries and reclaim (Call) alike. A SCRIPT LOAD that would start only after the NOSCRIPT
// steps' deadline is never sent, and each resend pipeline keeps the deadline of its own steps'
// first send (D = 500 ms), not an earlier pipeline's. A SCRIPT LOAD or a resend pipeline that
// serves steps of two first-round pipelines sent 200 ms apart keeps the earlier one's deadline
// (D = 300 ms), not the later one's: a resend pipeline that would go out after it is never
// sent, its steps keeping their NOSCRIPT response, and a SCRIPT LOAD answered after it ends
// the call then, 300–450 ms after it began.
func TestCleanupNoscriptDeadline(t *testing.T) {
	ms, index := members(t, 1)
	runs := []struct {
		script string
		run    func(ctx context.Context, a Adapter) Response
		code   int64 // the reply code once it runs
	}{
		{"cleanup_entry", func(ctx context.Context, a Adapter) Response { return CleanupEntries(ctx, a, testCleanup(), ms)[0] }, 30},
		{"due_entries", func(ctx context.Context, a Adapter) Response { return DueEntries(ctx, a, testCleanup(), 1000, 0) }, 0},
		{"reclaim", func(ctx context.Context, a Adapter) Response { return Reclaim(ctx, a, []string{"rl:e:x:ix"}) }, 40},
	}
	timings := []struct {
		name      string
		prep      func(srv *fakeresp.Server)
		want      Outcome
		sentCmds  int
		storeErr  string
		evals     int
		atD, fits bool // ends at its deadline (300–450 ms); ends before it
	}{
		{name: "SCRIPT LOAD answered after the deadline", prep: func(srv *fakeresp.Server) {
			srv.Delay("EVALSHA", 200*time.Millisecond)
			srv.Delay("SCRIPT LOAD", 280*time.Millisecond)
		}, want: OutcomeUnknown, sentCmds: 1, storeErr: noScript, evals: 1, atD: true},
		{name: "resend answered after the deadline", prep: func(srv *fakeresp.Server) {
			srv.Delay("SCRIPT LOAD", 150*time.Millisecond)
			srv.After("SCRIPT LOAD", func() { srv.Delay("EVALSHA", 250*time.Millisecond) })
		}, want: OutcomeUnknown, sentCmds: 2, evals: 2, atD: true},
		{name: "reload and resend within the deadline", prep: func(srv *fakeresp.Server) {
			srv.Delay("EVALSHA", 40*time.Millisecond)
			srv.Delay("SCRIPT LOAD", 40*time.Millisecond)
		}, want: OK, sentCmds: 2, evals: 2, fits: true},
	}
	for _, r := range runs {
		for _, c := range timings {
			t.Run(r.script+": "+c.name, func(t *testing.T) {
				e := newEnv(t)
				e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index)) // none loaded: NOSCRIPT first
				e.srv.Handle(scripts.DueEntries.SHA, func(_, _ []string) resp.Value { return rep(0, tNow, "", 0) })
				e.srv.Handle(scripts.Reclaim.SHA, func(_, _ []string) resp.Value { return rep(40, 0, "", 1) })
				c.prep(e.srv)
				e.start(withConns(1, 1))
				e.warm(1, 1)
				began := time.Now()
				got := r.run(t.Context(), e.a)
				el := time.Since(began)
				if got.Outcome != c.want || !got.NoScript || !got.Sent || got.SentCommands != c.sentCmds || got.StoreError != c.storeErr || got.Raw != "" {
					t.Errorf("%v, NoScript %v, Sent %v, SentCommands %d, StoreError %q, Raw %q; want %v, true, true, %d, %q, none",
						got.Outcome, got.NoScript, got.Sent, got.SentCommands, got.StoreError, got.Raw, c.want, c.sentCmds, c.storeErr)
				}
				if c.want == OK && got.Reply.Code != r.code {
					t.Errorf("reply code %d, want %d", got.Reply.Code, r.code)
				}
				switch {
				case c.atD && (el < testD || el > testD+150*time.Millisecond):
					t.Errorf("ended %v after the call began, want 300–450 ms: at its deadline, start + D", el)
				case c.fits && el >= testD:
					t.Errorf("ended %v after the call began, want before its deadline (%v)", el, testD)
				}
				if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 1 || ev != c.evals {
					t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 1 and %d", l, ev, c.evals)
				}
			})
		}
	}

	// One pipeline in flight: the first pipeline's 100 steps get NOSCRIPT, then 400 ms pass
	// (D + 100 ms) before the second goes out and succeeds, so the SCRIPT LOAD would start after
	// the 100 steps' deadline: it is never sent, and they keep their NOSCRIPT response.
	t.Run("SCRIPT LOAD after the deadline", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 150)
		e.srv.Load(scripts.CleanupEntry.Text)
		e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index))
		e.srv.ReplyError("EVALSHA", noScript, 100)
		e.start(withConns(1, 1))
		e.warm(1, 1)
		p := &pausedBatches{Adapter: e.a, after: map[int]time.Duration{1: testD + 100*time.Millisecond}}
		cl := testCleanup()
		cl.Pipelines = 1
		rs := CleanupEntries(t.Context(), p, cl, ms)
		for i, r := range rs {
			switch {
			case i < 100:
				if r.Outcome != OutcomeUnknown || !r.NoScript || !r.Sent || r.SentCommands != 1 || r.StoreError != noScript {
					t.Errorf("member %d: %v, NoScript %v, Sent %v, SentCommands %d, StoreError %q; want its NOSCRIPT response, not resent",
						i, r.Outcome, r.NoScript, r.Sent, r.SentCommands, r.StoreError)
				}
			case r.Outcome != OK || r.Reply.Code != 30 || r.NoScript || r.SentCommands != 1:
				t.Errorf("member %d: %v, code %d, NoScript %v, SentCommands %d; want ok, 30, false, 1", i, r.Outcome, r.Reply.Code, r.NoScript, r.SentCommands)
			}
		}
		if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 0 || ev != 150 {
			t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 0 and 150", l, ev)
		}
	})

	// D = 500 ms, one pipeline in flight, a store that knows no script: the first pipeline's
	// 100 steps get NOSCRIPT at once, then 300 ms pass before the second pipeline goes out (its
	// deadline 300 ms later than the first's) and gets NOSCRIPT; the load and the first resend
	// pipeline go out at once, the second resend pipeline 300 ms later, after the first
	// pipeline's deadline but before its own: every step succeeds, sent twice.
	t.Run("each resend pipeline keeps its steps' deadline", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 200)
		e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index)) // unknown until SCRIPT LOAD
		e.start(withConns(1, 1), withDeadline(500*time.Millisecond))
		e.warm(1, 1)
		p := &pausedBatches{Adapter: e.a,
			after:  map[int]time.Duration{1: 300 * time.Millisecond},
			before: map[int]time.Duration{4: 300 * time.Millisecond}}
		cl := testCleanup()
		cl.Pipelines = 1
		rs := CleanupEntries(t.Context(), p, cl, ms)
		checkRemoved(t, rs, 200)
		for i, r := range rs {
			if !r.NoScript || !r.Sent || r.SentCommands != 2 {
				t.Errorf("member %d: NoScript %v, Sent %v, SentCommands %d; want true, true, 2", i, r.NoScript, r.Sent, r.SentCommands)
			}
		}
		if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 1 || ev != 400 {
			t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 1 and 400", l, ev)
		}
	})

	// D = 300 ms, one pipeline in flight: the first pipeline's members 0–49 get NOSCRIPT at once
	// (50–99 succeed), then 200 ms pass before the second goes out, whose members 100–149 get
	// NOSCRIPT (150–199 succeed). The load goes out at once, within both pipelines' deadlines,
	// and the one resend pipeline, which holds the steps of both, 150 ms after it: at 350 ms or
	// more, after the first pipeline's deadline but well before the second's (500 ms or more).
	// It keeps the first pipeline's deadline, so it is never sent, and all 100 steps keep their
	// NOSCRIPT response.
	t.Run("a resend pipeline keeps its earliest step's deadline", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 200)
		e.srv.Load(scripts.CleanupEntry.Text)
		h := removedReply(index)
		var mu sync.Mutex
		answered := map[int]bool{}
		e.srv.Handle(scripts.CleanupEntry.SHA, func(k, argv []string) resp.Value {
			i, ok := 0, false
			if len(argv) == 8 {
				i, ok = index[argv[2]]
			}
			mu.Lock()
			defer mu.Unlock()
			if ok && i%100 < 50 && !answered[i] { // members 0–49 and 100–149, on their first send
				answered[i] = true
				return resp.Value{Kind: resp.Error, Str: noScript}
			}
			return h(k, argv)
		})
		e.start(withConns(1, 1))
		e.warm(1, 1)
		p := &pausedBatches{Adapter: e.a,
			after:  map[int]time.Duration{1: 200 * time.Millisecond},
			before: map[int]time.Duration{3: 150 * time.Millisecond}}
		cl := testCleanup()
		cl.Pipelines = 1
		for i, r := range CleanupEntries(t.Context(), p, cl, ms) {
			switch {
			case i%100 < 50:
				if r.Outcome != OutcomeUnknown || !r.NoScript || !r.Sent || r.SentCommands != 1 || r.StoreError != noScript {
					t.Errorf("member %d: %v, NoScript %v, Sent %v, SentCommands %d, StoreError %q; want its NOSCRIPT response, not resent",
						i, r.Outcome, r.NoScript, r.Sent, r.SentCommands, r.StoreError)
				}
			case r.Outcome != OK || r.Reply.Code != 30 || r.NoScript || r.SentCommands != 1:
				t.Errorf("member %d: %v, code %d, NoScript %v, SentCommands %d; want ok, 30, false, 1", i, r.Outcome, r.Reply.Code, r.NoScript, r.SentCommands)
			}
		}
		if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 1 || ev != 200 {
			t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 1 and 200: no resend reaches the store", l, ev)
		}
	})

	// D = 300 ms, one pipeline in flight, a store that knows no script: the first pipeline's
	// 100 steps get NOSCRIPT at once, then 200 ms pass before the second pipeline's 100 do. The
	// SCRIPT LOAD, which serves the steps of both, goes out at once and is answered 280 ms after
	// its write, at 480 ms or more: after the first pipeline's deadline. It keeps that deadline,
	// not the second pipeline's (500 ms or more), so the call ends then, 300–450 ms after it
	// began, and every step keeps its NOSCRIPT response.
	t.Run("the SCRIPT LOAD keeps its earliest step's deadline", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 200)
		e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index)) // unknown until SCRIPT LOAD
		e.srv.Delay("SCRIPT LOAD", 280*time.Millisecond)
		e.start(withConns(1, 1))
		e.warm(1, 1)
		p := &pausedBatches{Adapter: e.a, after: map[int]time.Duration{1: 200 * time.Millisecond}}
		cl := testCleanup()
		cl.Pipelines = 1
		began := time.Now()
		rs := CleanupEntries(t.Context(), p, cl, ms)
		el := time.Since(began)
		for i, r := range rs {
			if r.Outcome != OutcomeUnknown || !r.NoScript || !r.Sent || r.SentCommands != 1 || r.StoreError != noScript {
				t.Errorf("member %d: %v, NoScript %v, Sent %v, SentCommands %d, StoreError %q; want its NOSCRIPT response, not resent",
					i, r.Outcome, r.NoScript, r.Sent, r.SentCommands, r.StoreError)
			}
		}
		if el < testD || el > testD+150*time.Millisecond {
			t.Errorf("ended %v after the call began, want 300–450 ms: at the first pipeline's deadline, start + D", el)
		}
		if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 1 || ev != 200 {
			t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 1 and 200", l, ev)
		}
	})
}

// TestCleanupNoscriptResendSettles (plan S7 step 4, L1; DESIGN §A5.4; SPEC API-22): how a
// resent step settles besides ok. A resend the store refuses before running it (LOADING) is
// outcome_unknown, sent once (the first EVALSHA), with the refusal kept; a step the resend
// never carries, because a resent step before it failed, keeps its NOSCRIPT response
// (outcome_unknown, sent once, the NOSCRIPT reply kept), as in Call.
func TestCleanupNoscriptResendSettles(t *testing.T) {
	const loading = "LOADING Valkey is loading the dataset in memory"
	t.Run("resend refused", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 100)
		e.srv.Load(scripts.CleanupEntry.Text)
		e.srv.Handle(scripts.CleanupEntry.SHA, removedReply(index))
		e.srv.ReplyError("EVALSHA", noScript, 100)
		e.srv.ReplyError("EVALSHA", loading, 100)
		e.start(withConns(1, 1), withDeadline(5*time.Second))
		e.warm(1, 1)
		for i, r := range CleanupEntries(t.Context(), e.a, testCleanup(), ms) {
			if r.Outcome != OutcomeUnknown || !r.NoScript || !r.Sent || r.SentCommands != 1 || r.StoreError != loading {
				t.Errorf("member %d: %v, NoScript %v, Sent %v, SentCommands %d, StoreError %q; want outcome_unknown, true, true, 1, the LOADING reply",
					i, r.Outcome, r.NoScript, r.Sent, r.SentCommands, r.StoreError)
			}
		}
	})
	t.Run("resend stopped", func(t *testing.T) {
		e := newEnv(t)
		ms, index := members(t, 250)
		h := removedReply(index)
		e.srv.Handle(scripts.CleanupEntry.SHA, func(k, argv []string) resp.Value { // unknown until SCRIPT LOAD
			if len(argv) > 2 && argv[2] == ms[50] {
				return resp.Value{Kind: resp.Error, Str: "ERR test: resend failed"}
			}
			return h(k, argv)
		})
		e.start(withConns(1, 1), withDeadline(5*time.Second))
		e.warm(1, 1)
		c := testCleanup()
		c.Pipelines = 1
		for i, r := range CleanupEntries(t.Context(), e.a, c, ms) {
			switch {
			case i == 50:
				if r.Outcome != OutcomeUnknown || r.SentCommands != 2 || r.StoreError != "ERR test: resend failed" {
					t.Errorf("member 50: %v, SentCommands %d, StoreError %q; want outcome_unknown, 2, the resend's error", r.Outcome, r.SentCommands, r.StoreError)
				}
			case i < 100:
				if r.Outcome != OK || r.SentCommands != 2 || !r.NoScript {
					t.Errorf("member %d: %v, SentCommands %d, NoScript %v; want ok, 2, true", i, r.Outcome, r.SentCommands, r.NoScript)
				}
			default:
				if r.Outcome != OutcomeUnknown || !r.NoScript || !r.Sent || r.SentCommands != 1 || r.StoreError != noScript {
					t.Errorf("member %d: %v, NoScript %v, Sent %v, SentCommands %d, StoreError %q; want its NOSCRIPT response",
						i, r.Outcome, r.NoScript, r.Sent, r.SentCommands, r.StoreError)
				}
			}
		}
		if l, ev := e.srv.Count("SCRIPT LOAD"), e.srv.Count("EVALSHA"); l != 1 || ev != 350 {
			t.Errorf("%d SCRIPT LOAD and %d EVALSHA, want 1 and 350 (250, then one resent pipeline)", l, ev)
		}
	})
}

// TestCleanupNoscriptAndFailure (plan S7 step 4, L1; DESIGN §A5.4; SPEC API-1f): the first
// failed step stops the pass, so only the NOSCRIPT steps before it in member order are
// resent. With one pipeline of two steps: when the first step fails and the second is answered
// NOSCRIPT, nothing more is sent (no SCRIPT LOAD; the second keeps its NOSCRIPT response,
// outcome_unknown, sent once, so it is not counted); when the first is answered NOSCRIPT and
// the second fails, one SCRIPT LOAD follows and the first alone is resent (ok, sent twice),
// while the second keeps its failure.
func TestCleanupNoscriptAndFailure(t *testing.T) {
	const failure = "ERR test: step failed"
	for _, c := range []struct {
		name     string
		noscript int // the member answered NOSCRIPT once; the other one's step fails
		loads    int
	}{
		{name: "failure, then NOSCRIPT", noscript: 1},
		{name: "NOSCRIPT, then failure", noscript: 0, loads: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ms, index := members(t, 2)
			e.srv.Load(scripts.CleanupEntry.Text)
			h := removedReply(index)
			var mu sync.Mutex
			answered := false
			e.srv.Handle(scripts.CleanupEntry.SHA, func(k, argv []string) resp.Value {
				if len(argv) != 8 || index[argv[2]] != c.noscript {
					return resp.Value{Kind: resp.Error, Str: failure}
				}
				mu.Lock()
				defer mu.Unlock()
				if !answered {
					answered = true
					return resp.Value{Kind: resp.Error, Str: noScript}
				}
				return h(k, argv)
			})
			e.start(withConns(1, 1), withDeadline(5*time.Second))
			e.warm(1, 1)
			cl := testCleanup()
			cl.Pipelines = 1
			rs := CleanupEntries(t.Context(), e.a, cl, ms)
			if len(rs) != 2 {
				t.Fatalf("%d responses for 2 steps", len(rs))
			}
			failed := 1 - c.noscript
			if r := rs[failed]; r.Outcome != OutcomeUnknown || r.NoScript || !r.Sent || r.SentCommands != 1 || r.StoreError != failure {
				t.Errorf("member %d: %v, NoScript %v, Sent %v, SentCommands %d, StoreError %q; want its failure: outcome_unknown, false, true, 1, %q",
					failed, r.Outcome, r.NoScript, r.Sent, r.SentCommands, r.StoreError, failure)
			}
			r := rs[c.noscript]
			want := []string{ms[0], ms[1]} // the EVALSHA members and the SCRIPT LOADs, in arrival order
			if c.loads == 0 {
				if r.Outcome != OutcomeUnknown || !r.NoScript || !r.Sent || r.SentCommands != 1 || r.StoreError != noScript {
					t.Errorf("member 1: %v, NoScript %v, Sent %v, SentCommands %d, StoreError %q; want its NOSCRIPT response, not resent",
						r.Outcome, r.NoScript, r.Sent, r.SentCommands, r.StoreError)
				}
			} else {
				if r.Outcome != OK || r.Reply.Code != 30 || r.Reply.Elems[3].Int != 0 || !r.NoScript || !r.Sent || r.SentCommands != 2 {
					t.Errorf("member 0: %v, code %d, NoScript %v, Sent %v, SentCommands %d, StoreError %q; want ok with its own reply, true, true, 2",
						r.Outcome, r.Reply.Code, r.NoScript, r.Sent, r.SentCommands, r.StoreError)
				}
				want = append(want, "SCRIPT LOAD", ms[0])
			}
			var got []string
			for _, cmd := range e.srv.Commands() {
				switch fakeresp.Label(cmd.Args) {
				case "SCRIPT LOAD":
					got = append(got, "SCRIPT LOAD")
				case "EVALSHA":
					got = append(got, cmd.Args[3+6+2]) // EVALSHA sha 6 k1…k6 epoch fp member …
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("the store received %q, want %q", got, want)
			}
		})
	}
}

// TestCleanupStopsAtFailure (plan S7 step 4, L1; SPEC API-1f): once a step fails, with an
// error reply, a refusal reply (20–22, 25) or any outcome but NOSCRIPT, no further pipeline
// starts: with one pipeline in flight, a failed step in the second pipeline leaves the third
// unsent (unavailable, nothing sent) while the first two pipelines' other steps keep their
// replies. A step's result of every code 30–33 lets the next pipeline start.
func TestCleanupStopsAtFailure(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply resp.Value
		check func(r Response) bool
	}{
		{"an error reply", resp.Value{Kind: resp.Error, Str: "ERR test: step failed"},
			func(r Response) bool { return r.Outcome == OutcomeUnknown && r.StoreError == "ERR test: step failed" }},
		{"an integrity hold", rep(22, 0, "integrity_hold"), func(r Response) bool { return r.Outcome == OK && r.Reply.Code == 22 }},
		{"a config_error", rep(20, 0, "epoch_or_fingerprint"), func(r Response) bool { return r.Outcome == OK && r.Reply.Code == 20 }},
		{"a clock_error", rep(21, 0, "clock"), func(r Response) bool { return r.Outcome == OK && r.Reply.Code == 21 }},
		{"a bad call", rep(25, 0, "keys_mismatch"), func(r Response) bool { return r.Outcome == OK && r.Reply.Code == 25 }},
		{"a step's result", rep(33, tNow, "gone", 0), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ms, index := members(t, 300)
			e.srv.Load(scripts.CleanupEntry.Text)
			h := removedReply(index)
			e.srv.Handle(scripts.CleanupEntry.SHA, func(k, argv []string) resp.Value {
				if len(argv) > 2 && argv[2] == ms[150] {
					return c.reply
				}
				switch r := h(k, argv); {
				case len(argv) > 2 && index[argv[2]]%4 == 1:
					return rep(31, tNow, "", 0)
				case len(argv) > 2 && index[argv[2]]%4 == 2:
					return rep(32, tNow, "malformed_bucket", 0)
				case len(argv) > 2 && index[argv[2]]%4 == 3:
					return rep(33, tNow, "not_due", 0)
				default:
					return r
				}
			})
			e.start(withConns(1, 1), withDeadline(5*time.Second))
			e.warm(1, 1)
			cl := testCleanup()
			cl.Pipelines = 1
			rs := CleanupEntries(t.Context(), e.a, cl, ms)
			sent := 300
			if c.check != nil {
				sent = 200
			}
			for i, r := range rs {
				switch {
				case i == 150 && c.check != nil:
					if !c.check(r) {
						t.Errorf("member 150: %v, code %d, %q; want the failed step's response", r.Outcome, r.Reply.Code, r.StoreError)
					}
				case i < sent:
					if r.Outcome != OK || r.Reply.Code < 30 || r.Reply.Code > 33 {
						t.Errorf("member %d: %v, code %d; want ok with a step's result", i, r.Outcome, r.Reply.Code)
					}
				default:
					if r.Outcome != Unavailable || r.Sent {
						t.Errorf("member %d: %v, Sent %v; want unavailable, never sent", i, r.Outcome, r.Sent)
					}
				}
			}
			if n := e.srv.Count("EVALSHA"); n != sent {
				t.Errorf("%d EVALSHA, want %d", n, sent)
			}
		})
	}
}

// TestCleanupRequests (plan S7 step 4, L1; DESIGN §A4.1): the commands the maintenance calls
// send, as the fake store receives them on the maintenance connection: due_entries with KEYS
// epoch, ctl, ix and ARGV epoch_id, fp, limit, cutoff and the floors; cleanup_entry with KEYS
// epoch, ctl, ix, q, k:<member>, mi:<member>, or the placeholders k:! and mi:! for a member that
// fails is_enc, and ARGV epoch_id, fp, member, cap_units, rate_tokens, push_us and the floors;
// reclaim with KEYS rl:epoch and the candidates; SCAN <cursor> MATCH rl:e:* COUNT 1000.
func TestCleanupRequests(t *testing.T) {
	e := newEnv(t)
	for _, sc := range []scripts.Script{scripts.DueEntries, scripts.CleanupEntry, scripts.Reclaim} {
		e.srv.Load(sc.Text)
	}
	e.srv.Handle(scripts.DueEntries.SHA, func(_, _ []string) resp.Value { return rep(0, tNow, "", 0) })
	e.srv.Handle(scripts.CleanupEntry.SHA, func(_, _ []string) resp.Value { return rep(33, tNow, "gone", 0) })
	e.srv.Handle(scripts.Reclaim.SHA, func(_, _ []string) resp.Value { return rep(40, 0, "", 2) })
	e.start(withConns(1, 1), withDeadline(5*time.Second))
	e.warm(1, 1)
	c := testCleanup()
	id := c.EpochID
	if r := DueEntries(t.Context(), e.a, c, 1000, tNow-7); r.Outcome != OK || r.Reply.Code != 0 {
		t.Errorf("DueEntries: %v, code %d", r.Outcome, r.Reply.Code)
	}
	rs := CleanupEntries(t.Context(), e.a, c, []string{"YQ", "YR"})
	if len(rs) != 2 || rs[0].Outcome != OK || rs[1].Outcome != OK {
		t.Errorf("CleanupEntries: %v", rs)
	}
	if r := Reclaim(t.Context(), e.a, []string{"rl:e:x:ix", "rl:e:y:q"}); r.Outcome != OK || r.Reply.Elems[3].Int != 2 {
		t.Errorf("Reclaim: %v", r)
	}
	e.srv.ReplyValue("SCAN", rep("0", resp.Value{Kind: resp.Array, Elems: []resp.Value{{Kind: resp.Bulk, Str: "rl:e:x:ix"}}}), 1)
	if next, found, r := Scan(t.Context(), e.a, "17"); r.Outcome != OK || next != "0" || !slices.Equal(found, []string{"rl:e:x:ix"}) {
		t.Errorf("Scan = %q, %q, %v; want 0, [rl:e:x:ix], ok", next, found, r.Outcome)
	}
	want := [][]string{
		{"EVALSHA", scripts.DueEntries.SHA, "3", "rl:epoch", "rl:ctl:time", "rl:e:" + id + ":ix",
			id, c.FP, "1000", fmt.Sprint(tNow - 7), fmt.Sprint(tNow - 5), "3"},
		{"EVALSHA", scripts.CleanupEntry.SHA, "6", "rl:epoch", "rl:ctl:time", "rl:e:" + id + ":ix", "rl:e:" + id + ":q",
			"rl:e:" + id + ":k:YQ", "rl:e:" + id + ":mi:YQ", id, c.FP, "YQ", "20000000000", "10", "60000000", fmt.Sprint(tNow - 5), "3"},
		{"EVALSHA", scripts.CleanupEntry.SHA, "6", "rl:epoch", "rl:ctl:time", "rl:e:" + id + ":ix", "rl:e:" + id + ":q",
			"rl:e:" + id + ":k:!", "rl:e:" + id + ":mi:!", id, c.FP, "YR", "20000000000", "10", "60000000", fmt.Sprint(tNow - 5), "3"},
		{"EVALSHA", scripts.Reclaim.SHA, "3", "rl:epoch", "rl:e:x:ix", "rl:e:y:q"},
		{"SCAN", "17", "MATCH", "rl:e:*", "COUNT", "1000"},
	}
	var got [][]string
	conns := map[int]bool{}
	for _, cmd := range e.srv.Commands() {
		if l := fakeresp.Label(cmd.Args); l == "EVALSHA" || l == "SCAN" {
			got = append(got, cmd.Args)
			conns[cmd.Conn] = true
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the store received %d commands, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("command %d: %q, want %q", i+1, got[i], want[i])
		}
	}
	if len(conns) != 1 {
		t.Errorf("the commands came on connections %v, want one, the maintenance connection", conns)
	}
}

// TestScanReply (plan S7 step 4, L1): Scan returns the next cursor and the keys of a SCAN
// reply [cursor, [key, …]]; a reply of another shape, or a cursor that is not an unsigned
// 64-bit decimal of 1–20 digits (no sign; 2^64 − 1 accepted, 2^64 refused), is outcome_unknown
// with the raw reply, and the cursor given comes back.
func TestScanReply(t *testing.T) {
	bulk := func(s string) resp.Value { return resp.Value{Kind: resp.Bulk, Str: s} }
	arr := func(vs ...resp.Value) resp.Value { return resp.Value{Kind: resp.Array, Elems: vs} }
	cases := []struct {
		name  string
		reply resp.Value
		next  string
		keys  []string
		ok    bool
	}{
		{"keys", arr(bulk("1234"), arr(bulk("rl:e:a:ix"), bulk("rl:e:b:q"))), "1234", []string{"rl:e:a:ix", "rl:e:b:q"}, true},
		{"no keys, iteration done", arr(bulk("0"), arr()), "0", []string{}, true},
		{"20-digit cursor", arr(bulk("18446744073709551615"), arr()), "18446744073709551615", []string{}, true},
		{"21-digit cursor", arr(bulk("184467440737095516150"), arr()), "", nil, false},
		{"cursor 2^64", arr(bulk("18446744073709551616"), arr()), "", nil, false},
		{"20 nines", arr(bulk("99999999999999999999"), arr()), "", nil, false},
		{"21 characters, leading zeros", arr(bulk("000000000000000000005"), arr()), "", nil, false},
		{"cursor with a plus sign", arr(bulk("+5"), arr()), "", nil, false},
		{"negative cursor", arr(bulk("-5"), arr()), "", nil, false},
		{"empty cursor", arr(bulk(""), arr()), "", nil, false},
		{"cursor not decimal", arr(bulk("12a"), arr()), "", nil, false},
		{"cursor an integer", arr(vInt(5), arr()), "", nil, false},
		{"null cursor", arr(resp.Value{Kind: resp.Bulk, Null: true}, arr()), "", nil, false},
		{"a key not a string", arr(bulk("5"), arr(vInt(3))), "", nil, false},
		{"a null key", arr(bulk("5"), arr(resp.Value{Kind: resp.Bulk, Null: true})), "", nil, false},
		{"keys not an array", arr(bulk("5"), bulk("x")), "", nil, false},
		{"null key list", arr(bulk("5"), resp.Value{Kind: resp.Array, Null: true}), "", nil, false},
		{"three elements", arr(bulk("5"), arr(), arr()), "", nil, false},
		{"not an array", bulk("5"), "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.srv.ReplyValue("SCAN", c.reply, 1)
			e.start(withConns(1, 1))
			e.warm(1, 1)
			next, found, r := Scan(t.Context(), e.a, "77")
			if c.ok {
				if r.Outcome != OK || next != c.next || !slices.Equal(found, c.keys) {
					t.Errorf("Scan = %q, %q, %v; want %q, %q, ok", next, found, r.Outcome, c.next, c.keys)
				}
				return
			}
			if r.Outcome != OutcomeUnknown || r.Raw == "" || next != "77" || found != nil {
				t.Errorf("Scan = %q, %q, %v (raw %q); want the cursor 77, no keys, outcome_unknown with the raw reply", next, found, r.Outcome, r.Raw)
			}
		})
	}
}
