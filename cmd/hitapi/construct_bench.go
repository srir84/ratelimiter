//go:build !rltest

package main

import (
	"ratelimiter/harness/config"
	"ratelimiter/harness/httpapi"
	"ratelimiter/limiter"
)

// construct builds the benchmark binary's limiter through limiter.New (DESIGN §B5
// "Construction"), with listener as its refund listener; the handler uses db as it is, and
// there are no fixtures or /_test routes (§B6). It never returns a nil limiter.
func construct(f config.File, db httpapi.DB, listener func(limiter.RefundEvent)) (built, error) {
	l, err := limiter.New(f.Policy, f.Instance, f.Store, limiter.WithRefundListener(listener))
	return built{lim: l, db: db}, err
}
