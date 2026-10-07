package main

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// apiFixture merges the base fixture module (testdata/api/base) with overlays from
// testdata/api into a fresh root, with the real DESIGN.md and IMPLEMENTATION_PLAN.md, so
// the check reads the real §A5.2 listing, §A5.11, §A5.12 and Build items.
func apiFixture(t *testing.T, overlays ...string) string {
	t.Helper()
	names := []string{"api/base"}
	for _, o := range overlays {
		names = append(names, "api/"+o)
	}
	root := fixture(t, names...)
	copyFile(t, root, "DESIGN.md")
	copyFile(t, root, "IMPLEMENTATION_PLAN.md")
	return root
}

// apiRun runs trace api on root with args and returns its report; an error fails the test.
func apiRun(t *testing.T, root string, args ...string) report {
	t.Helper()
	r, err := checkAPI(t.Context(), testEnv(root), args)
	if err != nil {
		t.Fatalf("api %v: %v", args, err)
	}
	return r
}

// TestAPI (DESIGN §C7, §C9; plan S5 step 15, S6 step 7): the base fixture, staged like S5 but
// with §A5.2's Close(opts ...CloseOption), passes and names its missing §A5.2 items; under
// --complete each missing item fails. Each must-fail fixture fails with exactly its problem,
// also through the command line (exit 1), one per rule: an extra export in limiter or limitertest; an
// exported field, method or interface method no doc names (Health.Zork, Diag.Zork(), a
// method of the limitertest Adapter interface); names found in the docs only outside the
// allowed sections (a §A5.2 bullet, elsewhere in DESIGN, DESIGN after §A5.12, the plan
// outside every Build item, a slice's bullet after its Build item); a wrong field type; an extra field that is a doc word; an alias with a different
// method set (an extra method that is a doc word, a changed method signature); a changed
// function signature; a listed type, struct or function declared as another kind; a Close
// signature other than §A5.2's, S5's staged func() included, since S6 emptied the interim list;
// and limiter or limitertest not type-checking in its build. Without limiter/ the check passes.
func TestAPI(t *testing.T) {
	root := apiFixture(t)
	r := apiRun(t, root)
	if len(r.problems) != 0 {
		t.Fatalf("the staged base fixture fails:\n%s", strings.Join(r.problems, "\n"))
	}
	// limiter: 22 top-level names (13 types, 6 constants, 3 functions), plus 4 methods of
	// Limiter, 12 fields of Policy, 17 of Instance, 3 of StoreOptions, 5 of Result, 3 of Diag,
	// 2 methods of Permit, 2 fields of Health and 1 of MetricsSnapshot = 71. limitertest: 5
	// top-level names plus the 5 methods of the Adapter interface = 10.
	wantOut := "api: limiter (71 exported names) and limitertest (10) match DESIGN §A5.2, §A5.11, §A5.12 and the plan; " +
		"§A5.2 items not built yet: type RefundEvent, func WithRefundListener, method Limiter.RunCleanupPass, method Permit.Release"
	if len(r.out) != 1 || r.out[0] != wantOut {
		t.Fatalf("base fixture: out %q\nwant %q", r.out, wantOut)
	}

	r = apiRun(t, root, "--complete")
	wantComplete := []string{
		"limiter: §A5.2 item func WithRefundListener is missing (--complete)",
		"limiter: §A5.2 item method Limiter.RunCleanupPass is missing (--complete)",
		"limiter: §A5.2 item method Permit.Release is missing (--complete)",
		"limiter: §A5.2 item type RefundEvent is missing (--complete)",
	}
	got := slices.Clone(r.problems)
	sort.Strings(got)
	if !slices.Equal(got, wantComplete) {
		t.Fatalf("--complete on the base fixture:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantComplete, "\n"))
	}

	const notDoc = " appears in none of the DESIGN §A5.2 listing, §A5.11, §A5.12 or a plan Build item"
	for _, c := range []struct {
		overlay string
		want    string // the one problem
		prefix  bool   // want is the problem's start: the type checker's message follows
	}{
		{overlay: "extra", want: "limiter/extra.go:4: limiter.Frobnicate: exported name Frobnicate" + notDoc},
		{overlay: "extra_limitertest", want: "limitertest/extra.go:6: limitertest.Teleport: exported name Teleport" + notDoc},
		{overlay: "healthfield", want: "internal/core/limiter.go:9: limiter.Health.Zork: exported name Zork" + notDoc},
		{overlay: "diagmethod", want: "internal/core/diag_zork.go:5: limiter.Diag.Zork: exported name Zork" + notDoc},
		{overlay: "adaptermethod", want: "internal/store/store.go:16: limitertest.Adapter.Zork: exported name Zork" + notDoc},
		{overlay: "wordsprose", want: "limiter/runners.go:4: limiter.Runners: exported name Runners" + notDoc},
		{overlay: "wordsdesign", want: "limiter/batching.go:4: limiter.Batching: exported name Batching" + notDoc},
		{overlay: "wordsplan", want: "limiter/reviewer.go:5: limiter.Reviewer: exported name Reviewer" + notDoc},
		{overlay: "wordsafter", want: "limiter/checker.go:4: limiter.Checker: exported name Checker" + notDoc},
		{overlay: "wordsnonbuild", want: "limiter/breaker.go:5: limiter.Breaker: exported name Breaker" + notDoc},
		{overlay: "fieldtype", want: "internal/core/result.go:12: limiter.Result: fields differ from the DESIGN §A5.2 listing: not listed [DecisionNs int32], listed but absent [DecisionNs int64]"},
		{overlay: "extrafield", want: "internal/core/result.go:12: limiter.Result: fields differ from the DESIGN §A5.2 listing: not listed [Reason string], listed but absent []"},
		{overlay: "methodset", want: "internal/core/permit_kill.go:4: limiter.Permit.Kill: the DESIGN §A5.2 listing declares no method Kill on Permit, so its method set differs"},
		{overlay: "methodsig", want: "internal/core/result.go:22: limiter.Permit.Done: signature func(bool), the DESIGN §A5.2 listing has func()"},
		{overlay: "funcsig", want: "limiter/limiter.go:35: limiter.New: signature func(Policy, Instance, ...Option) (*Limiter, error), the DESIGN §A5.2 listing has func(Policy, Instance, StoreOptions, ...Option) (*Limiter, error)"},
		{overlay: "kindtype", want: "limiter/limiter.go:30: limiter.Result: §A5.2 declares a type, this is a variable"},
		{overlay: "kindstruct", want: "limiter/limiter.go:13: limiter.Result: §A5.2 declares a struct, this is int"},
		{overlay: "kindfunc", want: "limiter/limiter.go:31: limiter.DefaultPolicy: §A5.2 declares a function, this is a variable"},
		{overlay: "interimclose", want: "internal/core/limiter.go:21: limiter.Limiter.Close: signature func(bool), the DESIGN §A5.2 listing has func(...CloseOption)"},
		{overlay: "s5close", want: "internal/core/limiter.go:21: limiter.Limiter.Close: signature func(), the DESIGN §A5.2 listing has func(...CloseOption)"},
		{overlay: "broken", want: "limiter: does not load (untagged build): type-checking ratelimiter/limiter: ", prefix: true},
		{overlay: "brokentest", want: "limitertest: does not load (rltest build): type-checking ratelimiter/limitertest: ", prefix: true},
	} {
		t.Run(c.overlay, func(t *testing.T) {
			t.Parallel() // each case has its own root and loader; nothing is shared
			fx := apiFixture(t, c.overlay)
			r := apiRun(t, fx)
			ok := len(r.problems) == 1 && r.problems[0] == c.want
			if c.prefix {
				ok = len(r.problems) == 1 && strings.HasPrefix(r.problems[0], c.want)
			}
			if !ok {
				t.Errorf("problems\n  %s\nwant exactly one (matching as a prefix: %v)\n  %s", strings.Join(r.problems, "\n  "), c.prefix, c.want)
			}
			if len(r.out) != 0 {
				t.Errorf("a failing run printed %q", r.out)
			}
			line := "api: " + c.want
			if !c.prefix {
				line += "\n"
			}
			code, stdout, stderr := runIn(t, fx, "api")
			if code != 1 || stdout != "" || !strings.Contains(stderr, line) || !strings.Contains(stderr, "api: FAIL, 1 problem(s)\n") {
				t.Errorf("trace api: exit %d, want 1\nstdout %q\nstderr %q\nwant stderr to hold %q", code, stdout, stderr, line)
			}
		})
	}

	// Through the command line: exit 1 on a problem, 0 on the base, 2 on a bad flag.
	if code, _, stderr := runIn(t, apiFixture(t, "methodset"), "api"); code != 1 || !strings.Contains(stderr, "api: FAIL, 1 problem(s)") {
		t.Errorf("trace api on methodset: exit %d, want 1\n%s", code, stderr)
	}
	if code, stdout, stderr := runIn(t, root, "api"); code != 0 || stdout != wantOut+"\n" || stderr != "" {
		t.Errorf("trace api on the base: exit %d\nstdout %q\nstderr %q", code, stdout, stderr)
	}
	if code, _, _ := runIn(t, root, "api", "--bogus"); code != 2 {
		t.Errorf("trace api --bogus: exit %d, want 2", code)
	}

	// No limiter/ yet: pass, whatever else exists.
	bare := t.TempDir()
	writeTestFile(t, bare, "go.mod", "module ratelimiter\n\ngo 1.24\n")
	if r := apiRun(t, bare); len(r.problems) != 0 || len(r.out) != 1 || r.out[0] != "api: limiter/ does not exist yet, nothing to check" {
		t.Errorf("without limiter/: %q, %q", r.out, r.problems)
	}

	// Docs without the §A5.2 listing fail closed.
	broken := apiFixture(t)
	writeTestFile(t, broken, "DESIGN.md", "# Detailed design\n\n### A5.2 Public API (Go)\n\nNo listing.\n\n### A5.11 Health\n\n### A5.12 Hooks\n")
	if _, err := checkAPI(t.Context(), testEnv(broken), nil); err == nil || !strings.Contains(err.Error(), "§A5.2 has no ```go listing") {
		t.Errorf("docs without the §A5.2 listing: err = %v", err)
	}
}

// TestAPI_Docs: the real docs give the §A5.2 declarations and the words the check allows.
func TestAPI_Docs(t *testing.T) {
	d, err := readAPIDocs(t.Context(), repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string][]string) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if got := keys(d.structs); !slices.Equal(got, []string{"Instance", "Policy", "RefundEvent", "Result", "StoreOptions"}) {
		t.Errorf("structs %v", got)
	}
	if got := sortedKeys(d.funcs); !slices.Equal(got, []string{"DefaultInstance", "DefaultPolicy", "New", "WithRefundListener"}) {
		t.Errorf("funcs %v", got)
	}
	if got := sortedKeys(d.methods["Limiter"]); !slices.Equal(got, []string{"Acquire", "Close", "Health", "Metrics", "RunCleanupPass"}) {
		t.Errorf("Limiter methods %v", got)
	}
	if got := sortedKeys(d.methods["Permit"]); !slices.Equal(got, []string{"DeductionID", "Done", "Release"}) {
		t.Errorf("Permit methods %v", got)
	}
	for name, want := range map[string]string{
		"New":                "func(Policy, Instance, StoreOptions, ...Option) (*Limiter, error)",
		"WithRefundListener": "func(func(RefundEvent)) Option",
		"DefaultInstance":    "func(Policy) Instance",
	} {
		if got := d.funcs[name]; got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if got, want := d.methods["Limiter"]["Acquire"], "func(context.Context, string) Result"; got != want {
		t.Errorf("Acquire: %q, want %q", got, want)
	}
	if got := d.structs["RefundEvent"]; !slices.Contains(got, "FinalMonoNs int64") || !slices.Contains(got, "Result RefundResult") || len(got) != 7 {
		t.Errorf("RefundEvent fields %q", got)
	}
	if !slices.Contains(d.types, "Limiter") {
		t.Errorf("types %v lack the Limiter alias", d.types)
	}
	// Words from the listing's comments, §A5.11, §A5.12 and Build items; case and
	// underscores do not matter.
	for _, w := range []string{"ListenerDrops", "listener_drops", "LISTENERQUEUELEN", "PermitEpoch", "WithKeyReader", "ReceiptError", "UnresolvedClosed", "StoreError"} {
		if !d.words[apiNorm(w)] {
			t.Errorf("%s is not an allowed word", w)
		}
	}
	for _, w := range []string{"Frobnicate", "Teleport", "Zork"} {
		if d.words[apiNorm(w)] {
			t.Errorf("%s is an allowed word", w)
		}
	}
	// TestAPI's word fixtures use names the docs hold only outside the allowed sections:
	// Runners in a §A5.2 bullet, Batching elsewhere in DESIGN, Reviewer in the plan outside
	// every Build item. Each stays out of the allowed words and in its place.
	design, err := readLines(t.Context(), repoRoot(t), "DESIGN.md")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := readLines(t.Context(), repoRoot(t), "IMPLEMENTATION_PLAN.md")
	if err != nil {
		t.Fatal(err)
	}
	a52, ok := docSection(design, "### A5.2 ")
	if !ok {
		t.Fatal("DESIGN.md has no section ### A5.2")
	}
	for _, c := range []struct {
		word, where string
		lines       []string
	}{
		{"Runners", "DESIGN §A5.2", a52},
		{"Batching", "DESIGN", design},
		{"Reviewer", "the plan", plan},
	} {
		if d.words[apiNorm(c.word)] {
			t.Errorf("%s is an allowed word", c.word)
		}
		re := regexp.MustCompile(`(?i)\b` + c.word + `\b`)
		if !slices.ContainsFunc(c.lines, re.MatchString) {
			t.Errorf("%s no longer appears in %s, so its fixture tests nothing", c.word, c.where)
		}
	}
	// Two fixtures pin where the allowed sections end. Checker: DESIGN has it only after
	// §A5.12, so an §A5.11 or §A5.12 running on to the end of DESIGN would allow it. Breaker:
	// the plan has it only in a slice's later bullets (Tests), after its Build item and before
	// the next heading, so a Build item ending only at a heading would allow it.
	for _, c := range []struct {
		word, where string
		lines       []string // where the word must appear
	}{
		{"Checker", "DESIGN after §A5.12", afterHeading(design, "### A5.12 ")},
		{"Breaker", "a slice's bullets after its Build item", buildToHeading(plan)},
	} {
		if d.words[apiNorm(c.word)] {
			t.Errorf("%s is an allowed word", c.word)
		}
		re := regexp.MustCompile(`(?i)\b` + c.word + `\b`)
		if !slices.ContainsFunc(c.lines, re.MatchString) {
			t.Errorf("%s no longer appears in %s, so its fixture tests nothing", c.word, c.where)
		}
	}
}

// afterHeading returns the lines after the first heading starting with prefix, to the end.
func afterHeading(lines []string, prefix string) []string {
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return lines[i+1:]
		}
	}
	return nil
}

// buildToHeading returns the plan lines from each "- **Build:**" line to the next heading,
// outside code fences: a slice's Build item and the bullets after it.
func buildToHeading(plan []string) []string {
	var out []string
	in, fence := false, false
	for _, line := range plan {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			fence = !fence
		}
		if !fence {
			switch {
			case strings.HasPrefix(line, "- **Build:**"):
				in = true
			case strings.HasPrefix(line, "#"):
				in = false
			}
		}
		if in {
			out = append(out, line)
		}
	}
	return out
}

// TestAPI_Cancelled: a done ctx stops the check with its error, also once limiter/ exists.
func TestAPI_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := checkAPI(ctx, testEnv(apiFixture(t)), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
