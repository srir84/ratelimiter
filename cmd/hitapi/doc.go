// Command hitapi is the harness's API process (DESIGN §B1, §B3; HARNESS §2, §3, §9.2): a host
// of the limiter that serves GET /hit (header checks, the in-flight cap, acquire, the
// database row, done or release), /health and /metrics on a loopback address, and writes
// its run log (§B8).
//
//	hitapi --config FILE --listen 127.0.0.1:PORT --log PATH [--instance N] [--run-id ID]
//
// The store password comes only from RL_LIB_PASSWORD (harness/config). hitapi refuses to
// start when the soft RLIMIT_NOFILE is below 1,024 (§B1). The benchmark build constructs its
// limiter with limiter.New, the rltest build with limitertest.New (§B5 "Construction";
// construct_bench.go, construct_rltest.go); a baseline run (limiter_enabled = false) builds
// it too and only skips the calls. The rltest build also puts the database behind the
// fixtures' database wrapper and serves the /_test control routes (§B6).
//
// On SIGTERM (HAR-15, SPEC HOST-6) it stops accepting /hit, refuses work not yet at its first
// DB statement (ERR-15), waits at most P for the rest, closes the limiter with the default
// drain, waits until the run log's channel is empty, writes the stop record and exits 0. The
// orchestrator's kill grace is P + drain_timeout + 5 s, 45 s at harness values (§B10).
//
// Exit codes (plan choice): 0 after SIGTERM; 1 a runtime failure (the run log, listening,
// serving); 2 a bad command line or config, RL_LIB_PASSWORD unset, or a soft RLIMIT_NOFILE
// below 1,024.
package main
