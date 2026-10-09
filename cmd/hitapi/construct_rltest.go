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
// "Construction", §A5.12), so its /health reports fixtures: true: behind the S5 store wrapper
// and the S5/S6 signer wrapper (no fault armed), heard by the send observer (the §A5.4 test
// hook, a bounded fixtures.SendLog), with the controlled clock allowed when clock_source =
// "controlled", and with listener as its refund listener. The handler's database is db behind
// the database wrapper (FLT-5, FLT-9; no fault armed), and the /_test routes drive the three
// wrappers and read the observer (§B6; IMPLEMENTATION_PLAN H4 steps 4 and 5). /health also gets
// the listener queue length. It never returns a nil limiter.
func construct(f config.File, db httpapi.DB, listener func(limiter.RefundEvent)) (built, error) {
	stores := fixtures.NewStoreWrapper()
	signers := fixtures.NewSignerWrapper()
	sends := fixtures.NewSendLog()
	dbs := fixtures.NewDBWrap(db)
	opts := []limitertest.Option{
		limitertest.WithStoreWrapper(stores.Wrap),
		limitertest.WithSignerWrapper(signers.Wrap),
		limitertest.WithSendObserver(sends.ObserveSend),
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
		fixtures:         httpapi.Fixtures{Store: stores, DB: dbs, Signer: signers, Sends: sends},
	}, err
}
