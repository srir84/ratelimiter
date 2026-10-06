//go:build rltest

package core

import (
	"os"
	"testing"

	"ratelimiter/internal/testutil"
)

// TestMain fails the package when a goroutine of internal/core or internal/store outlives
// the tests (AGENTS.md §2).
func TestMain(m *testing.M) { os.Exit(testutil.VerifyNoLeaks(m)) }
