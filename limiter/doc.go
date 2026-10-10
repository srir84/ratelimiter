// Package limiter is the rate limiter's public API (DESIGN §A5.2): several instances, in one
// process or many, share one token budget per key through a dedicated store. Every type is
// defined in internal/core (or a lower internal package) and re-exported here as an alias;
// this package holds only thin wrappers: New, DefaultPolicy, DefaultInstance,
// WithRefundListener and WithDrainTimeout.
//
// # Construction
//
// A host starts from DefaultPolicy (the SPEC defaults, CFG-8) and DefaultInstance (the DESIGN
// defaults, §A5.10), sets the required fields, which have no default and fail validation while
// 0 (Policy.MaxTrackedBuckets and MaxLiveMarkers; Instance.MaxReleaseDelayNs, MarginNs,
// MaxHeldReceipts and MaxPendingRefunds), and calls New with the store address and the rl_lib
// user and password it supplies (StoreOptions, CFG-7). Every instance of a deployment gets
// identical Policy values (HOST-5): their fingerprint must equal the one the epoch record
// holds, which rladmin start-epoch wrote from the same policy. The library reads no
// environment, files or flags and has no logger: it reports through Result.Diag, Health,
// Metrics and refund events.
//
// New never returns nil. Invalid settings (CFG-4, RCP-8, the §A7 refund inequality),
// ClockSource = Controlled (CMP-9) or a failed signing-key generation (RCP-4) give an instance
// that is permanently not ready (settings_invalid) and contacts no store, and New also returns
// the error for the host to log. A valid instance joins the store's epoch in the background.
// Until it has, its epoch reads decide what Acquire returns (CFG-6, DESIGN §A5.7): not_ready
// (Health reason no_epoch) while no epoch record exists, or no read has succeeded yet;
// config_error (Health reason fingerprint_mismatch) when the record's fingerprint differs
// from the instance's; an epoch-wide state_integrity_error (Health reason state_lost or
// integrity_hold) when the record is malformed or held, or the high-water mark is missing or
// malformed. One process may build several instances, each with its own connections, slots
// and epoch.
//
// # Acquire results
//
// Acquire(ctx, key) asks for one token of key's budget. key is any host-chosen string of
// 1–256 bytes of UTF-8. ctx's deadline is the host's deadline, capped at DependencyDeadlineNs,
// and its cancellation is the host's cancel. Result.Kind is one of a closed set (SPEC API-4 …
// API-12a), and the host treats each distinctly (HOST-4):
//
//   - Allowed: a token was deducted; Permit holds the signed receipt and DecisionNs the
//     decision's budget time.
//   - Degraded: open mode only, returned whenever the acquire would be unavailable: the store
//     could not be reached and nothing was sent, or it refused the command before running it
//     because it was not ready (LOADING, BUSY). Permit holds no receipt and nothing was
//     charged. Do the work and report it apart from allowed.
//   - Limited: a quota rejection; RetryAfterNs is the exact wait.
//   - Busy, Cancelled, Unavailable (closed mode), OutcomeUnknown, ReceiptError, ConfigError,
//     NotReady, ClockError, StateIntegrityError, CapacityError and ValidationError: refusals
//     with no permit, so no protected work, never reported as quota rejections.
//     OutcomeUnknown may have charged a token (a deduction cannot be ruled out, API-12), and
//     ReceiptError did (charge known, no receipt, no permit, API-12a). After OutcomeUnknown or
//     Cancelled the host never acquires again for the same call (REF-7), and an unknown
//     outcome is never resolved by sending a second acquire (API-24).
//
// The protected work runs only on Allowed or Degraded (HOST-1). The Diag of an Acquire result
// explains it for operators and never holds a password, a signing key or a budget key. Never
// use a secret as a key; hash it first, because keys appear in inspect output and diagnostics
// (HOST-9). As built, a key is stored base64url-encoded, which is reversible, in the store's
// key names and script arguments, which rladmin store-info's slow log can show, and the raw
// store replies that a RunCleanupPass report's Diag.StoreError carries can hold encoded keys.
//
// # Done and Release
//
// Every permit is finished (HOST-2): Release only for a confirmed failure, Done for success or
// an unknown outcome. Done keeps the charge and frees the permit's slots. Release returns at
// once and the refund runs in the background: when Release comes within MaxReleaseDelayNs of
// just before Acquire (HOST-3), the refund is confirmed within refund_deadline plus one outage
// of at most outage_tolerance (REF-5, within the scope of REF-9). The first finish wins: any
// later Done or Release is a no-op, and both are no-ops on a degraded permit. A listener set
// with WithRefundListener receives each released permit's one final refund event; it must
// stay fast or hand events off, and never call back into the limiter (HOST-8).
//
// # Shutdown
//
// Close is the graceful stop (SPEC API-1e, HOST-6). From its start Acquire returns not_ready;
// it keeps retrying the pending refunds until all are final or the drain timeout passes
// (DrainTimeoutNs, refund_deadline + outage_tolerance by default, or WithDrainTimeout), and
// ends the rest as unresolved (closed). The refund listener gets until the same deadline to
// take its queued events; then its dispatcher is stopped without waiting for a callback that
// is still blocked, and later events are dropped and counted. When Close returns, the refund
// workers, the epoch watch and the cleanup runner have stopped and every connection is
// closed; every refund event was delivered, dropped and counted, or is in a callback still
// running, and that callback's dispatcher goroutine ends only when the callback returns (keep
// the listener fast, HOST-8). The platform's kill grace must exceed MaxReleaseDelayNs + the
// drain timeout + shutdown overhead.
package limiter
