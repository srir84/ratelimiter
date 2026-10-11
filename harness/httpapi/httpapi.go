package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sync"
	"time"

	"ratelimiter/harness/config"
	"ratelimiter/harness/dbclient"
	"ratelimiter/limiter"
)

// Limiter is what the API needs from the rate limiter.
type Limiter interface {
	Acquire(ctx context.Context, key string) Decision
	Health() limiter.Health
	Metrics() limiter.MetricsSnapshot
	Close() // with the default drain (HAR-15)
}

// Decision is an Acquire result as the handler uses it. Permit is non-nil only for the
// allowed and degraded kinds.
type Decision struct {
	Kind         limiter.ResultKind
	DecisionNs   int64 // allowed, limited
	RetryAfterNs int64 // limited
	Permit       Permit
}

// Permit is an allowed or degraded acquire's permit (SPEC HOST-2): Done for success or an
// unknown outcome, Release only for a confirmed failure.
type Permit interface {
	Done()
	Release()
	DeductionID() string // "" for a degraded permit
}

// DB is the harness database client (dbclient.Client): one operation's insert-or-read,
// classified per the DESIGN §B3 table. stop closes when SIGTERM begins (BeginShutdown): a Hit
// still waiting for a connection stops waiting, and one that holds a connection checks stop
// just before its first statement; either way it sends nothing and returns dbclient.Cancelled.
type DB interface {
	Hit(ctx context.Context, userID, requestID string, arrivalNs int64, stop <-chan struct{}) dbclient.Result
}

// RunLog is the run-log writer (runlog.Writer): Enqueue never blocks and counts a drop
// when its channel is full. Drain waits until the lines queued before it began have been
// handed to the kernel, or ctx ends, and returns how many of them are not on the file, those
// not handed over yet and those whose write failed, with an error (0, with a nil error, when
// all were written).
type RunLog interface {
	Enqueue(rec any)
	Drops() int64
	Len() int
	Drain(ctx context.Context) (int64, error)
}

// Config is what the server takes from the command line and the config file.
type Config struct {
	API          config.API       // max_in_flight, limiter_enabled, refuse_after_acquire_bp
	Instance     limiter.Instance // P (MaxReleaseDelayNs) and the start record's settings
	RunID        string           // --run-id
	InstanceNum  int              // --instance N
	ProcessStart time.Time        // the base of release_mono_ns (plan choice)
	NofileLimit  uint64           // the effective soft RLIMIT_NOFILE (§B1)
	// ListenerQueueLen reports the limiter's listener queue length for /health
	// (limitertest.ListenerQueueLen; test builds only). When nil, /health has no
	// listener_queue_len.
	ListenerQueueLen func() int
	// Fixtures are the fault fixtures the /_test routes drive (test builds only; an empty
	// struct in the benchmark build, which has no such routes; DESIGN §B6).
	Fixtures Fixtures
}

// Server serves /hit, /health and /metrics.
type Server struct {
	cfg   Config
	lim   Limiter
	db    DB
	log   RunLog
	p     time.Duration // max_release_delay: the deadline from just before acquire (HAR-11)
	slots *slots        // the in-flight cap (HAR-14)

	// stopping closes when SIGTERM begins (BeginShutdown), so work not yet at its first DB
	// statement is refused after its acquire (ERR-15, HAR-15); DB.Hit gets it as its stop.
	stopping chan struct{}
	stopOnce sync.Once
}

// New returns a server for lim, db and log. It fails on a missing dependency and on settings
// out of range.
func New(cfg Config, lim Limiter, db DB, log RunLog) (*Server, error) {
	var errs []error
	if lim == nil || db == nil || log == nil {
		errs = append(errs, errors.New("httpapi: the limiter, the database and the run log are required"))
	}
	if cfg.API.MaxInFlight < 1 {
		errs = append(errs, fmt.Errorf("httpapi: max_in_flight = %d: want at least 1", cfg.API.MaxInFlight))
	}
	if bp := cfg.API.RefuseAfterAcquireBP; bp < 0 || bp > config.MaxRefuseBP {
		errs = append(errs, fmt.Errorf("httpapi: refuse_after_acquire_bp = %d: want 0 to %d", bp, config.MaxRefuseBP))
	}
	if cfg.Instance.MaxReleaseDelayNs <= 0 {
		errs = append(errs, fmt.Errorf("httpapi: max_release_delay_ns = %d: want more than 0", cfg.Instance.MaxReleaseDelayNs))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Server{
		cfg:      cfg,
		lim:      lim,
		db:       db,
		log:      log,
		p:        time.Duration(cfg.Instance.MaxReleaseDelayNs),
		slots:    newSlots(cfg.API.MaxInFlight),
		stopping: make(chan struct{}),
	}, nil
}

// Handler returns the routes. /hit, /health and /metrics are registered without a method
// pattern and check the method themselves, and / answers every other path, so every 404
// and 405 is JSON (the mux's own are plain text; plan choice). Test builds add the /_test
// control routes (DESIGN §B6, testctl_rltest.go); the benchmark build has none. The outer
// handler sets Cache-Control: no-store on every response (HTTP-8), and answers itself, with
// the JSON 404 not_found, the requests the mux would answer outside the ERR table (HTTP-7):
// the request URI "*" (a bare 400) and a path that is not clean (a redirect to the cleaned
// path), neither of which names a route. cmd/hitapi's server passes "OPTIONS *" on to it
// (DisableGeneralOptionsHandler).
//
// Some requests never reach a handler: net/http's server answers them itself while reading
// the request, so their responses carry neither Cache-Control: no-store nor a JSON body, and
// no handler can change that. They are the plain-text 400s for a request it cannot parse (a
// malformed request line, header or Host), the 431 for headers larger than MaxHeaderBytes,
// the 417 (empty body) for an Expect header other than 100-continue, and the plain-text 501
// and 505 for an unsupported transfer coding or protocol version.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/hit", s.hit)
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/metrics", s.metrics)
	mux.HandleFunc("/", s.notFound)
	s.testRoutes(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.RequestURI == "*" || !isCleanPath(r.URL.EscapedPath()) {
			s.notFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// isCleanPath reports whether p is a path http.ServeMux serves as it is, one it would not
// redirect to its cleaned form (net/http's cleanPath): it starts with a slash and has no
// empty, "." or ".." element, a trailing slash allowed.
func isCleanPath(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np == p
}

// BeginShutdown starts the SIGTERM sequence in the handlers (HAR-15): a new /hit gets 503
// service_overloaded with no acquire (ERR-7), and a request whose first DB statement is not
// sent yet is refused after its acquire: release(), 503 service_aborted_after_acquire
// (ERR-15, §B3). A request already in the database step is refused there: at once while it
// waits for a connection, else at its last check before that statement (DB.Hit's stop).
func (s *Server) BeginShutdown() {
	s.slots.stop()
	s.stopOnce.Do(func() { close(s.stopping) })
}

// shuttingDown reports whether SIGTERM began (BeginShutdown).
func (s *Server) shuttingDown() bool {
	select {
	case <-s.stopping:
		return true
	default:
		return false
	}
}

// WaitIdle waits until no /hit request holds an in-flight slot, or ctx ends.
func (s *Server) WaitIdle(ctx context.Context) error { return s.slots.waitIdle(ctx) }

// Stop ends the SIGTERM sequence once the /hit work has finished or P has passed (HAR-15):
// it closes the limiter with the default drain, which returns once every pending refund is
// final (its event handed to the run log) or the drain timeout has passed; then waits until
// the run-log channel is empty, for at most drain from Close's return (Close may take up to
// drain_timeout + at most listenerGrace (100 ms) while the listener empties its queue), or
// until ctx ends; and writes the stop record. The caller closes the run log afterwards.
func (s *Server) Stop(ctx context.Context, drain time.Duration) error {
	s.lim.Close()
	dctx, cancel := context.WithTimeout(ctx, drain)
	defer cancel()
	_, err := s.log.Drain(dctx)
	s.logStop()
	return err
}
