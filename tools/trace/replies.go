package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The reply-code closure of DESIGN §C3 (plan S4 step 15): `trace replies [--upto <id>]`.
// The reply table below is transcribed from DESIGN §A3.3 and §A4.1–§A4.9, never generated
// from the scripts. The check fails unless (a) every reply literal in internal/scripts/*.lua
// is in the table, (b) every non-empty reason of the table is in those DESIGN sections, and
// (c) from S4 on, every row is observed in .out/replies-l2.jsonl. Rows of a script with no
// .lua file are skipped throughout.

const (
	// repliesLog is where scriptrun.Run appends {script, code, reason} after each decoded
	// L2 reply (plan S4 step 14).
	repliesLog = ".out/replies-l2.jsonl"
	// repliesTarget is the make target whose runs fill repliesLog.
	repliesTarget = "test-scripts"
	// repliesTableRel names the table in problems that point at it.
	repliesTableRel = "tools/trace/replies.go"
	designRel       = "DESIGN.md"
)

var (
	// designHeadingRe is a Markdown heading: its level and its first word (the section number).
	designHeadingRe = regexp.MustCompile(`^(#{1,6})\s+(\S+)`)
	// designSectionRe names the sections (b) reads: §A3.3 and §A4.1–§A4.9.
	designSectionRe = regexp.MustCompile(`^(A3\.3|A4\.[1-9])$`)
	// quotedTokenRe is a '…' string holding one token.
	quotedTokenRe = regexp.MustCompile(`'([A-Za-z0-9_]+)'`)
	// bareTokenRe is a backtick span that is one token.
	bareTokenRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	// decimalRe is a Lua numeral that is a plain non-negative integer.
	decimalRe = regexp.MustCompile(`^[0-9]+$`)
)

// replyCode is one reply code of a script and the reasons it comes with ("" = none).
type replyCode struct {
	code    int
	reasons []string
}

// replySource is a non-literal code or reason in a script's code: the Lua name that holds
// it, and the codes and reasons it can hold (plan S4 step 15 (a)).
type replySource struct {
	name  string
	codes []replyCode
}

// replyScript is one script's part of the reply table: script → code → reasons, and the
// sets its non-literal codes and reasons expand to.
type replyScript struct {
	script  string
	codes   []replyCode
	sources []replySource
}

// replyTable is the reply table (DESIGN §A3.3, §A4.1–§A4.9; plan S4 step 15) for the scripts
// written so far; S6–S8 add the rows of the scripts they write. No call-graph expansion: each
// script's rows say which replies it can give, the prelude's included.
func replyTable() []replyScript {
	// load_epoch's reasons (§A4.2), E_INTEGRITY_EPOCH's list in §A4.9; integrity_hold is also
	// the reply of hold(…).
	loadEpoch := []string{"epoch_record_missing", "epoch_record_malformed", "hwm_missing", "hwm_malformed", "integrity_hold"}
	// check_key's quarantine reasons, the §A3.3 rows top to bottom.
	checkKey := []string{
		"marker_index_without_bucket", "index_without_bucket", "malformed_bucket", "marker_without_bucket",
		"malformed_marker", "marker_count", "marker_index_mismatch", "bucket_without_index", "malformed_index",
	}
	// The record checks admin_status and admin_inspect share (§A4.6).
	record := []string{"epoch_record_missing", "epoch_record_malformed", "epoch_changed"}
	none := []string{""}
	return []replyScript{
		{
			script: "acquire", // §A4.1 (keys_mismatch), §A4.2 (the prelude), §A4.3, §A3.3, §A4.9
			codes: []replyCode{
				{1, none}, // E_ALLOWED
				{2, none}, // E_LIMITED
				{20, []string{"epoch_or_fingerprint"}},
				{21, []string{"clock"}},
				{22, loadEpoch},
				{23, slices.Concat([]string{"quarantined"}, checkKey)},
				{24, []string{"tracked_buckets"}},
				{25, []string{"keys_mismatch"}},
			},
			sources: []replySource{
				{"why", []replyCode{{22, loadEpoch}}}, // load_epoch's reason, in the prelude's enter
				{"b", []replyCode{{23, checkKey}}},    // quarantine(b): check_key's reason
			},
		},
		{
			script: "read_epoch", // §A4.9: state 0 = healthy, 22 = epoch-wide problem
			codes:  []replyCode{{0, none}, {22, loadEpoch}},
			sources: []replySource{
				{"state", []replyCode{{0, none}, {22, loadEpoch}}}, // with '' or load_epoch's reason
			},
		},
		{
			script: "hold", // §A4.9
			codes:  []replyCode{{40, none}, {41, []string{"epoch_changed"}}},
		},
		{
			script: "admin_start_epoch", // §A4.6
			codes: []replyCode{
				{21, []string{"clock"}},
				{40, none},
				{41, []string{"epoch_exists", "epoch_record_missing", "epoch_record_malformed", "epoch_changed"}},
			},
		},
		{
			script: "admin_status", // §A4.6, §A4.8 rule 2
			codes:  []replyCode{{40, none}, {41, slices.Concat(record, []string{"shared_key_malformed"})}},
		},
		{
			script: "admin_inspect", // §A4.1 (keys_mismatch), §A4.6, §A4.8 rule 2
			codes: []replyCode{
				{25, []string{"keys_mismatch"}},
				{40, none},
				{41, slices.Concat(record, []string{"shared_key_malformed"})},
			},
		},
	}
}

// replyDatum is a local variable whose tables a return gives as data, never as a reply: the
// .lua file, the function (its name as luaFuncName gives it) and the variable.
type replyDatum struct{ file, fn, name string }

// replyData is the checker's own list of returned data (plan choice, S4 repair F1). Check (a)
// reads every table a return can give as a reply (luaFlow), so a function that returns a map
// or a record names its variable here. An entry exempts only the tables built empty or from
// name = value fields that the variable holds, and its assignments by index; every other
// table in the function, or held by the variable, is still read. S6–S8 add the helpers of the
// scripts they write.
func replyData() []replyDatum {
	return []replyDatum{
		{"prelude.lua", "to_map", "m"},    // HGETALL's field → value map (§A4.2)
		{"prelude.lua", "check_key", "b"}, // the bucket check_key returns with 'ok' (§A3.3)
	}
}

// replyRow is one (code, reason) a script can reply with.
type replyRow struct {
	code   int
	reason string
}

func (r replyRow) String() string { return fmt.Sprintf("%d '%s'", r.code, r.reason) }

func flatRows(codes []replyCode) []replyRow {
	var rows []replyRow
	for _, c := range codes {
		for _, reason := range c.reasons {
			rows = append(rows, replyRow{c.code, reason})
		}
	}
	return rows
}

// tabled is one script of the table whose .lua file exists.
type tabled struct {
	rows    map[replyRow]bool
	order   []replyRow
	sources map[string][]replyRow
}

func checkReplies(ctx context.Context, e *env, args []string) (report, error) {
	fs := flag.NewFlagSet("replies", flag.ContinueOnError)
	upto := fs.String("upto", "", "last slice: (c) runs when "+repliesTarget+" is among its targets")
	if err := parseFlags(fs, e, args); err != nil {
		return report{}, err
	}
	observe := false
	if *upto != "" {
		ts, err := targetsUpTo(*upto)
		if err != nil {
			fmt.Fprintf(e.stderr, "replies: --upto: %v\n", err)
			return report{}, errUsage
		}
		observe = slices.Contains(ts, repliesTarget)
	}
	return replyProblems(ctx, e.root, replyTable(), observe, *upto)
}

// replyProblems runs (a), (b) and, when observe is set, (c) against root with table.
func replyProblems(ctx context.Context, root string, table []replyScript, observe bool, upto string) (report, error) {
	luaFiles, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(scriptsDir), "*.lua"))
	if err != nil {
		return report{}, fmt.Errorf("listing %s: %w", scriptsDir, err)
	}
	sort.Strings(luaFiles)
	var r report
	existing := map[string]bool{} // script names with a .lua file, prelude.lua aside
	var files []string            // root-relative .lua paths
	hasPrelude := false
	for _, p := range luaFiles {
		rel := scriptsDir + "/" + filepath.Base(p)
		files = append(files, rel)
		if rel == preludeRel {
			hasPrelude = true
			continue
		}
		existing[strings.TrimSuffix(path.Base(rel), ".lua")] = true
	}
	scripts := map[string]*tabled{}
	seen := map[string]bool{}
	for _, s := range table {
		if seen[s.script] {
			r.problems = append(r.problems, fmt.Sprintf("%s: %s has two entries in the reply table", repliesTableRel, s.script))
			continue
		}
		seen[s.script] = true
		if !existing[s.script] {
			continue // not written yet: skipped
		}
		ts := &tabled{rows: map[replyRow]bool{}, sources: map[string][]replyRow{}}
		for _, row := range flatRows(s.codes) {
			if !ts.rows[row] {
				ts.rows[row] = true
				ts.order = append(ts.order, row)
			}
		}
		for _, src := range s.sources {
			rows := flatRows(src.codes)
			ts.sources[src.name] = rows
			for _, row := range rows {
				if !ts.rows[row] {
					r.problems = append(r.problems, fmt.Sprintf("%s: %s's source %s holds %s, which is not in %s's rows",
						repliesTableRel, s.script, src.name, row, s.script))
				}
			}
		}
		scripts[s.script] = ts
	}

	// (a) every reply literal of every .lua file is in the table.
	consts := map[string]int{}
	if hasPrelude {
		src, err := readFile(ctx, root, preludeRel)
		if err != nil {
			return report{}, err
		}
		if consts, err = luaIntConstants(string(src)); err != nil {
			return report{}, fmt.Errorf("%s: %w", preludeRel, err)
		}
	} else if len(existing) > 0 {
		r.problems = append(r.problems, fmt.Sprintf("%s: missing, so the reply code names of the scripts cannot resolve", preludeRel))
	}
	data := map[string]map[string]bool{} // .lua file → "function.variable" of replyData
	for _, d := range replyData() {
		if data[d.file] == nil {
			data[d.file] = map[string]bool{}
		}
		data[d.file][d.fn+"."+d.name] = true
	}
	sites := 0
	for _, rel := range files {
		src, err := readFile(ctx, root, rel)
		if err != nil {
			return report{}, err
		}
		found, unreadable, err := luaReplySites(string(src), consts, rel == preludeRel, data[path.Base(rel)])
		if err != nil {
			return report{}, fmt.Errorf("%s: %w", rel, err)
		}
		for _, u := range unreadable {
			r.problems = append(r.problems, fmt.Sprintf("%s:%d: %s", rel, u.line, u.msg))
		}
		sites += len(found)
		if rel == preludeRel {
			for _, s := range found {
				if msg := preludeSiteProblem(s, scripts); msg != "" {
					r.problems = append(r.problems, fmt.Sprintf("%s:%d: %s", rel, s.line, msg))
				}
			}
			continue
		}
		name := strings.TrimSuffix(path.Base(rel), ".lua")
		ts, ok := scripts[name]
		if !ok {
			r.problems = append(r.problems, fmt.Sprintf("%s:1: %s has no rows in the reply table (%s)", rel, name, repliesTableRel))
			continue
		}
		for _, s := range found {
			if msg := scriptSiteProblem(s, name, ts); msg != "" {
				r.problems = append(r.problems, fmt.Sprintf("%s:%d: %s", rel, s.line, msg))
			}
		}
	}

	// (b) every non-empty reason of the table is in DESIGN §A3.3 or §A4.1–§A4.9.
	names := make([]string, 0, len(scripts))
	for n := range scripts {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 0 {
		design, err := readFile(ctx, root, designRel)
		if err != nil {
			return report{}, err
		}
		known, missing := designReasons(string(design))
		for _, sec := range missing {
			r.problems = append(r.problems, fmt.Sprintf("%s: no section %s, which the reasons must come from", designRel, sec))
		}
		for _, n := range names {
			for _, row := range scripts[n].order {
				if row.reason != "" && !known[row.reason] {
					r.problems = append(r.problems, fmt.Sprintf("%s: %s %s: the reason is not in DESIGN §A3.3 or §A4.1–§A4.9 as a backticked token or a quoted '…' string in a backtick span or Lua block",
						repliesTableRel, n, row))
				}
			}
		}
	}

	rows := 0
	for _, n := range names {
		rows += len(scripts[n].order)
	}
	r.out = append(r.out, fmt.Sprintf("replies: %d reply site(s) in %d .lua file(s); %d row(s) of %d script(s) with a .lua file", sites, len(files), rows, len(names)))

	// (c) every row of an existing script is observed, and every observation is a row.
	if !observe {
		if upto != "" {
			r.out = append(r.out, fmt.Sprintf("replies: (c) skipped: %s is not among the targets up to %s", repliesTarget, upto))
		}
		return r, nil
	}
	seenRows, present, problems, err := observedReplies(ctx, root)
	if err != nil {
		return report{}, err
	}
	r.problems = append(r.problems, problems...)
	if !present {
		return r, nil
	}
	observed := 0
	for _, n := range names {
		for _, row := range scripts[n].order {
			if seenRows[n][row] {
				observed++
				continue
			}
			r.problems = append(r.problems, fmt.Sprintf("%s: %s %s: never observed in %s (DESIGN §C3 (c))", repliesTableRel, n, row, repliesLog))
		}
		var extra []replyRow
		for row := range seenRows[n] {
			if !scripts[n].rows[row] {
				extra = append(extra, row)
			}
		}
		slices.SortFunc(extra, func(a, b replyRow) int {
			if a.code != b.code {
				return a.code - b.code
			}
			return strings.Compare(a.reason, b.reason)
		})
		for _, row := range extra {
			r.problems = append(r.problems, fmt.Sprintf("%s: observed %s %s, which is not in %s's rows of the reply table", repliesLog, n, row, n))
		}
	}
	r.out = append(r.out, fmt.Sprintf("replies: (c) %d of %d row(s) observed in %s", observed, rows, repliesLog))
	return r, nil
}

// scriptSiteProblem checks one reply site of script name's own file against its rows.
func scriptSiteProblem(s replySite, name string, ts *tabled) string {
	src := s.source()
	if src == "" {
		if ts.rows[replyRow{s.code, s.reason}] {
			return ""
		}
		return fmt.Sprintf("%s replies %s, which is not in %s's rows of the reply table (%s)", s.form, replyRow{s.code, s.reason}, name, repliesTableRel)
	}
	rows, ok := ts.sources[src]
	if !ok {
		return fmt.Sprintf("%s replies through %s, a non-literal for which the reply table declares no set for %s", s.form, src, name)
	}
	switch {
	case s.codeName == "":
		for _, row := range rows {
			if row.code != s.code {
				return fmt.Sprintf("%s replies code %d, but %s's source %s holds %s", s.form, s.code, name, src, row)
			}
		}
	case s.reasonName == "":
		if row, ok := missingReasonRow(s.reason, rows, ts); ok {
			return fmt.Sprintf("%s replies through %s with reason '%s', but %s's rows lack %s", s.form, src, s.reason, name, row)
		}
	}
	return ""
}

// missingReasonRow is the first (code, reason) that a site with the non-literal code of a
// source and a literal reason can give, for a code the source holds, which ts's rows lack.
func missingReasonRow(reason string, rows []replyRow, ts *tabled) (replyRow, bool) {
	for _, row := range rows {
		if want := (replyRow{row.code, reason}); !ts.rows[want] {
			return want, true
		}
	}
	return replyRow{}, false
}

// preludeSiteProblem checks one reply site of prelude.lua: it must be in the rows (or the
// declared sources) of at least one script with a .lua file.
func preludeSiteProblem(s replySite, scripts map[string]*tabled) string {
	src := s.source()
	for _, ts := range scripts {
		if src == "" {
			if ts.rows[replyRow{s.code, s.reason}] {
				return ""
			}
			continue
		}
		rows, ok := ts.sources[src]
		if !ok {
			continue
		}
		switch {
		case s.codeName == "":
			if !slices.ContainsFunc(rows, func(row replyRow) bool { return row.code != s.code }) {
				return ""
			}
		case s.reasonName == "":
			if _, missing := missingReasonRow(s.reason, rows, ts); !missing {
				return ""
			}
		default:
			return ""
		}
	}
	switch {
	case src == "":
		return fmt.Sprintf("%s replies %s, which no script with a .lua file lists in the reply table (%s)", s.form, replyRow{s.code, s.reason}, repliesTableRel)
	case s.codeName == "":
		return fmt.Sprintf("%s replies through %s, which no script with a .lua file declares with code %d in the reply table (%s)", s.form, src, s.code, repliesTableRel)
	case s.reasonName == "":
		return fmt.Sprintf("%s replies through %s with reason '%s', which no script with a .lua file declares with a row of that reason for every code %s holds (%s)",
			s.form, src, s.reason, src, repliesTableRel)
	}
	return fmt.Sprintf("%s replies through %s, which no script with a .lua file declares in the reply table (%s)", s.form, src, repliesTableRel)
}

// observedReplies reads repliesLog: script → the rows observed. A missing log is one problem,
// with present false; a malformed line is a problem naming it.
func observedReplies(ctx context.Context, root string) (map[string]map[replyRow]bool, bool, []string, error) {
	data, err := readFile(ctx, root, repliesLog)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, []string{fmt.Sprintf("%s: missing: make %s fills it (DESIGN §C3 (c))", repliesLog, repliesTarget)}, nil
		}
		return nil, false, nil, err
	}
	seen := map[string]map[replyRow]bool{}
	var problems []string
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		script, row, err := parseReplyLine(line)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s:%d: %v", repliesLog, i+1, err))
			continue
		}
		if seen[script] == nil {
			seen[script] = map[replyRow]bool{}
		}
		seen[script][row] = true
	}
	return seen, true, problems, nil
}

// parseReplyLine reads one line of repliesLog: exactly {"script": string, "code": integer,
// "reason": string}, the code a reply code (0 … 99).
func parseReplyLine(line string) (string, replyRow, error) {
	var v struct {
		Script *string `json:"script"`
		Code   *int    `json:"code"`
		Reason *string `json:"reason"`
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return "", replyRow{}, fmt.Errorf("not a {script, code, reason} line: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", replyRow{}, errors.New("text after the JSON object")
	}
	switch {
	case v.Script == nil || v.Code == nil || v.Reason == nil:
		return "", replyRow{}, errors.New("needs script, code and reason")
	case *v.Script == "":
		return "", replyRow{}, errors.New("empty script name")
	case *v.Code < 0 || *v.Code > 99:
		return "", replyRow{}, fmt.Errorf("code %d is not a reply code", *v.Code)
	}
	return *v.Script, replyRow{*v.Code, *v.Reason}, nil
}

// designReasons returns every token DESIGN §A3.3 and §A4.1–§A4.9 give as a backticked token,
// or as a quoted '…' string in a backtick span or a Lua block, and the sections it did not find.
func designReasons(text string) (map[string]bool, []string) {
	known := map[string]bool{}
	found := map[string]bool{}
	in := false
	fence, lua := "", false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, "`~") == "" {
				fence = ""
				continue
			}
			if in && lua {
				for _, m := range quotedTokenRe.FindAllStringSubmatch(line, -1) {
					known[m[1]] = true
				}
			}
			continue
		}
		if f := fenceOpener(trimmed); f != "" {
			fence = f
			lua = strings.TrimSpace(strings.TrimLeft(trimmed, "`~")) == "lua"
			continue
		}
		if m := designHeadingRe.FindStringSubmatch(line); m != nil {
			if designSectionRe.MatchString(m[2]) {
				in = true
				found[m[2]] = true
			} else if len(m[1]) <= 3 {
				in = false
			}
			continue
		}
		if !in {
			continue
		}
		for _, span := range backtickSpans(line) {
			if t := strings.TrimSpace(span); bareTokenRe.MatchString(t) {
				known[t] = true
			}
			for _, m := range quotedTokenRe.FindAllStringSubmatch(span, -1) {
				known[m[1]] = true
			}
		}
	}
	var missing []string
	for _, sec := range []string{"A3.3", "A4.1", "A4.2", "A4.3", "A4.4", "A4.5", "A4.6", "A4.7", "A4.8", "A4.9"} {
		if !found[sec] {
			missing = append(missing, "§"+sec)
		}
	}
	return known, missing
}

// fenceOpener returns the fence (``` or ~~~, at least three) that opens a code block on
// this trimmed line, or "".
func fenceOpener(trimmed string) string {
	for _, c := range []string{"`", "~"} {
		n := len(trimmed) - len(strings.TrimLeft(trimmed, c))
		if n >= 3 {
			return strings.Repeat(c, n)
		}
	}
	return ""
}

// backtickSpans returns the contents of the code spans on one line: a run of n backticks
// opens a span that the next run of exactly n closes; an unclosed run is plain text.
func backtickSpans(line string) []string {
	var spans []string
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		n := j - i
		closed := false
		for k := j; k < len(line); {
			if line[k] != '`' {
				k++
				continue
			}
			m := k
			for m < len(line) && line[m] == '`' {
				m++
			}
			if m-k == n {
				spans = append(spans, line[j:k])
				i, closed = m, true
				break
			}
			k = m
		}
		if !closed {
			i = j
		}
	}
	return spans
}

// luaIntConstants returns the top-level `local NAME = <integer>` constants of a Lua file
// (the prelude's reply code names among them).
func luaIntConstants(src string) (map[string]int, error) {
	toks, err := luaLex(src)
	if err != nil {
		return nil, err
	}
	consts := map[string]int{}
	depth := 0
	for i, t := range toks {
		if t.kind == 'n' {
			switch t.text {
			case "function", "if", "do", "repeat":
				depth++
			case "end", "until":
				depth--
			}
		}
		if depth != 0 || t.kind != 'n' || t.text != "local" || i+4 > len(toks) {
			continue
		}
		name, eq, num := toks[i+1], toks[i+2], toks[i+3]
		if name.kind != 'n' || luaKeyword(name.text) || eq.text != "=" || num.kind != 'p' || !decimalRe.MatchString(num.text) {
			continue
		}
		if i+4 < len(toks) && toks[i+4].kind == 'p' && toks[i+4].text != "(" && toks[i+4].text != ";" {
			continue // part of a longer expression or list
		}
		v, err := strconv.Atoi(num.text)
		if err != nil {
			continue // too large for a reply code
		}
		consts[name.text] = v
	}
	return consts, nil
}

// replySite is one reply a Lua file can give, as (a) reads it.
type replySite struct {
	line       int
	form       string // what it was read from, for messages
	code       int
	codeName   string // the name holding a non-literal code; "" when literal
	reason     string
	reasonName string // the name holding a non-literal reason; "" when literal
	param      string // the parameter of the enclosing function its code or reason is (a template); ""
}

// source is the name a non-literal site expands through: its code's, else its reason's;
// "" for a literal site.
func (s replySite) source() string {
	if s.codeName != "" {
		return s.codeName
	}
	return s.reasonName
}

// siteProblem is a reply (a) cannot read.
type siteProblem struct {
	line int
	msg  string
}

// luaFunc is one function of a Lua file: its name, for messages and the prelude's template
// helpers, and its parameter names.
type luaFunc struct {
	name   string
	params map[string]bool
}

// luaScopes returns, for each token, the innermost function it lies in (0: the file's top
// level, which is the script's own code) and each function (index 0: the top level).
func luaScopes(toks []luaTok) ([]int, []luaFunc) {
	fnOf := make([]int, len(toks))
	fns := []luaFunc{{name: "the top level"}}
	var open []int // the open blocks: a function's number, or 0 for if, do and repeat
	for i, t := range toks {
		if t.kind == 'n' {
			switch t.text {
			case "function":
				fns = append(fns, luaFunc{name: luaFuncName(toks, i), params: luaParams(toks, i)})
				open = append(open, len(fns)-1)
			case "if", "do", "repeat":
				open = append(open, 0)
			case "end", "until":
				if len(open) > 0 {
					open = open[:len(open)-1]
				}
			}
		}
		for k := len(open) - 1; k >= 0; k-- {
			if open[k] > 0 {
				fnOf[i] = open[k]
				break
			}
		}
	}
	return fnOf, fns
}

// templateHelper reports whether name is one of the prelude's helpers whose bodies build a reply
// from their parameters: templates, read at their calls (plan S4 step 15 (a)). A reply built
// from a parameter anywhere else is one (a) cannot read.
func templateHelper(name string) bool {
	return name == "reply" || name == "finish" || name == "quarantine" || name == "hold"
}

// luaReplySites returns the replies of one Lua file (plan S4 step 15 (a)); prelude is set for
// prelude.lua, and data holds the "function.variable" entries of replyData for the file:
//   - reply(code, reason, …) and finish(code, reason, …);
//   - quarantine(reason), code 23, and hold(…), {22, 0, 'integrity_hold'};
//   - a table constructor whose first element is one of consts, wherever it is (local out =
//     {OK, …}); and every table constructor a return can give (luaFlow: return {…}, return false,
//     {…}, an operand of and/or, a local returned anywhere in the file that the table is assigned
//     to), at any depth. Its first element is the code, its third the reason. A table no return
//     gives, with no code name first, is data.
//
// A code is an integer or a name in consts; a reason is a string literal. Any other name is
// a non-literal, which the table must declare. A reply built from a parameter of the enclosing
// function (its code or its reason, or a returned table's first element) is a template: inside
// the prelude's reply, finish, quarantine and hold (quarantine's {E_INTEGRITY_KEY, T, reason}) it
// is read at the helper's calls; anywhere else (a script's refusal helper, a prelude helper of
// another name) it is a reply the check cannot read, so it fails. So are (S4 repair F1): a
// returned table whose first field is no code (an expression, a string, a name = value field)
// or that is built empty (a reply filled in by index); any use of reply, finish, quarantine or
// hold but a call with parentheses, their definitions, a field of that name and a table key
// (an alias, a value, a call with a string or table and no parentheses); and what luaFlow cannot
// read. A statement the walk cannot parse is reported only on a line with no other problem,
// where the bracket that broke it already is.
func luaReplySites(src string, consts map[string]int, prelude bool, data map[string]bool) ([]replySite, []siteProblem, error) {
	toks, err := luaLex(src)
	if err != nil {
		return nil, nil, err
	}
	at := func(i int, kind byte, text string) bool {
		return i >= 0 && i < len(toks) && toks[i].kind == kind && (text == "" || toks[i].text == text)
	}
	fnOf, fns := luaScopes(toks)
	flow := luaFlowOf(toks)
	returned, flowProblems := flow.returned(func(decl int) bool {
		d := flow.decls[decl]
		return d.fn < len(fns) && data[fns[d.fn].name+"."+d.name]
	}, consts)
	var sites []replySite
	var problems []siteProblem
	// add keeps one read reply: a problem, a template (skipped only in the prelude's template
	// helpers) or a site.
	add := func(line int, form string, site replySite, problem string, keep bool, fn luaFunc) {
		switch {
		case problem != "":
			problems = append(problems, siteProblem{line, form + ": " + problem})
		case !keep:
		case site.param != "" && prelude && templateHelper(fn.name):
			// the helper's own body, read at its calls
		case site.param != "":
			problems = append(problems, siteProblem{line, fmt.Sprintf("%s: cannot read: reply built from parameter %s of %s (only the prelude's reply, finish, quarantine and hold are templates)",
				form, site.param, fn.name)})
		default:
			site.line, site.form = line, form
			sites = append(sites, site)
		}
	}
	for i, t := range toks {
		switch {
		case t.kind == 'n' && templateHelper(t.text):
			tableKey := flow.inTable[i] && (at(i-1, 'p', "{") || at(i-1, 'p', ",") || at(i-1, 'p', ";")) && soleEq(toks, i+1)
			if at(i-1, 'p', ".") || at(i-1, 'p', ":") || at(i-1, 'n', "function") || tableKey {
				continue // a field or a key of that name, or a definition
			}
			if !at(i+1, 'p', "(") {
				problems = append(problems, siteProblem{t.line, fmt.Sprintf("%s: cannot read: %s is used other than in a call with parentheses (as a value, through an alias, or called with a string or table)", t.text, t.text)})
				continue
			}
			args, ok := splitList(toks, i+1, ")")
			form := t.text + "(…)"
			if !ok {
				problems = append(problems, siteProblem{t.line, form + ": cannot read its arguments"})
				continue
			}
			fn := fns[fnOf[i]]
			site, problem, keep := callSite(t.text, args, consts, fn.params)
			add(t.line, form, site, problem, keep, fn)
		case t.kind == 'p' && t.text == "{":
			fields, ok := splitList(toks, i, "}")
			if !ok {
				problems = append(problems, siteProblem{t.line, "{…}: cannot read the table"})
				continue
			}
			f := fnOf[i]
			site, problem, keep := tableSite(fields, returned[i], consts, fns[f].params)
			add(t.line, "{…}", site, problem, keep, fns[f])
		}
	}
	problems = append(problems, flowProblems...)
	problems = append(problems, flow.sinks...)
	lines := map[int]bool{}
	for _, p := range problems {
		lines[p.line] = true
	}
	for _, p := range flow.unread {
		if !lines[p.line] {
			problems = append(problems, p)
		}
	}
	return sites, problems, nil
}

// elemKind is what one element of a reply is.
type elemKind int

const (
	elemOther  elemKind = iota // an expression (a reply cannot be read from it)
	elemNumber                 // an integer literal
	elemConst                  // a name in consts
	elemString                 // a string literal
	elemParam                  // a parameter of the enclosing function: a template
	elemName                   // any other name: a non-literal
)

func classifyElem(e []luaTok, consts map[string]int, params map[string]bool) (elemKind, int) {
	if len(e) != 1 {
		return elemOther, 0
	}
	t := e[0]
	switch {
	case t.kind == 's':
		return elemString, 0
	case t.kind == 'p' && decimalRe.MatchString(t.text):
		v, err := strconv.Atoi(t.text)
		if err != nil {
			return elemOther, 0
		}
		return elemNumber, v
	case t.kind != 'n' || luaKeyword(t.text):
		return elemOther, 0
	case params[t.text]:
		return elemParam, 0
	}
	if v, ok := consts[t.text]; ok {
		return elemConst, v
	}
	return elemName, 0
}

// codeReason fills a site's code and reason from their elements; a code or reason that is a
// parameter of the enclosing function makes the site a template (param set).
func codeReason(code, reason []luaTok, consts map[string]int, params map[string]bool) (replySite, string, bool) {
	var s replySite
	ck, cv := classifyElem(code, consts, params)
	rk, _ := classifyElem(reason, consts, params)
	switch {
	case ck == elemParam:
		return replySite{param: code[0].text}, "", true
	case rk == elemParam:
		return replySite{param: reason[0].text}, "", true
	case ck == elemOther || ck == elemString:
		return s, "the code is neither an integer nor a name", false
	case rk != elemString && rk != elemName:
		return s, "the reason is neither a string literal nor a name", false
	}
	if ck == elemName {
		s.codeName = code[0].text
	} else {
		s.code = cv
	}
	if rk == elemName {
		s.reasonName = reason[0].text
	} else {
		s.reason = reason[0].text
	}
	return s, "", true
}

// callSite reads a call of reply, finish, quarantine or hold.
func callSite(fn string, args [][]luaTok, consts map[string]int, params map[string]bool) (replySite, string, bool) {
	switch fn {
	case "hold":
		if len(args) < 1 {
			return replySite{}, "needs the hold's reason", false
		}
		return replySite{code: 22, reason: "integrity_hold"}, "", true
	case "quarantine":
		if len(args) < 1 {
			return replySite{}, "needs a reason", false
		}
		return codeReason([]luaTok{{kind: 'p', text: "23"}}, args[0], consts, params)
	}
	if len(args) < 2 {
		return replySite{}, "needs a code and a reason", false
	}
	return codeReason(args[0], args[1], consts, params)
}

// tableSite reads a table constructor's fields; ret is set when a return can give it
// (luaFlow). keep is false when it is no reply: a table no return gives, unless a code name
// comes first. A returned table must be read as a reply, or it is a problem.
func tableSite(fields [][]luaTok, ret bool, consts map[string]int, params map[string]bool) (replySite, string, bool) {
	if len(fields) == 0 {
		if ret {
			return replySite{}, "cannot read: a returned table built empty (a reply filled in by index cannot be read)", false
		}
		return replySite{}, "", false
	}
	// A named first field (x = …, […] = …) is more than one token, so it is no code.
	switch k, _ := classifyElem(fields[0], consts, params); {
	case k == elemConst:
	case !ret:
		return replySite{}, "", false // no return gives it: data
	case k == elemNumber, k == elemName:
	case k == elemParam:
		return replySite{param: fields[0][0].text}, "", true // a reply built from a parameter: a template
	default:
		return replySite{}, "cannot read: a returned table whose first field is no code (an integer, a code name or a name the reply table declares)", false
	}
	if len(fields) < 3 {
		return replySite{}, "a reply needs a code, t_us and a reason", false
	}
	return codeReason(fields[0], fields[2], consts, params)
}

// splitList splits the bracketed list opening at toks[open] into its top-level elements
// (split at ',' or ';'), up to the matching closer; ok is false when there is none.
func splitList(toks []luaTok, open int, closer string) ([][]luaTok, bool) {
	var out [][]luaTok
	var cur []luaTok
	depth := 0
	for i := open + 1; i < len(toks); i++ {
		t := toks[i]
		if t.kind == 'p' {
			switch t.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				if depth == 0 {
					if t.text != closer {
						return nil, false
					}
					if len(cur) > 0 || len(out) > 0 {
						out = append(out, cur)
					}
					return out, true
				}
				depth--
			case ",", ";": // ';' separates fields of a table; a call never has one at its level
				if depth == 0 {
					out = append(out, cur)
					cur = nil
					continue
				}
			}
		}
		cur = append(cur, t)
	}
	return nil, false
}

func blockEnd(word string) bool {
	return word == "end" || word == "else" || word == "elseif" || word == "until"
}

// luaFuncName names the function whose keyword is toks[i]: NAME in function NAME(…), local
// function NAME(…) and local NAME = function(…), a.b in function a.b(…); "an anonymous function"
// otherwise.
func luaFuncName(toks []luaTok, i int) string {
	var name strings.Builder
	for j := i + 1; j < len(toks) && (toks[j].kind != 'p' || toks[j].text != "("); j++ {
		name.WriteString(toks[j].text)
	}
	switch {
	case name.Len() > 0:
		return name.String()
	case i >= 2 && toks[i-1].kind == 'p' && toks[i-1].text == "=" && toks[i-2].kind == 'n':
		return toks[i-2].text
	}
	return "an anonymous function"
}

// luaParams returns the parameter names of the function whose keyword is toks[i].
func luaParams(toks []luaTok, i int) map[string]bool {
	params := map[string]bool{}
	j := i + 1
	for j < len(toks) && (toks[j].kind != 'p' || toks[j].text != "(") {
		j++ // the function's name: a, a.b, a:b
	}
	for j++; j < len(toks) && (toks[j].kind != 'p' || toks[j].text != ")"); j++ {
		if toks[j].kind == 'n' {
			params[toks[j].text] = true
		}
	}
	return params
}

func luaKeyword(word string) bool {
	switch word {
	case "and", "break", "do", "else", "elseif", "end", "false", "for", "function", "if", "in",
		"local", "nil", "not", "or", "repeat", "return", "then", "true", "until", "while":
		return true
	}
	return false
}
