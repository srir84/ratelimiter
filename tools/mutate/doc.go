// Command mutate is the mutation tester of DESIGN §C9 (IMPLEMENTATION_PLAN S4 step 16):
// bin/mutate --curated runs the curated planted bugs of curated.go whose files exist, each
// killed only by an assertion in one of its named tests; bin/mutate --sweep <files> runs the
// operator sweep within a time budget and lists survivors and untried mutants in
// .out/mutants.txt. A mutant reaches Go and embedded Lua through go test -overlay (the
// mutated copy and the overlay file live in a temp directory, never in the tree) and Python
// through a patched temp copy of its package. Only L1 and L2 tests run. Every go test it
// starts gets RL_REPLIES_FILE naming a file in a temp directory, so the replies its L2 tests
// record never reach .out/replies-l2.jsonl, where they would count for the reply-code closure.
//
// Usage: mutate --curated [ID…] | mutate --sweep <file>… [--budget 30m]; `mutate --help`.
package main
