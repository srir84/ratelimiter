package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// curatedEntry is one curated planted bug of DESIGN §C9 (plan S4 step 16): From is one whole
// line of File (indentation ignored) and To replaces it, or deletes it when "". Tests are the
// tests that must kill it: Go names (Name_* selects every test with that prefix) or Python
// paths (path.py, path.py::name).
type curatedEntry struct {
	ID    string
	File  string
	Tests []string
	From  string
	To    string
}

// curatedEntries holds the §C9 rows whose files exist; later slices add theirs as their files
// land (S5: PB15, PB17; S6: PB5–PB8, PB16; S7: PB9, PB10; H5: PB18).
func curatedEntries() []curatedEntry {
	const limited = "if u < rate_period_ns then return reply(E_LIMITED, '', u) end -- limited: writes nothing (CLK-4)"
	return []curatedEntry{
		{
			ID: "PB1", File: "internal/scripts/acquire.lua", Tests: []string{"TestAcquireDecisions"},
			From: limited, // refuses at exactly one token (BUD-13)
			To:   "if u <= rate_period_ns then return reply(E_LIMITED, '', u) end -- limited: writes nothing (CLK-4)",
		},
		{
			ID: "PB2", File: "internal/scripts/acquire.lua", Tests: []string{"TestAcquireDecisions"},
			From: limited, // a limited acquire stores hwm (CLK-4)
			To:   "if u < rate_period_ns then return finish(E_LIMITED, '', u) end -- limited: writes nothing (CLK-4)",
		},
		{
			ID: "PB3", File: "internal/scripts/prelude.lua", Tests: []string{"TestAcquirePrelude"},
			From: "if v >= TBOUND then return nil end", // read_clock accepts TBOUND (CLK-8, §A3.1)
			To:   "",
		},
		{
			ID: "PB4", File: "internal/scripts/prelude.lua", Tests: []string{"TestPreludeValidators"},
			From: "if #s > 1 and string.sub(s, 1, 1) == '0' then return false end", // is_uint53 accepts a leading zero (§A4.2)
			To:   "",
		},
		{
			ID: "PB11", File: "internal/store/classify.go", Tests: []string{"TestClassifyErrorReplies"},
			From: "return Unavailable, false", // LOADING/BUSY → outcome_unknown (API-22)
			To:   "return OutcomeUnknown, false",
		},
		{
			ID: "PB12", File: "internal/store/reply.go", Tests: []string{"TestDecodeRanges"},
			From: `{6, "expiry_us", t + lim.LifetimeUs, t + lim.LifetimeUs},`, // the E_ALLOWED expiry check removed (§A4.9)
			To:   "",
		},
		{
			ID: "PB13", File: "internal/core/budget.go", Tests: []string{"TestRetryAfterNs"},
			From: "return (ratePeriodNs - u + rateTokens - 1) / rateTokens", // RetryAfterNs rounds down (BUD-13, API-6)
			To:   "return (ratePeriodNs - u) / rateTokens",
		},
		{
			ID: "PB14", File: "internal/receipt/receipt.go", Tests: []string{"TestVerifyRejectsEachFieldChange"},
			From: "b = binary.BigEndian.AppendUint64(b, r.S)", // the MAC input omits s (RCP-1)
			To:   "",
		},
		{
			ID: "PB15", File: "internal/core/acquire.go", Tests: []string{"TestFailureModeTable"},
			From: "resp := l.adapter.Call(ctx, req)", // Acquire sends again after OutcomeUnknown (API-12)
			To:   "resp := l.adapter.Call(ctx, req); if resp.Outcome == store.OutcomeUnknown { resp = l.adapter.Call(ctx, req) }",
		},
		{
			ID: "PB17", File: "refmodel/replay.py", Tests: []string{"refmodel/tests/test_replay.py::test_detects_each_field"},
			From: `checks.append(("u", expected.u, actual["u"]))`, // Replay stops comparing units (BUD-12)
			To:   `checks.append(("u", actual["u"], actual["u"]))`,
		},
	}
}

// applyLineEdit returns content with the one line whose trimmed text is from's replaced by to
// (with that line's indentation), or deleted when to is "". It fails unless exactly one line
// matches, or when to spans lines.
func applyLineEdit(content []byte, from, to string) ([]byte, error) {
	want := strings.TrimSpace(from)
	if want == "" {
		return nil, errors.New("the From line is empty")
	}
	if strings.ContainsAny(to, "\r\n") {
		return nil, errors.New("the To text spans more than one line")
	}
	lines := strings.SplitAfter(string(content), "\n")
	at := -1
	for i, l := range lines {
		if strings.TrimSpace(l) != want {
			continue
		}
		if at >= 0 {
			return nil, fmt.Errorf("the From line matches lines %d and %d", at+1, i+1)
		}
		at = i
	}
	if at < 0 {
		return nil, errors.New("the From line matches no line")
	}
	line := lines[at]
	if to == "" {
		lines[at] = ""
	} else {
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		end := line[len(strings.TrimRight(line, "\r\n")):]
		lines[at] = indent + strings.TrimSpace(to) + end
	}
	out := []byte(strings.Join(lines, ""))
	if string(out) == string(content) {
		return nil, errors.New("the edit changes nothing")
	}
	return out, nil
}

// designRow is one row of DESIGN §C9's curated table.
type designRow struct {
	id, file string
	tests    []string
}

// curatedRows parses the curated table of DESIGN §C9: the table whose header is
// | ID | File | Planted bug | Rule | Must fail |. Tests are the backticked test names in
// Must fail: Go Test…/Fuzz…/Example… names (a trailing _* kept) and Python .py paths.
func curatedRows(design string) ([]designRow, error) {
	var rows []designRow
	in := false
	for _, line := range strings.Split(design, "\n") {
		cells := tableCells(line)
		switch {
		case !in && len(cells) == 5 && cells[0] == "ID" && cells[1] == "File" && cells[4] == "Must fail":
			in = true
		case in && cells == nil:
			in = false
		case in && strings.Trim(strings.Join(cells, ""), "-: ") == "":
			// the delimiter row
		case in:
			if len(cells) != 5 {
				return nil, fmt.Errorf("curated table row %q has %d cells, want 5", line, len(cells))
			}
			r := designRow{id: cells[0], file: strings.Trim(cells[1], "`")}
			for _, span := range backticked(cells[4]) {
				if testNameRe.MatchString(span) || pyTestRe.MatchString(span) {
					r.tests = append(r.tests, span)
				}
			}
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return nil, errors.New("no curated table (| ID | File | Planted bug | Rule | Must fail |) in DESIGN.md")
	}
	return rows, nil
}

// tableCells splits a Markdown table row into its trimmed cells (a '|' inside a backtick
// span is text); nil when the line is no table row.
func tableCells(line string) []string {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "|") || !strings.HasSuffix(t, "|") || len(t) < 2 {
		return nil
	}
	var cells []string
	var cur strings.Builder
	inCode := false
	for _, r := range t[1 : len(t)-1] {
		switch {
		case r == '`':
			inCode = !inCode
			cur.WriteRune(r)
		case r == '|' && !inCode:
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// backticked returns the contents of the single-backtick spans of s.
func backticked(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '`')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(s[i+1:], '`')
		if j < 0 {
			return out
		}
		out = append(out, s[i+1:i+1+j])
		s = s[i+2+j:]
	}
}

// curatedProblems checks entries against DESIGN §C9's table and the files under root
// (TestCurated_MatchesDesign): every row whose file exists has exactly one entry with the same
// ID, file and tests, every entry has a row, and each entry's From matches exactly one line of
// its file while To is one line, so the edit changes or deletes exactly one line.
func curatedProblems(ctx context.Context, root, design string, entries []curatedEntry) ([]string, error) {
	rows, err := curatedRows(design)
	if err != nil {
		return nil, err
	}
	var problems []string
	byID := map[string][]curatedEntry{}
	for _, e := range entries {
		byID[e.ID] = append(byID[e.ID], e)
	}
	known := map[string]bool{}
	for _, r := range rows {
		known[r.id] = true
		exists, err := fileExists(ctx, filepath.Join(root, filepath.FromSlash(r.file)))
		if err != nil {
			return nil, err
		}
		got := byID[r.id]
		switch {
		case !exists && len(got) == 0:
			continue // not written yet
		case len(got) == 0:
			problems = append(problems, fmt.Sprintf("%s: %s exists, but curated.go has no entry for it", r.id, r.file))
			continue
		case len(got) > 1:
			problems = append(problems, fmt.Sprintf("%s: %d entries in curated.go, want one", r.id, len(got)))
		}
		e := got[0]
		if e.File != r.file {
			problems = append(problems, fmt.Sprintf("%s: file %s, but DESIGN §C9 names %s", r.id, e.File, r.file))
		}
		if a, b := slices.Sorted(slices.Values(e.Tests)), slices.Sorted(slices.Values(r.tests)); !slices.Equal(a, b) {
			problems = append(problems, fmt.Sprintf("%s: tests %v, but DESIGN §C9 names %v", r.id, e.Tests, r.tests))
		}
	}
	for _, e := range entries {
		if !known[e.ID] {
			problems = append(problems, fmt.Sprintf("%s: no row in DESIGN §C9's curated table", e.ID))
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.File)))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s: %v", e.ID, e.File, err))
			continue
		}
		if _, err := applyLineEdit(content, e.From, e.To); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s: %v", e.ID, e.File, err))
		}
	}
	return problems, nil
}

// fileExists reports whether p is a regular file. A done ctx stops it before it looks.
func fileExists(ctx context.Context, p string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := os.Stat(p)
	switch {
	case err == nil:
		return info.Mode().IsRegular(), nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}

// curatedCmd is `mutate --curated [ID…]` over entries (curatedEntries in main): each entry
// whose file exists runs; an error (a failing baseline, a test name that matches nothing) counts
// no mutant. Exit 1 when an entry errs or its mutant is not killed, 2 for an unknown ID.
func curatedCmd(ctx context.Context, cfg config, entries []curatedEntry, ids []string, out, errOut *printer) int {
	if len(ids) > 0 {
		var picked []curatedEntry
		for _, id := range ids {
			i := slices.IndexFunc(entries, func(e curatedEntry) bool { return e.ID == id })
			if i < 0 {
				errOut.printf("mutate: no curated entry %s\n", id)
				return 2
			}
			picked = append(picked, entries[i])
		}
		entries = picked
	}
	if err := resetDir(ctx, cfg.outDir); err != nil {
		errOut.printf("mutate: %v\n", err)
		return 1
	}
	idx := &testIndex{root: cfg.root}
	code, nKilled, nNotKilled, nErrors, nSkipped := 0, 0, 0, 0, 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			errOut.printf("mutate: stopped before %s: %v\n", e.ID, err)
			return 1
		}
		exists, err := fileExists(ctx, filepath.Join(cfg.root, filepath.FromSlash(e.File)))
		if err != nil {
			errOut.printf("mutate: %s: %v\n", e.ID, err)
			return 1
		}
		if !exists {
			nSkipped++
			continue
		}
		start := time.Now()
		res, err := runCurated(ctx, cfg, idx, e)
		switch {
		case err != nil:
			code, nErrors = 1, nErrors+1
			errOut.printf("mutate: %s (%s): %v\n", e.ID, e.File, err)
		case res.verdict == killed:
			nKilled++
			out.printf("mutate: %s (%s): killed by %s (%s)\n", e.ID, e.File, strings.Join(res.by, ", "), time.Since(start).Round(time.Second))
		default:
			code, nNotKilled = 1, nNotKilled+1
			errOut.printf("mutate: %s (%s): %s%s; see %s\n", e.ID, e.File, res.verdict, res.whyText(), res.log)
		}
	}
	out.printf("mutate: %d curated mutant(s) run: %d killed, %d not killed; %d error(s), counting no mutant; %d skipped (file not written yet)\n",
		nKilled+nNotKilled, nKilled, nNotKilled, nErrors, nSkipped)
	return code
}

// runCurated runs one entry: the edit, the named tests on the unmutated code (each must pass),
// then on the mutant. An error (a bad entry, a test name that matches nothing, a failing
// baseline) counts no mutant.
func runCurated(ctx context.Context, cfg config, idx *testIndex, e curatedEntry) (result, error) {
	content, err := os.ReadFile(filepath.Join(cfg.root, filepath.FromSlash(e.File)))
	if err != nil {
		return result{}, err
	}
	mutated, err := applyLineEdit(content, e.From, e.To)
	if err != nil {
		return result{}, err
	}
	m := mutant{id: e.ID, file: e.File, desc: "curated " + e.ID, content: mutated}
	if len(e.Tests) == 0 {
		return result{}, errors.New("no tests named")
	}
	if isPyTest(e.Tests[0]) {
		return runPyMutant(ctx, cfg, m, e.Tests)
	}
	runs, err := idx.runsFor(ctx, e.Tests)
	if err != nil {
		return result{}, err
	}
	return runGoMutant(ctx, cfg, m, runs, e.Tests)
}
