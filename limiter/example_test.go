package limiter_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"ratelimiter/limiter"
)

// maxReleaseDelay is the host's declared max_release_delay (P): the longest time from just
// before Acquire until Release (SPEC RCP-8, HOST-3). The harness uses 5 s.
const maxReleaseDelay = 5 * time.Second

// errConfirmedFailure stands for a protected-work failure the host knows did nothing.
var errConfirmedFailure = errors.New("confirmed failure")

// hostLimiter builds an instance as ExampleNew does, without a refund listener.
func hostLimiter() *limiter.Limiter {
	p := limiter.DefaultPolicy()
	p.MaxTrackedBuckets, p.MaxLiveMarkers = 100_000, 50_000
	in := limiter.DefaultInstance(p)
	in.MaxReleaseDelayNs, in.MarginNs = maxReleaseDelay.Nanoseconds(), time.Second.Nanoseconds()
	in.MaxHeldReceipts, in.MaxPendingRefunds = 1_152, 1_152
	st := limiter.StoreOptions{Addr: "127.0.0.1:6390", User: "rl_lib", Password: os.Getenv("RL_LIB_PASSWORD")}
	l, err := limiter.New(p, in, st)
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("invalid limiter settings", "error", err)
	}
	return l
}

// ExampleNew builds an instance from the defaults (DESIGN §A5.2, §A5.10): the host sets the
// required fields, gives the store address and the rl_lib password it was handed, and
// registers a refund listener that hands each event off without blocking (HOST-8).
func ExampleNew() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Shared settings: identical on every instance and in the policy file given to rladmin
	// start-epoch, or the fingerprints differ (HOST-5).
	p := limiter.DefaultPolicy()                            // 10 tokens per second, capacity 20
	p.MaxTrackedBuckets, p.MaxLiveMarkers = 100_000, 50_000 // required: sized per STA-7

	// Per-instance settings. DrainTimeoutNs defaults to refund_deadline + outage_tolerance.
	in := limiter.DefaultInstance(p)
	in.MaxReleaseDelayNs = maxReleaseDelay.Nanoseconds() // required (RCP-8)
	in.MarginNs = time.Second.Nanoseconds()              // required: ≥ 60 s × 1,000 ppm
	in.MaxHeldReceipts, in.MaxPendingRefunds = 1_152, 1_152

	st := limiter.StoreOptions{
		Addr:     "127.0.0.1:6390",
		User:     "rl_lib",
		Password: os.Getenv("RL_LIB_PASSWORD"), // never in code or config files
	}

	// The host's own bounded queue: a goroutine of the host drains it and matches each
	// DeductionID to its own records (REF-8).
	events := make(chan limiter.RefundEvent, 1_024)
	listener := func(ev limiter.RefundEvent) {
		select {
		case events <- ev: // fast hand-off; never call back into the limiter here
		default: // full: the host counts its own drop
		}
	}

	l, err := limiter.New(p, in, st, limiter.WithRefundListener(listener))
	if err != nil {
		// l is still usable: permanently not ready (settings_invalid), and it never
		// contacts the store.
		logger.Error("invalid limiter settings", "error", err)
	}
	defer l.Close()

	// Until the instance has joined the epoch that rladmin start-epoch created, Health is
	// not_ready (CFG-6): reason no_epoch while no epoch record exists, or no read has
	// succeeded yet (Acquire returns not_ready); fingerprint_mismatch when the record's
	// fingerprint differs (config_error); state_lost or integrity_hold when the record is
	// malformed or held, or the high-water mark is missing or malformed (an epoch-wide
	// state_integrity_error).
	h := l.Health()
	logger.Info("limiter built", "status", h.Status, "reason", h.Reason)
}

// ExampleLimiter_Acquire runs one protected operation (SPEC HOST-1 … HOST-4): the deadline is
// set just before Acquire and bounds the work too, the work runs only on allowed or degraded,
// every permit is finished, and every other result is a refusal of its own, never a quota
// rejection.
func ExampleLimiter_Acquire() {
	l := hostLimiter()
	defer l.Close()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	userID := "user-42" // from a bounded identity space; hash secrets first (HOST-9)

	// The protected work, bounded by ctx: nil on success, errConfirmedFailure when it
	// provably did nothing (say, a rolled-back transaction), any other error when its outcome
	// is unknown (say, a lost commit reply, or P passing during the commit).
	work := func(ctx context.Context) error { return ctx.Err() }

	// P starts just before Acquire, so a slow acquire shortens the time left for the work,
	// never the refund window (HOST-3, HARNESS HTTP-13). The library caps the acquire itself
	// at DependencyDeadlineNs (2 s).
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(maxReleaseDelay))
	defer cancel()
	r := l.Acquire(ctx, userID)

	switch r.Kind {
	case limiter.Allowed, limiter.Degraded:
		// Degraded: open mode admitted the call without a receipt; report it apart.
		err := work(ctx)
		switch {
		case err == nil:
			r.Permit.Done() // success: the charge stays
		case errors.Is(err, errConfirmedFailure):
			r.Permit.Release() // confirmed failure: refunded in the background
		default:
			r.Permit.Done() // unknown outcome: the charge stays, no refund
		}
	case limiter.Limited:
		// The only quota rejection: tell the caller to retry after RetryAfterNs.
		logger.Info("rate limited", "user_id", userID, "retry_after_ns", r.RetryAfterNs)
	case limiter.Busy:
		logger.Warn("overloaded: no free receipt or refund slot", "user_id", userID)
	case limiter.Cancelled:
		logger.Warn("deadline passed before acquire was sent", "user_id", userID)
	case limiter.Unavailable:
		logger.Warn("store unavailable or not ready (closed mode)", "user_id", userID)
	case limiter.OutcomeUnknown:
		// Maybe charged: no work, and never a second acquire for this call (REF-7).
		logger.Warn("acquire outcome unknown", "user_id", userID, "store_error", r.Diag.StoreError)
	case limiter.ReceiptError:
		logger.Error("charged, but signing failed", "user_id", userID, "reason", r.Diag.Reason)
	case limiter.ConfigError:
		logger.Error("settings or epoch mismatch", "user_id", userID, "reason", r.Diag.Reason)
	case limiter.NotReady:
		logger.Warn("limiter not ready", "user_id", userID, "reason", r.Diag.Reason)
	case limiter.ClockError:
		logger.Error("store clock unreadable", "user_id", userID, "reason", r.Diag.Reason)
	case limiter.StateIntegrityError:
		logger.Error("state loss: an operator must repair", "user_id", userID,
			"scope", r.Diag.Scope, "reason", r.Diag.Reason)
	case limiter.CapacityError:
		logger.Error("shared-state limit full", "user_id", userID, "reason", r.Diag.Reason)
	case limiter.ValidationError:
		logger.Info("invalid key", "reason", r.Diag.Reason)
	}
}

// ExampleLimiter_Close is a graceful stop (SPEC HOST-6, API-1e): the host stops taking new
// work, lets in-flight work finish or cancel within max_release_delay, then calls Close,
// which drains the pending refunds. The platform's kill grace must exceed
// max_release_delay + the drain + shutdown overhead: 5 s + 35 s + 5 s = 45 s in the harness.
func ExampleLimiter_Close() {
	l := hostLimiter()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	var inFlight sync.WaitGroup // the host's in-flight work, each piece ended by its own P deadline
	// … on SIGTERM the host stops accepting work, then waits for the pieces in flight.
	inFlight.Wait()

	// The default drain (DrainTimeoutNs) is refund_deadline + outage_tolerance = 35 s; a
	// shorter one gives up the refund guarantee for the refunds still pending (REF-9).
	drain := 35 * time.Second
	l.Close(limiter.WithDrainTimeout(drain)) // returns as soon as every refund is final

	// Refund events the listener queue had to drop; 0 means no refund event was dropped
	// (always 0 without a listener, as here).
	logger.Info("limiter closed", "listener_drops", l.Health().ListenerDrops)
}
