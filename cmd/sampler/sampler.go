package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"ratelimiter/harness/runlog"
)

// sample is one samples.jsonl line (DESIGN §B7, §B8; IMPLEMENTATION_PLAN H3 step 7; the field
// names are a plan choice). Each read's value and its error sit side by side: the value is
// null when the read failed, and the error, the failure's text, is null when it worked. A
// process entry carries its own error.
type sample struct {
	TNs           int64           `json:"t_ns"`        // wall-clock ns when the tick started
	Procs         []procSample    `json:"procs"`       // pids.json's processes, in its order
	ProcsError    *string         `json:"procs_error"` // reading pids.json
	Postgres      *pgSample       `json:"postgres"`    // the postmaster and its children
	PostgresError *string         `json:"postgres_error"`
	APIs          []apiSample     `json:"apis"`   // instance 1, 2, … in --apis order
	Status        json.RawMessage `json:"status"` // rladmin status --json (ADM-4)
	StatusError   *string         `json:"status_error"`
	Store         *storeSample    `json:"store"` // INFO persistence and INFO clients
	StoreError    *string         `json:"store_error"`
}

// procSample is one pids.json process's stats: its CPU % since its reading on the previous
// tick (null on its first reading, or when the PID names a newer process), and its resident
// set size in bytes.
type procSample struct {
	Role     string   `json:"role"`
	Instance int      `json:"instance,omitempty"` // hitapi only, as in pids.json
	PID      int      `json:"pid"`
	CPU      *float64 `json:"cpu"` // % of one CPU, rounded to 0.01: a process busy on two cores reads 200
	RSS      *uint64  `json:"rss"`
	Error    *string  `json:"error"`
}

// pgSample is the harness database's stats (DESIGN §B7: the postmaster plus its backends):
// the postmaster and every child read this tick, RSS summed over them all, and CPU % over those
// also read on the previous tick (a backend started since joins the sum on its second tick).
// CPU % is null when the postmaster itself has no earlier reading.
type pgSample struct {
	PID       int      `json:"pid"`       // the postmaster's, from postmaster.pid
	Processes int      `json:"processes"` // the postmaster and the children read
	CPU       *float64 `json:"cpu"`
	RSS       uint64   `json:"rss"`
}

// apiSample is one hitapi instance's /health and /metrics.
type apiSample struct {
	Instance     int                 `json:"instance"`
	Addr         string              `json:"addr"`
	Health       json.RawMessage     `json:"health"` // the /health JSON object as served
	HealthError  *string             `json:"health_error"`
	Metrics      map[string]*float64 `json:"metrics"` // each /metrics series' value by its series text
	MetricsError *string             `json:"metrics_error"`
}

// storeSample is the store's INFO persistence and INFO clients sections, each value by its key
// as the store wrote it.
type storeSample struct {
	Persistence map[string]string `json:"persistence"`
	Clients     map[string]string `json:"clients"`
}

// samplerConfig is what the sampler reads and when (plan H3 step 7).
type samplerConfig struct {
	runDir    string        // pids.json is read from it
	apis      []string      // the addresses of instance 1, 2, …
	rladmin   string        // the rladmin binary (bin/<run_kind>/rladmin)
	pgData    string        // PostgreSQL's data directory, holding postmaster.pid
	interval  time.Duration // between ticks
	timeout   time.Duration // every read of a tick ends by then: below the interval
	waitDelay time.Duration // after a kill, the most rladmin's Wait waits for its output
}

// storeAccess is how the sampler reaches the store (DESIGN §A2): the address, rl_ops's
// password for INFO, and rladmin's environment, which carries rl_admin's password. Formatting
// or logging a storeAccess shows none of it, and redact takes both passwords out of any text
// the sampler records (AGENTS.md §1 Secrets).
type storeAccess struct {
	addr       string
	opsPass    string
	adminPass  string
	rladminEnv []string
}

func (storeAccess) String() string       { return "storeAccess{redacted}" }
func (storeAccess) GoString() string     { return "storeAccess{redacted}" }
func (storeAccess) LogValue() slog.Value { return slog.StringValue("redacted") }

// newStoreAccess returns the store access for addr and the two passwords; base is the
// sampler's own environment, which rladmin gets without any RL_ variable, plus
// RL_ADMIN_PASSWORD and RL_STORE_ADDR (DESIGN §A6), so rl_ops's password never reaches it.
func newStoreAccess(addr, opsPass, adminPass string, base []string) storeAccess {
	env := make([]string, 0, len(base)+2)
	for _, kv := range base {
		if !strings.HasPrefix(kv, envPrefix) {
			env = append(env, kv)
		}
	}
	env = append(env, envAdminPassword+"="+adminPass, envStoreAddr+"="+addr)
	return storeAccess{addr: addr, opsPass: opsPass, adminPass: adminPass, rladminEnv: env}
}

// redact replaces each password in text with [redacted].
func (a storeAccess) redact(text string) string {
	for _, p := range []string{a.opsPass, a.adminPass} {
		if p != "" {
			text = strings.ReplaceAll(text, p, "[redacted]")
		}
	}
	return text
}

// sampler takes one sample per tick and writes it to samples.jsonl (DESIGN §B7).
type sampler struct {
	cfg    samplerConfig
	access storeAccess
	client *http.Client
	out    *runlog.Writer
	// cpu is the previous tick's CPU readings by PID. Only readProcesses touches it, once per
	// tick, and ticks never overlap.
	cpu map[int32]cpuReading
}

func newSampler(cfg samplerConfig, access storeAccess, client *http.Client, out *runlog.Writer) *sampler {
	return &sampler{cfg: cfg, access: access, client: client, out: out, cpu: map[int32]cpuReading{}}
}

// run takes a first CPU reading of the processes, then a sample every interval until ctx ends
// (SIGTERM). A tick in progress when ctx ends still finishes and writes its line.
func (s *sampler) run(ctx context.Context) {
	s.prime(ctx)
	t := time.NewTicker(s.cfg.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ctx.Err() != nil {
				return
			}
			s.tick(ctx)
		}
	}
}

// prime reads the processes once without writing a line, so the first tick already has each
// process's CPU % over the interval since the start.
func (s *sampler) prime(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.timeout)
	defer cancel()
	var discard sample
	s.readProcesses(ctx, &discard)
}

// tick takes one sample and enqueues its line. Every read runs in a goroutine of its own, all
// bounded by one deadline cfg.timeout away, below the interval: a slow or failed read records
// its error, and the line is written once every read has ended, so a tick never runs into the
// next one. The reads outlive ctx (SIGTERM), so the last line is a whole one too.
func (s *sampler) tick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.timeout)
	defer cancel()
	line := sample{TNs: time.Now().UnixNano(), APIs: make([]apiSample, len(s.cfg.apis))}
	var wg sync.WaitGroup
	start := func(read func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			read()
		}()
	}
	start(func() { s.readProcesses(ctx, &line) })
	for i, addr := range s.cfg.apis {
		a := &line.APIs[i]
		a.Instance, a.Addr = i+1, addr
		start(func() {
			v, err := s.readHealth(ctx, addr)
			a.Health, a.HealthError = v, s.errText(err)
		})
		start(func() {
			v, err := s.readMetrics(ctx, addr)
			a.Metrics, a.MetricsError = v, s.errText(err)
		})
	}
	start(func() {
		v, err := s.readStatus(ctx)
		line.Status, line.StatusError = v, s.errText(err)
	})
	start(func() {
		v, err := s.readStore(ctx)
		line.Store, line.StoreError = v, s.errText(err)
	})
	wg.Wait()
	s.out.Enqueue(line)
}

// errText is err's text for a line, without either password; nil for no error.
func (s *sampler) errText(err error) *string {
	if err == nil {
		return nil
	}
	text := s.access.redact(err.Error())
	return &text
}

// round2 rounds a CPU % to 0.01; nil stays nil.
func round2(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := math.Round(*v*100) / 100
	return &r
}
