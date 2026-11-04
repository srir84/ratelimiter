package store

import (
	"os"
	"testing"

	"github.com/srir84/ratelimiter/internal/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.VerifyNoLeaks(m)) }
