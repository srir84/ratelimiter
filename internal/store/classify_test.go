package store

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store/resp"
	"ratelimiter/internal/testutil/fakeresp"
)

// handlerFor answers a script with a valid reply of its own layout.
func handlerFor(script string) fakeresp.Handler {
	switch script {
	case "refund":
		return func(_, _ []string) resp.Value { return rep(3, tNow, "", 1_000_000_000) }
	case "cleanup_entry":
		return func(_, _ []string) resp.Value { return rep(30, tNow, "", 0) }
	}
	return echoAcquire
}

func TestClassifyErrorReplies(t *testing.T) {
	const (
		noscript   = "NOSCRIPT No matching script. Please use EVAL."
		oom        = "OOM command not allowed when used memory > 'maxmemory'."
		loading    = "LOADING Valkey is loading the dataset in memory"
		busy       = "BUSY Valkey is busy running a script. You can only call SCRIPT KILL or SHUTDOWN NOSAVE."
		compileErr = "ERR Error compiling script (new function): user_script:1: unexpected symbol near 'x'"
		otherSHA   = "0123456789abcdef0123456789abcdef01234567" // 40 hex digits, not the script's SHA-1
	)
	scriptedError := func(text string, n int) func(*fakeresp.Server) {
		return func(s *fakeresp.Server) { s.ReplyError("EVALSHA", text, n) }
	}
	cases := []struct {
		name       string
		script     string
		loaded     bool // the store knows the script before the call
		prep       func(*fakeresp.Server)
		failResend bool // the resend's write fails with no byte sent
		want       Outcome
		sentCmds   int
		noScript   bool
		evals      int    // EVALSHA commands the server receives
		loads      int    // SCRIPT LOAD commands the server receives
		storeErr   string // the Response's StoreError
		raw        string // what the Response's Raw holds; "" means Raw is empty
		// atD: the call ends at its deadline, start + D (300–450 ms), while its last command's
		// reply is due later and the stall rule (that command's write + D) has not ended it.
		atD bool
	}{
		{name: "NOSCRIPT once", script: "acquire", want: OK, sentCmds: 2, noScript: true, evals: 2, loads: 1},
		{name: "NOSCRIPT twice", script: "acquire", prep: scriptedError(noscript, 2),
			want: OutcomeUnknown, sentCmds: 2, noScript: true, evals: 2, loads: 1, storeErr: noscript},
		// The store refused the resend before running it, so only the first EVALSHA counts as
		// sent (SPEC API-22: such a refusal "counts as not sent"; API-1d counts commands
		// actually sent); a failed NOSCRIPT retry is still outcome_unknown, never unavailable.
		{name: "NOSCRIPT, then LOADING on the resend", script: "acquire",
			prep: func(s *fakeresp.Server) { s.After("SCRIPT LOAD", func() { s.ReplyError("EVALSHA", loading, 1) }) },
			want: OutcomeUnknown, sentCmds: 1, noScript: true, evals: 2, loads: 1, storeErr: loading},
		{name: "reload delayed past the deadline", script: "acquire",
			prep: func(s *fakeresp.Server) { s.Delay("SCRIPT LOAD", 400*time.Millisecond) },
			want: OutcomeUnknown, sentCmds: 1, noScript: true, evals: 1, loads: 1, storeErr: noscript},
		// NOSCRIPT comes 200 ms after the write and SCRIPT LOAD's reply 280 ms after its own:
		// at 480 ms, after start + D but before the load's write + D, so only the call's
		// deadline ends the reload.
		{name: "SCRIPT LOAD answered after start + D", script: "acquire",
			prep: func(s *fakeresp.Server) {
				s.Delay("EVALSHA", 200*time.Millisecond)
				s.Delay("SCRIPT LOAD", 280*time.Millisecond)
			},
			want: OutcomeUnknown, sentCmds: 1, noScript: true, evals: 1, loads: 1, storeErr: noscript, atD: true},
		// SCRIPT LOAD's reply comes at 150 ms and the resend's 250 ms after its write: at
		// 400 ms, after start + D but before the resend's write + D, so only the call's
		// deadline ends the resend.
		{name: "resend answered after start + D", script: "acquire",
			prep: func(s *fakeresp.Server) {
				s.Delay("SCRIPT LOAD", 150*time.Millisecond)
				s.After("SCRIPT LOAD", func() { s.Delay("EVALSHA", 250*time.Millisecond) })
			},
			want: OutcomeUnknown, sentCmds: 2, noScript: true, evals: 2, loads: 1, atD: true},
		{name: "SCRIPT LOAD returns another SHA", script: "acquire",
			prep: func(s *fakeresp.Server) { s.ReplyValue("SCRIPT LOAD", resp.Value{Kind: resp.Bulk, Str: otherSHA}, 1) },
			want: OutcomeUnknown, sentCmds: 1, noScript: true, evals: 1, loads: 1, storeErr: noscript, raw: otherSHA},
		{name: "SCRIPT LOAD answered with an error", script: "acquire",
			prep: func(s *fakeresp.Server) { s.ReplyError("SCRIPT LOAD", compileErr, 1) },
			want: OutcomeUnknown, sentCmds: 1, noScript: true, evals: 1, loads: 1, storeErr: compileErr},
		{name: "the resend writes no byte", script: "acquire", failResend: true,
			want: OutcomeUnknown, sentCmds: 1, noScript: true, evals: 1, loads: 1, storeErr: noscript},
		{name: "OOM on acquire", script: "acquire", loaded: true, prep: scriptedError(oom, 1),
			want: CapacityError, sentCmds: 1, evals: 1, storeErr: oom},
		{name: "OOM on refund", script: "refund", loaded: true, prep: scriptedError(oom, 1),
			want: CapacityError, sentCmds: 1, evals: 1, storeErr: oom},
		{name: "OOM on cleanup_entry", script: "cleanup_entry", loaded: true, prep: scriptedError(oom, 1),
			want: OutcomeUnknown, sentCmds: 1, evals: 1, storeErr: oom},
		{name: "LOADING", script: "acquire", loaded: true, prep: scriptedError(loading, 1),
			want: Unavailable, sentCmds: 0, evals: 1, storeErr: loading},
		{name: "BUSY", script: "acquire", loaded: true, prep: scriptedError(busy, 1),
			want: Unavailable, sentCmds: 0, evals: 1, storeErr: busy},
		{name: "BUSYKEY", script: "acquire", loaded: true, prep: scriptedError("BUSYKEY Target key name already exists.", 1),
			want: OutcomeUnknown, sentCmds: 1, evals: 1, storeErr: "BUSYKEY Target key name already exists."},
		{name: "READONLY", script: "acquire", loaded: true, prep: scriptedError("READONLY You can't write against a read only replica.", 1),
			want: OutcomeUnknown, sentCmds: 1, evals: 1, storeErr: "READONLY You can't write against a read only replica."},
		{name: "ERR unknown command", script: "acquire", loaded: true,
			prep: scriptedError("ERR unknown command 'EVALSHA', with args beginning with: ", 1),
			want: OutcomeUnknown, sentCmds: 1, evals: 1,
			storeErr: "ERR unknown command 'EVALSHA', with args beginning with: "},
		{name: "ERR Error running script", script: "acquire", loaded: true,
			prep: scriptedError("ERR Error running script (call to f_0a1b): @user_script:7: attempt to compare nil with number", 1),
			want: OutcomeUnknown, sentCmds: 1, evals: 1,
			storeErr: "ERR Error running script (call to f_0a1b): @user_script:7: attempt to compare nil with number"},
		{name: "lowercase loading", script: "acquire", loaded: true, prep: scriptedError("loading the dataset in memory", 1),
			want: OutcomeUnknown, sentCmds: 1, evals: 1, storeErr: "loading the dataset in memory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sc := scripts.New(tc.script, "-- "+tc.name)
			if tc.loaded {
				e.srv.Load(sc.Text)
			}
			e.srv.Handle(sc.SHA, handlerFor(tc.script))
			if tc.prep != nil {
				tc.prep(e.srv)
			}
			// One call connection: the server runs hooks before the next command.
			opts := []func(*Options){withConns(1, 1)}
			if tc.failResend {
				var evals atomic.Int64
				var taps tapList
				opts = append(opts, withTap(&taps, func(_ *tapConn, p []byte) (bool, int, error) {
					if bytes.Contains(p, []byte("EVALSHA")) && evals.Add(1) == 2 {
						return true, 0, injectedError{} // the resend: not one byte leaves
					}
					return false, 0, nil
				}))
			}
			e.start(opts...)
			e.warm(1, 1)
			began := time.Now()
			r := e.a.Call(t.Context(), Request{Kind: Call, Script: sc, Args: []string{"1"}})
			el := time.Since(began)
			if r.Outcome != tc.want || r.SentCommands != tc.sentCmds || r.Sent != (tc.sentCmds > 0) || r.NoScript != tc.noScript {
				t.Errorf("Call = %v, SentCommands %d, Sent %v, NoScript %v; want %v, %d, %v, %v",
					r.Outcome, r.SentCommands, r.Sent, r.NoScript, tc.want, tc.sentCmds, tc.sentCmds > 0, tc.noScript)
			}
			if r.StoreError != tc.storeErr {
				t.Errorf("StoreError = %q, want %q", r.StoreError, tc.storeErr)
			}
			switch {
			case tc.raw == "" && r.Raw != "":
				t.Errorf("Raw = %q, want none", r.Raw)
			case !strings.Contains(r.Raw, tc.raw):
				t.Errorf("Raw = %q, want the reply %q", r.Raw, tc.raw)
			}
			if tc.atD && (el < testD || el > testD+150*time.Millisecond) {
				t.Errorf("Call ended after %v, want 300–450 ms (its deadline, start + D)", el)
			}
			if tc.want == OK && r.Reply.Code != 1 {
				t.Errorf("Reply code = %d, want 1", r.Reply.Code)
			}
			if n := e.srv.Count("EVALSHA"); n != tc.evals {
				t.Errorf("the server received %d EVALSHA, want %d", n, tc.evals)
			}
			if n := e.srv.Count("SCRIPT LOAD"); n != tc.loads {
				t.Errorf("the server received %d SCRIPT LOAD, want %d", n, tc.loads)
			}
		})
	}
}

// TestNotReadyReply (DESIGN §A5.4 not-ready replies; SPEC API-22): exactly the error replies
// whose first space-delimited word is LOADING or BUSY are not-ready refusals; a longer word
// (BUSYKEY), a lowercase one, the other classified words and an empty text are not.
func TestNotReadyReply(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"LOADING Valkey is loading the dataset in memory", true},
		{"BUSY Valkey is busy running a script. You can only call SCRIPT KILL or SHUTDOWN NOSAVE.", true},
		{"LOADING", true},
		{"BUSY", true},
		{"BUSYKEY Target key name already exists.", false},
		{"LOADINGS the dataset", false},
		{"loading the dataset in memory", false},
		{"busy running a script", false},
		{" BUSY leading space", false},
		{"NOSCRIPT No matching script. Please use EVAL.", false},
		{"OOM command not allowed when used memory > 'maxmemory'.", false},
		{"ERR something", false},
		{"", false},
	} {
		if got := NotReadyReply(tc.text); got != tc.want {
			t.Errorf("NotReadyReply(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestReloadThenLostReply(t *testing.T) {
	e := newEnv(t)
	sc := scripts.New("acquire", "-- reload, then the resend's reply is lost")
	e.srv.Handle(sc.SHA, echoAcquire) // not loaded: the first EVALSHA gets NOSCRIPT
	e.srv.After("SCRIPT LOAD", e.srv.Stall)
	e.start(withConns(1, 1)) // one call connection: the stall lands before the resend arrives
	e.warm(1, 1)
	r := e.a.Call(t.Context(), acquireReq(sc, 1))
	if r.Outcome != OutcomeUnknown || !r.Sent || r.SentCommands != 2 || !r.NoScript {
		t.Errorf("Call = %v, Sent %v, SentCommands %d, NoScript %v; want outcome_unknown, true, 2, true",
			r.Outcome, r.Sent, r.SentCommands, r.NoScript)
	}
	if n := e.srv.Count("EVALSHA"); n != 2 {
		t.Errorf("the server received %d EVALSHA, want 2", n)
	}
	if n := e.srv.Count("SCRIPT LOAD"); n != 1 {
		t.Errorf("the server received %d SCRIPT LOAD, want 1", n)
	}
}
