// Command trace holds the repository checks of DESIGN §C7 (IMPLEMENTATION_PLAN S0
// step 6): test-ID and plan-named-test traceability, the import boundary, the ACL
// lists, script lint, inline suppressions, build tags, test globals, oracle use,
// leftover child processes, and the go test wrapper that the make targets use.
//
// Usage: trace <command> [flags]; `trace help` lists the commands.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := 2
	if wd, err := os.Getwd(); err != nil {
		fmt.Fprintf(os.Stderr, "trace: working directory: %v\n", err)
	} else {
		code = run(ctx, wd, os.Args[1:], os.Stdout, os.Stderr)
	}
	stop()
	os.Exit(code)
}
