package scripts_test

import (
	"testing"

	"ratelimiter/internal/core"
)

func TestScriptsDir(t *testing.T) {
	_ = core.DecisionNs(1) // want
}
