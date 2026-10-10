package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, func() (config, error) {
		wd, err := os.Getwd()
		if err != nil {
			return config{}, fmt.Errorf("working directory: %w", err)
		}
		root, err := moduleRoot(wd)
		if err != nil {
			return config{}, err
		}
		return defaultConfig(root, os.Environ()), nil
	})
	stop()
	os.Exit(code)
}

// moduleRoot returns the nearest directory at or above dir that holds a go.mod, with its
// symlinks resolved. The overlay names files under this path, and goTest gives each go command
// PWD set to it, so the go command names them the same way: it takes its working directory
// from $PWD whenever $PWD names that directory, through a symlink or not.
func moduleRoot(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("absolute path of %s: %w", dir, err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			real, err := filepath.EvalSymlinks(d)
			if err != nil {
				return "", fmt.Errorf("resolving the module root %s: %w", d, err)
			}
			return real, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no go.mod in %s or any parent directory", dir)
		}
		d = parent
	}
}
