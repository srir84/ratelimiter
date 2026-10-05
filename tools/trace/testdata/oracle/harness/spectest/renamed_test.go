package spectest

import (
	"testing"

	c "ratelimiter/internal/core"
)

func TestRenamed(t *testing.T) {
	_ = c.DecisionNs(1) // want
}
