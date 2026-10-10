// Command trace holds the repository checks of DESIGN §C7 (IMPLEMENTATION_PLAN S0
// step 6): test-ID and plan-named-test traceability, the import boundary, the ACL
// lists, script lint, inline suppressions, build tags, test globals, oracle use, the
// public API, leftover child processes, and the go test wrapper that the make targets use.
//
// Usage: trace <command> [flags]; `trace help` lists the commands.
package main
