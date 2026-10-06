package store

import (
	"os"
	"testing"

	"ratelimiter/internal/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.VerifyNoLeaks(m)) }
