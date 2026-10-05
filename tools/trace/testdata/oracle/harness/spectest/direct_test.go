package spectest

import (
	"testing"

	"ratelimiter/internal/core"
)

func TestDirect(t *testing.T) {
	expected := core.RetryAfterNs(1, 2, 3) // want
	var res core.Result
	_ = res.RetryAfterNs
	_ = core.Result{RetryAfterNs: 5}
	_ = expected
}
