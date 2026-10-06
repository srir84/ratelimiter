package scriptrun_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/scriptrun"
)

// recordTB is a testing.TB whose Errorf records the message instead of failing the test,
// so TestScriptAudit and TestRunRepliesFile can see what Run reports.
type recordTB struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recordTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recordTB) Helper() {}

func (r *recordTB) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errs...)
}

// fixedCaller is a scriptrun.Caller that answers every call with the same response.
type fixedCaller store.Response

func (c fixedCaller) Call(context.Context, store.Request) store.Response { return store.Response(c) }

// TestRunRepliesFile (plan S4 step 14 and Watch out): with RL_REPLIES_FILE set, Run appends
// one line {script, code, reason} per decoded reply to the file it names, creating its
// directory, and none for a reply it could not decode; a relative path is reported with
// t.Errorf, and nothing is written. bin/mutate sets the variable for every go test it starts,
// so the replies of a mutant never reach the module's .out/replies-l2.jsonl. Store-free: the
// callers are fakes.
func TestRunRepliesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "replies-l2.jsonl")
	t.Setenv(scriptrun.RepliesEnv, path)
	s := scripts.New("acquire", "#!lua name=acquire\nreturn 1\n")
	req := store.Request{Kind: store.Call, Script: s}
	for _, r := range []store.Response{
		{Outcome: store.OK, Reply: store.Reply{Code: 1}},
		{Outcome: store.OutcomeUnknown, StoreError: "i/o timeout"},
		{Outcome: store.OK, Reply: store.Reply{Code: 22, Reason: "integrity_hold"}},
	} {
		rec := &recordTB{TB: t}
		scriptrun.Run(rec, fixedCaller(r), req)
		if msgs := rec.messages(); len(msgs) > 0 {
			t.Fatalf("Run with outcome %v reported %q, want nothing", r.Outcome, msgs)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the replies file: %v", err)
	}
	want := `{"script":"acquire","code":1,"reason":""}` + "\n" + `{"script":"acquire","code":22,"reason":"integrity_hold"}` + "\n"
	if string(data) != want {
		t.Errorf("%s holds\n%s\nwant one line per decoded reply:\n%s", path, data, want)
	}

	t.Run("a relative path", func(t *testing.T) {
		const rel = "replies-relative.jsonl"
		t.Setenv(scriptrun.RepliesEnv, rel)
		rec := &recordTB{TB: t}
		scriptrun.Run(rec, fixedCaller{Outcome: store.OK, Reply: store.Reply{Code: 1}}, req)
		if msgs := rec.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], scriptrun.RepliesEnv+`="`+rel+`" is not an absolute path`) {
			t.Errorf("Run reported %q, want one error naming the relative %s", msgs, scriptrun.RepliesEnv)
		}
		if _, err := os.Stat(rel); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Run wrote %s in the working directory (stat: %v)", rel, err)
		}
	})
}
