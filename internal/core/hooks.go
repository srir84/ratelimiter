package core

import (
	"io"

	"ratelimiter/internal/receipt"
	"ratelimiter/internal/store"
)

// hookSet holds the test hooks of the test-only entry point (DESIGN §A5.12). It stays
// unexported, so only NewWithHooks, which exists in rltest builds alone, can fill it; New
// passes the zero value, which is production: the store clock only, the real adapter and
// signer, no send observer, and the signing key from crypto/rand.
type hookSet struct {
	controlledClock bool                                // accept ClockSource = Controlled (CMP-9)
	storeWrapper    func(store.Adapter) store.Adapter   // stands between the limiter and the adapter
	signerWrapper   func(receipt.Signer) receipt.Signer // stands between the limiter and the signer
	sendObserver    func(cmd string, bytesWritten int)  // the adapter's send observer (§A5.4)
	keyReader       io.Reader                           // the signing key's source; nil means crypto/rand.Reader
	fixtures        bool                                // Health.Fixtures
}
