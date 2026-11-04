//go:build rltest

// Package spectest holds the SPEC tests (TestT_<ID>) and the design checks (TestDesign_…) that
// run through the direct library driver against real stores (DESIGN §C1 L3, §C4).
package spectest

import (
	"os"
	"testing"

	"github.com/srir84/ratelimiter/internal/testutil"
)

// TestMain fails the package when a goroutine of internal/core or internal/store outlives
// the tests (AGENTS.md §2).
func TestMain(m *testing.M) { os.Exit(testutil.VerifyNoLeaks(m)) }
