// Command faultctl arms the harness's process and store faults from the command line (DESIGN
// §B6; IMPLEMENTATION_PLAN H4 step 3), on the run whose directory --run names:
//
//	faultctl --run runs/<id> store down|pause <duration>
//	faultctl --run runs/<id> api kill <N> [--restart]
//	faultctl --run runs/<id> tamper <op> [--key K] [--member g:s] [--field f] [--value v]
//	         [--target ix|q|epoch] [--epoch ID]
//	faultctl --run runs/<id> clock set|advance|back <ns>
//
// It is a thin layer over harness/fixtures, built only into bin/test (§B1): the command grammar
// (ParseFaultCommand: the tamper operations, one per §B6 item, and the flags each takes are
// listed at fixtures.TamperCommand), StoreDown and StorePause (FLT-1 a, FLT-2), KillAPI
// (FLT-6), the S8 tamper tool (FLT-7, FLT-11) and the S5 controlled clock driver. The
// orchestrator's fault runner runs the same functions in-process (plan H4 step 7).
//
// The tool needs the test build: its faults live in harness/fixtures, which only -tags rltest
// builds contain (DESIGN §B1, §B6; IMPLEMENTATION_PLAN H4 steps 3 and 8), so bin/test/faultctl
// is the real tool. Without the tag, main_stub.go keeps untagged builds of ./... compiling: it
// prints why and exits 2.
//
// The run directory gives the store's address (config.toml's [store] addr) and the run's
// processes (pids.json). store down rebuilds the store's exact command line from what the
// running store reports (runlog.StoreArgs) and writes the restarted store's PID into
// pids.json; api kill reads the instance's command line from its process and writes a
// restarted instance's PID into pids.json, so the sampler finds either. faultctl is for stacks
// without an orchestrator: every command changes the store or a process, which an orchestrated
// run's evidence would not show (its context.json records the scenario's [[faults]] alone),
// and a process faultctl restarts is detached from it: an orchestrator does not reach it with
// its final SIGTERM, and would fail the run. So faultctl refuses every command while the run's
// orchestrator is live (plan choice: .data/orchestrator.pid in the repository root, the run
// directory's grandparent, names a live process; the orchestrator holds it for its whole run):
// a scenario's [[faults]] arm them instead, run inside the orchestrator and recorded in
// context.json (plan H4 step 7). A standalone stack's run directory (config.toml and pids.json,
// no orchestrator) is accepted, with or without a context.json.
// Passwords come only from the environment (DESIGN §A2): RL_OPS_PASSWORD for the store faults,
// RL_FIXTURE_PASSWORD for tamper and clock, and RL_LIB_PASSWORD, handed to the restarted
// instance, for api kill --restart. rl_fixture cannot read the epoch record, so the operations
// that change the epoch's namespace take its ID from --epoch, or else from the epoch_id an
// instance of the run serves on GET /health (the instance found through pids.json and its
// --listen).
//
// It prints one JSON line on stdout per command: cmd, start_ns and end_ns, the command's own
// start and end (wall-clock ns, DESIGN §B5 "Faults"), and for a store fault whose outage was
// measured, outage_start_ns (plan choice: the wall-clock ns just before SHUTDOWN or SIGSTOP)
// and outage_ns (from there to the first PING answered, on the monotonic clock); error when
// the command failed. Errors and the usage go to stderr as JSON log lines. SIGTERM and SIGQUIT
// end a command early, always (Go's runtime replaces a SIGQUIT ignore faultctl inherits with
// its own handler), and so do SIGINT and SIGHUP (a closed terminal) unless faultctl started
// with them ignored (stopSignals); a fault's own recovery still runs: the store gets SIGCONT
// after a pause, and comes back after a down.
//
// Exit codes (plan choice): 0 ok; 1 the command failed; 2 bad arguments or environment, nothing
// done.
package main
