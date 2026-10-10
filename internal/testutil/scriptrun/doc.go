// Package scriptrun runs the store scripts in L2 tests (DESIGN §C3; IMPLEMENTATION_PLAN S4
// step 14). Store gives a test a fast-profile store whose state is audited at teardown
// (storestate.Audit), and Run calls a script through the store adapter, instrumented for
// the script discipline run when RL_SCRIPT_AUDIT=1, and records every decoded reply in
// .out/replies-l2.jsonl for reply-code closure (in the file RL_REPLIES_FILE names instead,
// when it is set: bin/mutate's runs). The package keeps no state (AGENTS.md §5).
// It is a test helper: production code never imports it.
package scriptrun
