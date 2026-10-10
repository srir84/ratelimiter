//go:build rltest

// Package driver is the direct library driver (DESIGN §C4; HARNESS §9.4; IMPLEMENTATION_PLAN
// S5 steps 10, 11 and 16, S6 steps 10 and 13, S7 step 11, S8 steps 5 and 10): one store per
// driver (the fast profile unless Options.Profile says otherwise), its controlled time and
// epoch, and N limiter instances built through limitertest, each with its own store wrapper,
// signer wrapper, send observer, refund listener and signing-key canary. It logs every acquire,
// permit finish, final refund event, clock step, cleanup step, cleanup page read, epoch start or
// replace, restore, clear and inspect snapshot for the reference model (CheckRefModel),
// captures every refund attempt and cleanup step, reads the store as the ACL users the plan
// names (StartEpoch, ReadEpoch, Status and Inspect as rl_admin; the clock, Fixture and Tamper as
// rl_fixture; Dump, StoreKeys, SetHold and the audit as rl_lib; Info as rl_ops), runs rladmin as
// rl_admin (RLAdmin, ReplaceEpoch, Restore, Clear, LogInspect), restarts the store with its data
// kept, puts an instance behind its own RESP proxy on request (InstanceSpec.ViaProxy, S9 step
// 6), and at teardown, once every instance is closed or killed and while the store still
// runs, checks the commands and refund attempts each instance sent, audits the store and its
// AOF files, and checks BUD-11 on the op log.
//
// The methods that read the store or build instances fail the test through the driver's
// testing.TB, so call them from the test's goroutine; Acquire and the accessors may run on
// any goroutine. Shapes are plan choices (DESIGN §C8).
package driver
