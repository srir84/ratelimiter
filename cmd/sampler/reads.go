package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/srir84/ratelimiter/internal/store/resp"
)

const (
	// maxBody bounds an API reply the sampler reads, in bytes (plan choice: hitapi's /metrics
	// is about 10 KB).
	maxBody = 1 << 20
	// maxOutput bounds the rladmin output kept, per stream, in bytes; status --json prints one
	// short line.
	maxOutput = 64 << 10
	// maxErrText bounds the part of rladmin's stderr an error quotes, in bytes.
	maxErrText = 200

	// userOps is the store user for INFO (DESIGN §A2: the harness's rl_ops).
	userOps = "rl_ops"
)

// readHealth reads the instance at addr's /health: one JSON object, kept as served.
func (s *sampler) readHealth(ctx context.Context, addr string) (json.RawMessage, error) {
	target := "http://" + addr + "/health"
	body, err := s.get(ctx, target)
	if err != nil {
		return nil, err
	}
	obj, err := jsonObject(body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	return obj, nil
}

// readMetrics reads the instance at addr's /metrics, Prometheus text, into each series'
// value by its series text.
func (s *sampler) readMetrics(ctx context.Context, addr string) (map[string]*float64, error) {
	target := "http://" + addr + "/metrics"
	body, err := s.get(ctx, target)
	if err != nil {
		return nil, err
	}
	m, err := parseMetrics(body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	return m, nil
}

// get makes one GET of target within ctx and returns the body of its 200 reply, at most
// maxBody bytes. The client never resends it (main turns keep-alive off) and follows no
// redirect.
func (s *sampler) get(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	res, err := s.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // its own text repeats the method and the URL
		}
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	_ = res.Body.Close() // read or failed: closing changes nothing recorded
	switch {
	case err != nil:
		return nil, fmt.Errorf("GET %s: reading the reply: %w", target, err)
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GET %s: %s", target, res.Status)
	case len(body) > maxBody:
		return nil, fmt.Errorf("GET %s: a reply over %d bytes", target, maxBody)
	}
	return body, nil
}

// jsonObject checks that data holds one JSON object in UTF-8 and nothing after it, and
// returns it.
func jsonObject(data []byte) (json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	if obj == nil {
		return nil, errors.New("not a JSON object: null")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the JSON object")
	}
	return json.RawMessage(bytes.TrimSpace(data)), nil
}

// parseMetrics reads hitapi's /metrics (DESIGN §A5.11; harness/httpapi writes it): "#" comment
// lines and one "series value" line per series, where the series is a metric name with an
// optional {label set} and the value a Prometheus float. It returns each series' value by its
// series text; NaN and ±Inf, which JSON cannot hold, become null. Any other line, or a series
// given twice, is an error.
func parseMetrics(text []byte) (map[string]*float64, error) {
	m := map[string]*float64{}
	for i, line := range strings.Split(string(text), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cut := strings.LastIndexByte(line, ' ')
		if cut < 0 || !validSeries(line[:cut]) {
			return nil, fmt.Errorf("line %d: %.80q is not \"series value\"", i+1, line)
		}
		series := line[:cut]
		v, err := strconv.ParseFloat(line[cut+1:], 64)
		if err != nil {
			return nil, fmt.Errorf("line %d: %.80q: the value is not a number", i+1, line)
		}
		if _, dup := m[series]; dup {
			return nil, fmt.Errorf("line %d: series %.80q given twice", i+1, series)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			m[series] = nil
			continue
		}
		m[series] = &v
	}
	return m, nil
}

// validSeries reports whether s is a metric name ([a-zA-Z_:][a-zA-Z0-9_:]*), alone or followed
// by a label set in braces that ends s.
func validSeries(s string) bool {
	name, labels, braced := strings.Cut(s, "{")
	if name == "" {
		return false
	}
	for i, c := range name {
		letter := c == '_' || c == ':' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return !braced || strings.HasSuffix(labels, "}")
}

// readStatus runs `rladmin status --json` (DESIGN §A6, SPEC ADM-4) with rladmin's environment
// and returns its JSON object. ctx's end kills it; its failure's text quotes the first line of
// its stderr.
func (s *sampler) readStatus(ctx context.Context) (json.RawMessage, error) {
	cmd := exec.CommandContext(ctx, s.cfg.rladmin, "status", "--json")
	cmd.Env = s.access.rladminEnv
	stdout, stderr := &capped{max: maxOutput}, &capped{max: maxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = s.cfg.waitDelay
	if err := cmd.Run(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			err = fmt.Errorf("%w (%w)", cerr, err) // killed at the deadline
		}
		if msg := firstLine(s.access.redact(stderr.buf.String())); msg != "" { // redacted before it is cut short
			return nil, fmt.Errorf("rladmin status: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("rladmin status: %w", err)
	}
	if stdout.over {
		return nil, fmt.Errorf("rladmin status: more than %d bytes of output", maxOutput)
	}
	obj, err := jsonObject(stdout.buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("rladmin status --json: %w", err)
	}
	return obj, nil
}

// capped keeps the first max bytes written to it and notes that more came.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	keep := p
	if room := c.max - c.buf.Len(); len(keep) > room {
		keep, c.over = keep[:max(room, 0)], true
	}
	c.buf.Write(keep)
	return len(p), nil
}

// firstLine is the first non-empty line of text, at most maxErrText bytes of it.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > maxErrText {
				line = line[:maxErrText] + "…"
			}
			return line
		}
	}
	return ""
}

// readStore reads the store's INFO persistence and INFO clients as rl_ops (DESIGN §A2, §B7)
// on a connection of its own, dialed, authenticated, used and closed within ctx's deadline, so
// a restarted store is found again on the next tick.
func (s *sampler) readStore(ctx context.Context) (*storeSample, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("INFO: no deadline")
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", s.access.addr)
	if err != nil {
		return nil, fmt.Errorf("INFO: connecting to the store at %s: %w", s.access.addr, err)
	}
	defer func() { _ = nc.Close() }() // the replies are read or failed: closing changes nothing
	stop := context.AfterFunc(ctx, func() { _ = nc.SetDeadline(time.Now()) })
	defer stop()
	if err := resp.Handshake(nc, userOps, s.access.opsPass, deadline, nil); err != nil {
		return nil, fmt.Errorf("INFO: authenticating to the store at %s as %s: %w", s.access.addr, userOps, cause(ctx, err))
	}
	if err := nc.SetDeadline(deadline); err != nil { // the handshake cleared it
		return nil, fmt.Errorf("INFO: setting the deadline: %w", err)
	}
	if _, err := nc.Write(resp.AppendCommand(resp.AppendCommand(nil, "INFO", "persistence"), "INFO", "clients")); err != nil {
		return nil, fmt.Errorf("INFO: sending: %w", cause(ctx, err))
	}
	r := resp.NewReader(nc)
	var st storeSample
	for _, sec := range []struct {
		name string
		dst  *map[string]string
	}{{"persistence", &st.Persistence}, {"clients", &st.Clients}} {
		v, err := r.ReadValue()
		if err != nil {
			return nil, fmt.Errorf("INFO %s: reading the reply: %w", sec.name, cause(ctx, err))
		}
		if v.Kind != resp.Bulk || v.Null {
			return nil, fmt.Errorf("INFO %s: the reply %s is not a bulk string", sec.name, v)
		}
		if *sec.dst, err = parseInfo(v.Str); err != nil {
			return nil, fmt.Errorf("INFO %s: %w", sec.name, err)
		}
	}
	return &st, nil
}

// cause is ctx's error when ctx has ended (its deadline made the I/O fail), else err.
func cause(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// parseInfo reads one INFO section reply: a "# Section" header and "key:value" lines, CRLF
// separated. It returns each value by its key, as the store wrote it.
func parseInfo(text string) (map[string]string, error) {
	m := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || k == "" {
			return nil, fmt.Errorf("the line %.80q is not key:value", line)
		}
		if _, dup := m[k]; dup {
			return nil, fmt.Errorf("the key %.80q given twice", k)
		}
		m[k] = v
	}
	return m, nil
}
