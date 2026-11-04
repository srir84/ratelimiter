package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/srir84/ratelimiter/limiter"
)

const (
	// readyWait bounds the wait for every instance to be ready (IMPLEMENTATION_PLAN H3 step 3,
	// plan choice: 30 s).
	readyWait = 30 * time.Second
	// readyPoll is the interval between two rounds of /health reads while the instances start
	// (plan choice).
	readyPoll = 100 * time.Millisecond
	// maxReplyBody bounds an instance reply the orchestrator reads, in bytes.
	maxReplyBody = 1 << 20
	// reasonNoEpoch is the /health reason of an instance that has not joined an epoch.
	reasonNoEpoch = "no_epoch"
)

// health is the part of an instance's /health the orchestrator reads (DESIGN §B8: the library
// Health plus hitapi's fields). Each field must be present: a /health that does not show
// fixtures or the clock source cannot pass the benchmark guard.
type health struct {
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	ClockSource string `json:"clock_source"`
	Fixtures    bool   `json:"fixtures"`
}

// readHealth reads /health of the instance at addr within ctx.
func (r *runner) readHealth(ctx context.Context, addr string) (health, error) {
	body, status, err := r.call(ctx, http.MethodGet, addr, "/health", "")
	if err != nil {
		return health{}, err
	}
	if status != http.StatusOK {
		return health{}, fmt.Errorf("GET /health: status %d: %.200s", status, body)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return health{}, fmt.Errorf("GET /health: not a JSON object: %.200s", body)
	}
	for _, k := range []string{"status", "reason", "clock_source", "fixtures"} {
		if _, ok := fields[k]; !ok {
			return health{}, fmt.Errorf("GET /health: no %s field: %.200s", k, body)
		}
	}
	var h health
	if err := json.Unmarshal(body, &h); err != nil {
		return health{}, fmt.Errorf("GET /health: %w: %.200s", err, body)
	}
	return h, nil
}

// call makes one request to the instance at addr within ctx and returns the reply's body (at
// most maxReplyBody bytes) and status. The client never resends it: main's client keeps no
// connection alive.
func (r *runner) call(ctx context.Context, method, addr, path, body string) ([]byte, int, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rd)
	if err != nil {
		return nil, 0, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := r.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxReplyBody))
	_ = res.Body.Close() // read or failed: closing changes nothing recorded
	if err != nil {
		return nil, res.StatusCode, fmt.Errorf("%s %s: reading the reply: %w", method, path, err)
	}
	return bytes.TrimSpace(data), res.StatusCode, nil
}

// ready reports whether instance i (0-based) is ready for the run (plan H3 steps 3 and 6):
// status ready, or for an instance started with --instance-config, any /health reason other
// than no_epoch (T-HAR-1's instance refuses with fingerprint_mismatch, and is ready so).
func (r *runner) ready(i int, h health) bool {
	if r.apis[i].own {
		return h.Reason != "" && h.Reason != reasonNoEpoch
	}
	return h.Status == "ready"
}

// waitReady waits until every instance is ready (ready), reading each /health every readyPoll,
// at most wait in all (readyWait in a run). An instance that exits first fails the wait at
// once.
func (r *runner) waitReady(ctx context.Context, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(readyPoll)
	defer tick.Stop()
	pending := make([]int, len(r.apis))
	for i := range pending {
		pending[i] = i
	}
	last := make([]string, len(r.apis))
	for {
		pending = slices.DeleteFunc(pending, func(i int) bool {
			if c := r.apiProcs[i]; c.exited() {
				last[i] = "exited with " + exitText(c.waitErr)
				return false
			}
			h, err := r.readHealth(ctx, r.apis[i].addr)
			if err != nil {
				if ctx.Err() == nil { // a read the wait's own end cut short keeps the last answer
					last[i] = err.Error()
				}
				return false
			}
			last[i] = "status " + h.Status + ", reason " + h.Reason
			return r.ready(i, h)
		})
		if len(pending) == 0 {
			return nil
		}
		for _, i := range pending {
			if r.apiProcs[i].exited() {
				return fmt.Errorf("hitapi %d %s before it was ready (see %s)", i+1, last[i], r.apiProcs[i].name+stderrSuffix)
			}
		}
		select {
		case <-ctx.Done():
			var waiting []string
			for _, i := range pending {
				waiting = append(waiting, fmt.Sprintf("hitapi %d (%s)", i+1, last[i]))
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("not ready after %v: %s", wait, strings.Join(waiting, "; "))
			}
			return fmt.Errorf("stopped while waiting for %s: %w", strings.Join(waiting, "; "), ctx.Err())
		case <-tick.C:
		}
	}
}

// guardHealth is the benchmark guard's check of the running instances (HAR-24; DESIGN §B5;
// plan H3 step 3): it refuses a benchmark run when an instance's /health reports fixtures:
// true or the controlled clock, or cannot be read, so cannot show production construction.
// Every other benchmark-guard check ran in preflight, before the store started.
func (r *runner) guardHealth(ctx context.Context) []refusal {
	var refs refusals
	for i, a := range r.apis {
		h, err := r.readHealth(ctx, a.addr)
		if err != nil {
			refs.add(reasonBenchHealthUnread, "instance %d at %s: %v: a benchmark instance must show it runs production construction", i+1, a.addr, err)
			continue
		}
		if h.Fixtures {
			refs.add(reasonBenchHealthFixtures, "instance %d at %s: /health reports fixtures: true: a test-build limiter in a benchmark run", i+1, a.addr)
		}
		if h.ClockSource == limiter.Controlled.String() {
			refs.add(reasonBenchHealthClock, "instance %d at %s: /health reports clock_source %q: benchmarks use the store clock", i+1, a.addr, h.ClockSource)
		}
	}
	return refs
}
