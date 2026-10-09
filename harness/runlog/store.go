package runlog

import "strconv"

// fileLimitScript is how a run's valkey-server and hitapi processes start (DESIGN §B1;
// IMPLEMENTATION_PLAN H3 step 4): Go raises its own soft RLIMIT_NOFILE and restores the
// original soft limit for the children it starts, so the child's shell raises it to 8,192,
// then execs the program, which keeps the shell's PID and process group.
const fileLimitScript = `ulimit -S -n 8192; exec "$0" "$@"`

// RewritePercentageOff is the auto-aof-rewrite-percentage StoreArgs sets unless the scenario
// forces rewrites (DESIGN §A2, §B5 "Forced rewrites"): automatic AOF rewrites off.
const RewritePercentageOff = "0"

// FileLimitCommand returns the command line that runs prog with args through the file-limit
// shell (fileLimitScript): the orchestrator starts the run's store and instances so, and
// faultctl restarts them with the same command lines (IMPLEMENTATION_PLAN H4 step 3).
func FileLimitCommand(prog string, args ...string) []string {
	return append([]string{"/bin/sh", "-c", fileLimitScript, prog}, args...)
}

// StoreArgs returns a run's valkey-server arguments (IMPLEMENTATION_PLAN H3 step 3; DESIGN
// §A2, §B5): the durable profile's file conf, the port, the run's store directory and ACL
// file, and --auto-aof-rewrite-percentage 0 (RewritePercentageOff) unless the scenario forces
// rewrites, which keeps the profile's production rewrite settings (§B5 "Forced rewrites"). The
// paths are absolute: Valkey changes into --dir before it reads the ACL file. It is the one
// definition of the store's command line (plan H4 step 3, plan choice): the orchestrator starts
// the store with it, and faultctl's store down rebuilds the same line from what the running
// store reports, so a restarted store keeps its port, data directory, ACL file and rewrite
// setting.
func StoreArgs(conf string, port int, dir, aclPath string, forcedRewrites bool) []string {
	args := []string{conf, "--port", strconv.Itoa(port), "--dir", dir, "--aclfile", aclPath}
	if !forcedRewrites {
		args = append(args, "--auto-aof-rewrite-percentage", RewritePercentageOff)
	}
	return args
}
