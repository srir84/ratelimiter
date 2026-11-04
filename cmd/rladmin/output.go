package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/srir84/ratelimiter/internal/store/resp"
)

// redacted stands in for the password wherever text would carry it.
const redacted = "<password>"

// redactor writes to w with every form of the password replaced (AGENTS.md §1 Secrets): the
// raw text, its JSON-escaped form and its Go-quoted form, so neither a reply that repeats the
// password nor a JSON or quoted rendering of one reaches the output. Every message reaches it
// in one Write.
type redactor struct {
	w     io.Writer
	forms []string
}

func newRedactor(w io.Writer, secret string) *redactor {
	return &redactor{w: w, forms: secretForms(secret)}
}

// secretForms returns the forms of secret that output may hold, or none for an empty secret.
func secretForms(secret string) []string {
	if secret == "" {
		return nil
	}
	forms := []string{secret}
	if b, err := json.Marshal(secret); err == nil && len(b) >= 2 {
		forms = append(forms, string(b[1:len(b)-1]))
	}
	if q := strconv.Quote(secret); len(q) >= 2 {
		forms = append(forms, q[1:len(q)-1])
	}
	return forms
}

// Write writes p with every form of the secret replaced; a message in which a replacement
// re-forms one is replaced whole.
func (r *redactor) Write(p []byte) (int, error) {
	s := string(p)
	for _, f := range r.forms {
		s = strings.ReplaceAll(s, f, redacted)
	}
	for _, f := range r.forms {
		if strings.Contains(s, f) {
			s = redacted + "\n"
		}
	}
	if _, err := io.WriteString(r.w, s); err != nil {
		return 0, err
	}
	return len(p), nil
}

// redactValue returns v with every occurrence of secret in its strings, nested ones included,
// replaced, before Value.String clips a string and could keep part of the password (as
// resp.Handshake does for the AUTH reply).
func redactValue(v resp.Value, secret string) resp.Value {
	if secret == "" {
		return v
	}
	v.Str = strings.ReplaceAll(v.Str, secret, redacted)
	if strings.Contains(v.Str, secret) {
		v.Str = redacted
	}
	if len(v.Elems) > 0 {
		elems := make([]resp.Value, len(v.Elems))
		for i, e := range v.Elems {
			elems[i] = redactValue(e, secret)
		}
		v.Elems = elems
	}
	return v
}

// field is one named value of a result, in output order. value is a string, an int64, a *int64
// (nil where the reply showed the empty string: missing or unreadable), a []*int64, a
// map[string]string or a []slowEntry.
type field struct {
	name  string
	value any
}

// opt converts an admin reply element of layout kind E (an integer, or the empty string for a
// value that is missing or fails is_uint53, §A4.9) to a *int64, nil for the empty string.
func opt(v resp.Value) *int64 {
	if v.Kind != resp.Int {
		return nil
	}
	n := v.Int
	return &n
}

// renderFields formats fs as one JSON object on one line, keys in order, or as a table: one
// "name  value" line per field, '-' standing for an empty or missing value.
func renderFields(fs []field, asJSON bool) ([]byte, error) {
	var b bytes.Buffer
	if asJSON {
		b.WriteByte('{')
		for i, f := range fs {
			if i > 0 {
				b.WriteByte(',')
			}
			k, err := json.Marshal(f.name)
			if err != nil {
				return nil, fmt.Errorf("encoding %s: %w", f.name, err)
			}
			v, err := json.Marshal(f.value)
			if err != nil {
				return nil, fmt.Errorf("encoding %s: %w", f.name, err)
			}
			b.Write(k)
			b.WriteByte(':')
			b.Write(v)
		}
		b.WriteString("}\n")
		return b.Bytes(), nil
	}
	width := 0
	for _, f := range fs {
		width = max(width, len(f.name))
	}
	for _, f := range fs {
		fmt.Fprintf(&b, "%-*s  %s\n", width, f.name, tableValue(f.value))
	}
	return b.Bytes(), nil
}

// tableValue formats one value for the table.
func tableValue(v any) string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return "-"
		}
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case *int64:
		if x == nil {
			return "-"
		}
		return strconv.FormatInt(*x, 10)
	case []*int64:
		if len(x) == 0 {
			return "-"
		}
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = tableValue(e)
		}
		return strings.Join(parts, " ")
	}
	return fmt.Sprint(v)
}

// failure writes a failed command's message to w: "rladmin: <message>", then the raw reply
// and the recovery hint when there are any.
func failure(w io.Writer, e *cmdError, secret string) {
	var b strings.Builder
	b.WriteString("rladmin: " + e.msg + "\n")
	if e.raw != nil {
		b.WriteString("raw reply: " + redactValue(*e.raw, secret).String() + "\n")
	}
	if e.hint != "" {
		b.WriteString(e.hint + "\n")
	}
	_, _ = io.WriteString(w, b.String()) // CLI output: a failed write changes nothing
}

// renderStoreInfo formats store-info's result: as JSON, one object with each INFO section
// (by its lower-case name, its fields as strings) and "slowlog", the entries of SLOWLOG GET;
// as a table, each section's "name:value" lines under its "# name" header, then the slow log,
// one entry per line.
func renderStoreInfo(si storeInfo, asJSON bool) ([]byte, error) {
	if asJSON {
		fs := make([]field, 0, len(si.sections)+1)
		for _, s := range si.sections {
			fs = append(fs, field{s.name, s.fields})
		}
		return renderFields(append(fs, field{"slowlog", si.slowlog}), true)
	}
	var b bytes.Buffer
	for _, s := range si.sections {
		b.WriteString("# " + s.name + "\n")
		for _, l := range s.lines {
			b.WriteString(l + "\n")
		}
	}
	b.WriteString("# slowlog\n")
	for _, e := range si.slowlog {
		fmt.Fprintf(&b, "%d %d %dus %s %s %s\n", e.ID, e.Timestamp, e.DurationUs, strconv.Quote(strings.Join(e.Args, " ")), tableValue(e.Client), tableValue(e.ClientName))
	}
	return b.Bytes(), nil
}
