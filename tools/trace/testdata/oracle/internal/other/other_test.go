package other

import (
	"testing"

	"ratelimiter/internal/core"
)

func TestOutside(t *testing.T) {
	_ = core.RetryAfterNs(1, 2, 3)
}
