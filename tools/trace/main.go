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
