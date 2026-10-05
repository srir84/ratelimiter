package main

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// The ACL users of DESIGN §A2: the client commands each user sends, and the inner
// commands its scripts call (the store checks the ACL on both).
const (
	libClient     = "evalsha script|load scan ping auth"
	libInner      = "hgetall hexists hset hdel hincrby hlen zscore zadd zrem zrange del unlink get time"
	adminClient   = "evalsha script|load ping auth info slowlog|get bgrewriteaof"
	adminInner    = "hgetall hget hexists hset hdel hincrby hlen zscore zadd zrem zrange zcard del get time"
	fixtureClient = "set get del hset hdel zrem zadd flushall ping auth type eval"
	opsClient     = "config|get config|set info bgrewriteaof shutdown ping auth"

	// keyPattern is the only key pattern; rl_ops has none.
	keyPattern = "~rl:*"
	// aclTemplate and scriptsDir are where the ACL file and the product's Lua live.
	aclTemplate = "deploy/acl.conf.tmpl"
	scriptsDir  = "internal/scripts"
)

// aclUser is one row of the §A2 ACL table.
type aclUser struct {
	name     string
	password string // the template field holding its password (S0 step 3)
	client   []string
	inner    []string
	keys     bool // has the ~rl:* key pattern
}

func aclUsers() []aclUser {
	return []aclUser{
		{name: "rl_lib", password: "LibPassword", client: strings.Fields(libClient), inner: strings.Fields(libInner), keys: true},
		{name: "rl_admin", password: "AdminPassword", client: strings.Fields(adminClient), inner: strings.Fields(adminInner), keys: true},
		{name: "rl_fixture", password: "FixturePassword", client: strings.Fields(fixtureClient), keys: true},
		{name: "rl_ops", password: "OpsPassword", client: strings.Fields(opsClient)},
	}
}

func aclUserByName(name string) (aclUser, bool) {
	for _, u := range aclUsers() {
		if u.name == name {
			return u, true
		}
	}
	return aclUser{}, false
}

// scriptNames lists the 12 scripts of DESIGN §A4.1.
func scriptNames() []string {
	return []string{
		"acquire", "refund", "due_entries", "cleanup_entry", "reclaim", "read_epoch", "hold",
		"admin_start_epoch", "admin_restore", "admin_clear", "admin_status", "admin_inspect",
	}
}

// scriptUsers maps a §A4.1 script to the users that run it (§A2): rl_lib runs 7 and
// rl_admin 6, read_epoch being in both. Any other name has no user.
func scriptUsers(script string) []string {
	switch script {
	case "acquire", "refund", "due_entries", "cleanup_entry", "reclaim", "hold":
		return []string{"rl_lib"}
	case "read_epoch":
		return []string{"rl_lib", "rl_admin"}
	case "admin_start_epoch", "admin_status", "admin_inspect", "admin_restore", "admin_clear":
		return []string{"rl_admin"}
	}
	return nil
}

// checkACL is `trace acl` (§A2): the template's lines and lists, and each script's
// inner commands against its user's list.
func checkACL(ctx context.Context, e *env, args []string) (report, error) {
	if err := parseFlags(flag.NewFlagSet("acl", flag.ContinueOnError), e, args); err != nil {
		return report{}, err
	}
	problems, err := aclTemplateProblems(ctx, filepath.Join(e.root, filepath.FromSlash(aclTemplate)))
	if err != nil {
		return report{}, err
	}
	scriptProblems, n, err := aclScriptProblems(ctx, e.root)
	if err != nil {
		return report{}, err
	}
	r := report{problems: append(problems, scriptProblems...)}
	if len(r.problems) == 0 {
		r.out = []string{fmt.Sprintf("acl: %s matches DESIGN §A2; %d script file(s) checked", aclTemplate, n)}
	}
	return r, nil
}

// aclTemplateProblems checks the template (S0 step 3, §A2): line 1 is exactly "user
// default off"; then one line per §A2 user, each exactly "user <name> on
// >{{.XPassword}} resetkeys ~rl:* -@all +<command> …": these rules in this order, each
// once, the user's own password field, the key pattern exactly ~rl:* (none for rl_ops),
// then only "+" tokens, which equal its §A2 client and inner commands, each once.
// Anything else fails, among them command categories, allcommands, all-keys patterns,
// %R~/%W~/%RW~ patterns, selectors "( … )" and any other template action, since the
// store loads the rendered file. Lines split into words as the store splits them
// (aclSplit), so a tab never separates two rules. No problem prints a token that can
// carry a password (redact), nor any part of a line holding a quote outside a template
// action (quoteOutsideActions), since a password is never formatted into an error or a
// log (AGENTS.md §1).
func aclTemplateProblems(ctx context.Context, path string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading ACL template: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading ACL template: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var problems []string
	add := func(line int, format string, a ...any) {
		problems = append(problems, fmt.Sprintf("%s:%d: %s", aclTemplate, line, fmt.Sprintf(format, a...)))
	}
	if lines[0] != "user default off" {
		add(1, "line 1 must be exactly %q (DESIGN §A2)", "user default off")
	}
	seen := map[string]bool{}
	see := func(line int, u aclUser) {
		if seen[u.name] {
			add(line, "user %s appears twice", u.name)
		}
		seen[u.name] = true
	}
	for i, line := range lines[1:] {
		n := i + 2
		fields := aclSplit(line)
		if len(fields) == 0 {
			continue
		}
		if quoteOutsideActions(line) {
			// The store cuts a quoted value into several rules at its spaces, and each piece
			// may be part of a password: no word of the line is shown.
			add(n, "a quote (\" or ') outside a template action: the store splits each line of an ACL file at spaces and reads a quote as part of a rule, and no DESIGN §A2 rule has one; the line is not shown, since a quoted value may hold a password")
			if len(fields) >= 2 && fields[0] == "user" {
				if u, ok := aclUserByName(fields[1]); ok {
					see(n, u)
				}
			}
			continue
		}
		if fields[0] != "user" || len(fields) < 2 {
			shown := make([]string, len(fields))
			for k, f := range fields {
				shown[k] = redact(f)
			}
			add(n, "not a user line: %q", strings.Join(shown, " "))
			continue
		}
		u, ok := aclUserByName(fields[1])
		if !ok {
			add(n, "unknown user %q (DESIGN §A2 has rl_lib, rl_admin, rl_fixture, rl_ops)", redact(fields[1]))
			continue
		}
		see(n, u)
		addN := func(format string, a ...any) { add(n, format, a...) }
		plus, keys := aclUserRules(u, fields[2:], addN)
		aclLineShape(u, fields[2:], keys, addN)
		missing, extra := setDiff(append(append([]string{}, u.client...), u.inner...), plus)
		for _, c := range missing {
			add(n, "%s: +%s is missing (DESIGN §A2)", u.name, c)
		}
		for _, c := range extra {
			add(n, "%s: %s is not in DESIGN §A2", u.name, redact("+"+c))
		}
		shownKeys := make([]string, len(keys))
		for k, key := range keys {
			shownKeys[k] = redact(key)
		}
		switch {
		case u.keys && (len(keys) != 1 || keys[0] != keyPattern):
			add(n, "%s: key patterns %v, want exactly %s", u.name, shownKeys, keyPattern)
		case !u.keys && len(keys) > 0:
			add(n, "%s: key patterns %v, want none", u.name, shownKeys)
		}
	}
	for _, u := range aclUsers() {
		if !seen[u.name] {
			problems = append(problems, fmt.Sprintf("%s: no line for user %s", aclTemplate, u.name))
		}
	}
	return problems, nil
}

// aclSplit splits one line of the ACL file into words the way the store loads an ACL
// file (Valkey's ACLLoadFromFile, checked against valkey-server 9.1.2): the line is
// trimmed of spaces, tabs, CRs and LFs and split at every single space; the first two
// words (user and the user name) stay as they are, even empty; each later word is a rule,
// trimmed of tabs, CRs and LFs, and an empty rule is a no-op the store skips. A tab
// separates nothing and a quote is an ordinary character: the store does not split an
// ACL file as sdssplitargs does, so ">alpha beta" is the two rules `">alpha` and `beta"`.
func aclSplit(line string) []string {
	line = strings.Trim(line, " \t\r\n")
	if line == "" {
		return nil
	}
	var out []string
	for k, w := range strings.Split(line, " ") {
		if k >= 2 {
			if w = strings.Trim(w, "\t\r\n"); w == "" {
				continue
			}
		}
		out = append(out, w)
	}
	return out
}

// quoteOutsideActions reports whether line holds a quote, " or ', outside its template
// actions ({{ … }}, or to the end of the line when one is not closed): text/template
// consumes the quotes inside an action before the store reads the rendered file.
func quoteOutsideActions(line string) bool {
	for {
		start := strings.Index(line, "{{")
		text := line
		if start >= 0 {
			text = line[:start]
		}
		if strings.ContainsAny(text, `"'`) {
			return true
		}
		if start < 0 {
			return false
		}
		end := strings.Index(line[start+2:], "}}")
		if end < 0 {
			return false
		}
		line = line[start+2+end+2:]
	}
}

// redact is how a problem shows a template token (or several, joined): one holding ">",
// "<", "#" or "!", the marks of the rules that carry a password or its hash (>pass,
// <pass, #hash, !hash), also inside a template action, prints as that mark and "***"
// (">***"), or as "***" when the mark is not its first character.
func redact(tok string) string {
	if !strings.ContainsAny(tok, "<>#!") {
		return tok
	}
	if strings.ContainsAny(tok[:1], "<>#!") {
		return tok[:1] + "***"
	}
	return "***"
}

// aclUserRules sorts the rules of one user line: it returns the commands of its "+"
// tokens (lower case) and its key patterns, and reports every rule outside the §A2
// whitelist through add.
func aclUserRules(u aclUser, toks []string, add func(format string, a ...any)) (plus, keys []string) {
	password := ">{{." + u.password + "}}"
	// through returns the index of the first token from k on that contains end (the last
	// token when none does): a selector or a template action can span several tokens.
	through := func(k int, end string) int {
		for k < len(toks)-1 && !strings.Contains(toks[k], end) {
			k++
		}
		return k
	}
	for k := 0; k < len(toks); k++ {
		tok, lower := toks[k], strings.ToLower(toks[k])
		switch {
		case tok == password, lower == "on", lower == "resetkeys", lower == "-@all":
			// The rules every user line has (DESIGN §A2); aclLineShape checks their order.
		case strings.HasPrefix(tok, "("):
			end := through(k, ")")
			add("%s: selector %q: DESIGN §A2 grants only flat rules", u.name, redact(strings.Join(toks[k:end+1], " ")))
			k = end
		case strings.Contains(tok, "{{"):
			end := through(k, "}}")
			add("%s: template action %q: the only one allowed is the user's own %s", u.name, redact(strings.Join(toks[k:end+1], " ")), password)
			k = end
		case strings.HasPrefix(tok, "+@"), lower == "allcommands":
			add("%s: %s grants a whole command set; list each command", u.name, redact(tok))
		case tok == "~*", lower == "allkeys", strings.HasPrefix(tok, "%") && strings.HasSuffix(tok, "~*"):
			add("%s: %s grants every key", u.name, redact(tok))
		case strings.HasPrefix(tok, "%"):
			add("%s: %s: no %%R~, %%W~ or %%RW~ key patterns; DESIGN §A2 grants exactly %s", u.name, redact(tok), keyPattern)
		case strings.HasPrefix(tok, ">"):
			add("%s: %s: the only password is the user's own %s field (no password in the template)", u.name, redact(tok), password)
		case strings.HasPrefix(tok, "+"):
			plus = append(plus, lower[1:])
		case strings.HasPrefix(tok, "~"):
			keys = append(keys, tok)
		default:
			add("%s: %q is not a rule DESIGN §A2 uses (only on, %s, resetkeys, -@all, +<command> and %s)", u.name, redact(tok), password, keyPattern)
		}
	}
	return plus, keys
}

// aclLineShape checks the order S0 step 3 fixes for the rules of a user line: on, the
// user's own password field, resetkeys, the key pattern (rl_ops has none) and -@all, in
// that order, then only "+" tokens, each rule and each command once. A rule missing or
// repeated is reported as such; when each is there once (and the key patterns, which the
// caller reports, are right), the first token out of place is. Keywords count in any
// case, as the store reads them, but must sit in place exactly as written here.
func aclLineShape(u aclUser, toks, keys []string, add func(format string, a ...any)) {
	password := ">{{." + u.password + "}}"
	head := []string{"on", password, "resetkeys"}
	if u.keys {
		head = append(head, keyPattern)
	}
	head = append(head, "-@all")
	shape := "user " + u.name + " " + strings.Join(head, " ") + " +<command> …"
	once := true
	for _, rule := range []string{"on", password, "resetkeys", "-@all"} {
		n := 0
		for _, tok := range toks {
			if tok == rule || (rule != password && strings.EqualFold(tok, rule)) {
				n++
			}
		}
		switch {
		case n == 0:
			add("%s: no %s rule: the line reads %s (S0 step 3)", u.name, rule, shape)
			once = false
		case n > 1:
			add("%s: %s appears %d times: the line reads %s, each rule once (S0 step 3)", u.name, rule, n, shape)
			once = false
		}
	}
	keysRight := len(keys) == 0
	if u.keys {
		keysRight = len(keys) == 1 && keys[0] == keyPattern
	}
	if once && keysRight {
		for i, rule := range head {
			if toks[i] != rule { // each head rule is there, so toks is at least as long as head
				add("%s: %s where %s belongs: the line reads %s, in that order (S0 step 3)", u.name, redact(toks[i]), rule, shape)
				break
			}
		}
	}
	seen := map[string]bool{}
	for _, tok := range toks {
		lower := strings.ToLower(tok)
		if !strings.HasPrefix(lower, "+") || strings.HasPrefix(lower, "+@") {
			continue
		}
		if seen[lower] {
			add("%s: %s appears twice: list each command once (S0 step 3)", u.name, redact(lower))
		}
		seen[lower] = true
	}
}

// aclScriptProblems checks internal/scripts/*.lua: only §A4.1 names and prelude.lua; no
// dynamic calls; each script's commands within its users' inner lists (prelude.lua's
// within every script user's list); and, once all of a user's scripts exist, the union
// of their commands (prelude.lua's included) equal to that list.
func aclScriptProblems(ctx context.Context, root string) ([]string, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing scripts: %w", err)
	}
	dir := filepath.Join(root, filepath.FromSlash(scriptsDir))
	paths, err := filepath.Glob(filepath.Join(dir, "*.lua"))
	if err != nil {
		return nil, 0, fmt.Errorf("listing scripts: %w", err)
	}
	sort.Strings(paths)
	var problems []string
	cmds := map[string]map[string]int{} // script name → command → first line
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		name := strings.TrimSuffix(filepath.Base(p), ".lua")
		rel := scriptsDir + "/" + name + ".lua"
		if name != "prelude" && scriptUsers(name) == nil {
			problems = append(problems, fmt.Sprintf("%s: not a DESIGN §A4.1 script name (nor prelude.lua)", rel))
			continue
		}
		src, err := readFile(ctx, root, rel)
		if err != nil {
			return nil, 0, err
		}
		calls, err := luaCalls(string(src))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		cmds[name] = map[string]int{}
		for _, c := range calls {
			if c.dynamic != "" {
				problems = append(problems, fmt.Sprintf("%s:%d: %s: name the command with a string literal, as redis.call('X', …), redis.pcall('X', …) or pcall(redis.call, 'X', …) (AGENTS.md §3)", rel, c.line, c.dynamic))
				continue
			}
			if _, ok := cmds[name][c.cmd]; !ok {
				cmds[name][c.cmd] = c.line
			}
		}
	}
	for _, u := range aclUsers() {
		if len(u.inner) == 0 {
			continue
		}
		allowed := map[string]bool{}
		for _, c := range u.inner {
			allowed[c] = true
		}
		used := map[string]bool{}
		complete := true
		for _, s := range append(scriptNames(), "prelude") {
			if s != "prelude" && !slices.Contains(scriptUsers(s), u.name) {
				continue
			}
			got, ok := cmds[s]
			if !ok {
				complete = complete && s == "prelude"
				continue
			}
			for _, c := range slices.Sorted(maps.Keys(got)) {
				used[c] = true
				if !allowed[c] {
					problems = append(problems, fmt.Sprintf("%s/%s.lua:%d: %s is not in %s's inner commands (DESIGN §A2)", scriptsDir, s, got[c], strings.ToUpper(c), u.name))
				}
			}
		}
		if !complete {
			continue
		}
		for _, c := range u.inner {
			if !used[c] {
				problems = append(problems, fmt.Sprintf("%s: all of %s's scripts exist, but none calls %s, which DESIGN §A2 lists", scriptsDir, u.name, strings.ToUpper(c)))
			}
		}
	}
	return problems, len(paths), nil
}

// setDiff returns the items of want missing from got and the items of got not in want, sorted.
func setDiff(want, got []string) (missing, extra []string) {
	w, g := map[string]bool{}, map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	for _, s := range got {
		g[s] = true
	}
	for s := range w {
		if !g[s] {
			missing = append(missing, s)
		}
	}
	for s := range g {
		if !w[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}
