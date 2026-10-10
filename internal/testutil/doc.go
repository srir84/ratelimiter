// Package testutil holds test helpers shared by the library's packages (DESIGN §A1): the
// goroutine leak check that TestMain runs (AGENTS.md §2) and InChild, which runs a test in a
// child copy of its test binary, with storeproc, fakeresp and the later helpers in its
// subpackages. It is test-only: production code never imports it.
package testutil
