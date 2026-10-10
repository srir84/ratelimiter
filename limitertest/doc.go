//go:build rltest

// Package limitertest is the limiter's test-only entry point (DESIGN §A5.12). It exists only
// in builds with -tags rltest and never in production builds (SPEC CMP-9): every file carries
// //go:build rltest, the benchmark binaries in bin/bench are built without the tag, and
// limiter.New refuses the controlled clock, so production code cannot reach fixtures.
//
// New builds an instance through core.NewWithHooks, whose Health reports fixtures true. Its
// options add the controlled clock (WithControlledClock), store and signer wrappers
// (WithStoreWrapper, WithSignerWrapper), a send observer (WithSendObserver), a signing-key
// reader (WithKeyReader) and limiter options passed through (With). Kill stops an instance at
// once, as a process kill would: no drain, and pending refunds are lost. ListenerQueueLen and
// PermitEpoch read state that only tests need (plan choices). The direct library
// driver (harness/driver) and the rltest build of hitapi build their instances through it, and
// the harness/fixtures wrappers plug into its options.
package limitertest
