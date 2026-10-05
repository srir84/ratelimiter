package spectest

import (
	"testing"

	"ratelimiter/internal/core"
)

func TestShadow(t *testing.T) {
	_ = core.Result{}
	core := struct{ RetryAfterNs int64 }{RetryAfterNs: 1}
	_ = core.RetryAfterNs
}
