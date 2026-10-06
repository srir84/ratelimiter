package core

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/metrics"
	"ratelimiter/internal/receipt"
	"ratelimiter/internal/settings"
	"ratelimiter/internal/store"
)

// Limiter is one limiter instance (DESIGN §A5.1, §A5.2): its own store connections, slots,
// epoch watch and metrics, bound to one deployment and its dedicated store. One process may
// build several. Its methods are safe for concurrent use.
type Limiter struct {
	p          Policy
	in         Instance
	fixtures   bool   // built through NewWithHooks
	invalid    bool   // settings_invalid: never ready, no goroutine, no connection (set once in newLimiter)
	fp         string // fingerprint.Of(p)
	capUnits   int64  // Capacity × RatePeriodNs, below 2^52 once validated (§A3.1)
	lifetimeUs int64  // ReceiptLifetimeNs / 1000
	reg        *metrics.Registry
	slots      *slots
	signer     receipt.Signer
	adapter    store.Adapter
	watch      *epochWatch

	epoch   atomic.Pointer[epochState] // the watch's latest snapshot
	closing atomic.Bool                // Close or Kill began

	storeMu  sync.Mutex
	storeErr string // Health.StoreError

	stop      context.CancelFunc // ends the lifetime context: the watch and the connections
	wg        sync.WaitGroup     // the watch goroutine
	closeOnce sync.Once
}

// New is production construction (DESIGN §A5.2; limiter.New wraps it): no test hooks, so
// ClockSource = Controlled is settings_invalid (CMP-9, §A5.12). It never returns nil.
func New(p Policy, in Instance, st StoreOptions, opts ...Option) (*Limiter, error) {
	return newLimiter(p, in, st, hookSet{}, opts)
}

// newLimiter builds a limiter for New and NewWithHooks (DESIGN §A5.2): it validates the
// settings (CFG-4, §A3.1, RCP-8, §A5.6), generates the signing key from crypto/rand or the
// hook reader (RCP-4), computes the fingerprint (§A5.9), starts the store adapter (§A5.4) and
// the epoch watch (§A5.7). Any failure gives a non-nil limiter that is permanently not ready
// (settings_invalid) and opens no connection, plus the error for the host to log. The error
// names the failed rule, never a setting's value or the store password.
func newLimiter(p Policy, in Instance, st StoreOptions, h hookSet, opts []Option) (*Limiter, error) {
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	reg := metrics.NewRegistry()
	l := &Limiter{
		p: p, in: in, fixtures: h.fixtures, reg: reg,
		slots: newSlots(in.MaxHeldReceipts, in.MaxPendingRefunds, reg),
	}
	l.epoch.Store(&epochState{state: wsNoEpoch})
	if err := settings.Validate(p, in, h.controlledClock); err != nil {
		l.invalid = true
		return l, fmt.Errorf("limiter not ready (settings_invalid): %w", err)
	}
	keyReader := h.keyReader
	if keyReader == nil {
		keyReader = rand.Reader
	}
	signer, err := newSigner(keyReader, h.signerWrapper)
	if err != nil {
		l.invalid = true
		return l, fmt.Errorf("limiter not ready (settings_invalid): %w", err)
	}
	l.signer = signer
	l.fp = fingerprint.Of(p)
	l.capUnits = p.Capacity * p.RatePeriodNs
	l.lifetimeUs = p.ReceiptLifetimeNs / 1000

	// The lifetime context (AGENTS.md §2): work that outlives a call runs on it, and Close
	// ends it.
	life, stop := context.WithCancel(context.Background())
	var start chan struct{} // nil in production: the adapter dials at once
	if h.storeWrapper != nil {
		// No dial before the wrapper's result is known: a nil result is settings_invalid,
		// which opens no connection (§A5.2).
		start = make(chan struct{})
	}
	var ad store.Adapter = store.New(life, store.Options{
		Addr: st.Addr, User: st.User, Password: st.Password,
		StoreConns: in.StoreConns, RefundConns: in.RefundConns,
		DependencyDeadline: time.Duration(in.DependencyDeadlineNs),
		Limits:             store.LimitsFor(p),
		SendObserver:       h.sendObserver,
		Metrics:            reg,
		Start:              start,
	})
	if h.storeWrapper != nil {
		inner := ad
		if ad = h.storeWrapper(inner); ad == nil {
			stop()
			inner.Close()
			l.invalid = true
			return l, errors.New("limiter not ready (settings_invalid): the store wrapper returned no adapter")
		}
		if start != nil {
			close(start) // the wrapper's result is known: the adapter may dial
		}
	}
	l.adapter, l.stop = ad, stop
	l.watch = newEpochWatch(l, time.Duration(in.EpochRereadIntervalNs))
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.watch.run(life)
	}()
	return l, nil
}

// newSigner reads the signing key from keyReader and wraps the signer with wrap when it is
// set (§A5.12).
func newSigner(keyReader io.Reader, wrap func(receipt.Signer) receipt.Signer) (receipt.Signer, error) {
	hs, err := receipt.NewHMACSigner(keyReader)
	if err != nil {
		return nil, fmt.Errorf("signing key (RCP-4): %w", err)
	}
	var s receipt.Signer = hs
	if wrap != nil {
		if s = wrap(s); s == nil {
			return nil, errors.New("the signer wrapper returned no signer")
		}
	}
	return s, nil
}

// Close stops the instance (plan choice for S5; S6 adds the drain): from now on Acquire
// returns not_ready and Health reports closing; the epoch watch stops and every connection
// closes. It returns once the watch and the connection goroutines have ended, and is safe to
// call more than once.
func (l *Limiter) Close() { l.shutdown() }

// shutdown marks the instance closing, ends the lifetime context, waits for the watch and
// closes the adapter, which fails the calls in flight and waits for its goroutines. Close
// and Kill share it; S6 puts the drain in front of it for Close only.
func (l *Limiter) shutdown() {
	l.closing.Store(true)
	l.closeOnce.Do(func() {
		if l.stop != nil {
			l.stop()
		}
		l.wg.Wait()
		if l.adapter != nil {
			l.adapter.Close()
		}
	})
}

// setStoreError sets Health.StoreError ("" clears it).
func (l *Limiter) setStoreError(s string) {
	l.storeMu.Lock()
	defer l.storeMu.Unlock()
	l.storeErr = s
}

func (l *Limiter) storeError() string {
	l.storeMu.Lock()
	defer l.storeMu.Unlock()
	return l.storeErr
}
