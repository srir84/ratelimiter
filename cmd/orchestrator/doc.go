// Command orchestrator runs one harness run end to end (DESIGN §B1, §B5; HARNESS §4, HAR-24;
// IMPLEMENTATION_PLAN H3): it checks the run before anything starts (preflight.go), takes the
// pid file (pidfile.go), creates the run directory with the merged config and the rendered ACL
// file (rundir.go), then runs the §B5 sequence (run.go).
//
//	orchestrator --scenario FILE [--baseline] [--instance-config N=FILE]… [--bin-dir DIR]
//
// --baseline runs with the limiter off ([api] limiter_enabled = false, HAR-17).
// --instance-config gives instance N its own config file in place of the run's config.toml
// (test runs only, T-HAR-1). --bin-dir is the directory hitapi and rladmin run from (default
// bin/<run_kind>); hitgen and the sampler always run from bin/test (§B1). It runs in the
// repository root, where it finds harness.toml, bin/, deploy/, runs/ and .data/.
//
// Exit codes (plan choice): 0 after a finished run; 1 a failure once the run has begun, or a
// signal (SIGINT, SIGTERM, SIGHUP or SIGQUIT); 2 a bad command line or a refused run. A
// refused run logs one "refusing the run" error line per reason found, each with a fixed
// reason code (preflight.go, run.go) and a detail. Preflight's refusals start nothing; the checks that need the running store or
// instances (maxclients, the benchmark guard's /health check) refuse once the run is torn down.
package main
