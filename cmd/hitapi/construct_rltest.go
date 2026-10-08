//go:build rltest

package main

import (
	"ratelimiter/harness/config"
	"ratelimiter/harness/fixtures"
	"ratelimiter/harness/httpapi"
	"ratelimiter/limiter"
	"ratelimiter/limitertest"
)

// construct builds the test binary's limiter through limitertest.New (DESIGN §B5
// "Construction"), so its /health reports fixtures: true: behind the S5 store wrapper (no
// fault armed), with the controlled clock allowed when clock_source = "controlled", and with
// listener as its refund listener. The handler's database is db behind the database wrapper
// (FLT-5, FLT-9; no fault armed), and the /_test routes drive both wrappers (§B6). /health
// also gets the listener queue length. It never returns a nil limiter.
func construct(f config.File, db httpapi.DB, listener func(limiter.RefundEvent)) (built, error) {
	stores := fixtures.NewStoreWrapper()
	dbs := fixtures.NewDBWrap(db)
	opts := []limitertest.Option{
		limitertest.WithStoreWrapper(stores.Wrap),
		limitertest.With(limiter.WithRefundListener(listener)),
	}
	if f.Policy.ClockSource == limiter.Controlled {
		opts = append(opts, limitertest.WithControlledClock())
	}
	l, err := limitertest.New(f.Policy, f.Instance, f.Store, opts...)
	return built{
		lim:              l,
		listenerQueueLen: func() int { return limitertest.ListenerQueueLen(l) },
		db:               dbs,
		fixtures:         httpapi.Fixtures{Store: stores, DB: dbs},
	}, err
}
