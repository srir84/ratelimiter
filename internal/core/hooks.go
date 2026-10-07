package core

import (
	"io"

	"ratelimiter/internal/receipt"
	"ratelimiter/internal/store"
)

// hookSet holds the test hooks of the test-only entry point (DESIGN §A5.12). It stays
// unexported, so only NewWithHooks, which exists in rltest builds alone, can fill it; New
// passes production: the store clock only, the real adapter and signer, no send observer, the
// signing key from crypto/rand, and cleanupDraw = jitterDraw, the cleanup runner's jitter from
// math/rand/v2. cleanupDraw is always set: New and NewWithHooks pass jitterDraw, and it has no
// Hooks field, since only this package's tests pass another (plan choice), to see each jitter
// draw of the runner.
type hookSet struct {
	controlledClock bool                                // accept ClockSource = Controlled (CMP-9)
	storeWrapper    func(store.Adapter) store.Adapter   // stands between the limiter and the adapter
	signerWrapper   func(receipt.Signer) receipt.Signer // stands between the limiter and the signer
	sendObserver    func(cmd string, bytesWritten int)  // the adapter's send observer (§A5.4)
	keyReader       io.Reader                           // the signing key's source; nil means crypto/rand.Reader
	cleanupDraw     func() float64                      // the runner's jitter draws in [0, 1); never nil
	fixtures        bool                                // Health.Fixtures
}
