// Package runlog writes the harness run logs (DESIGN §B3 "Run-log writer", §B8): callers
// hand JSON records to a Writer through a fixed-capacity channel, and one goroutine issues
// one write(2) per line on a file opened with O_APPEND, with no userspace buffer, so every
// line handed to the kernel survives SIGKILL. Enqueue never blocks: a full channel counts a
// drop, and any drop invalidates the run (§B8). Shared by hitapi (H1), hitgen (H2) and the
// sampler (H3). It also defines pids.json, the run directory's process list that the
// orchestrator writes and the sampler reads (pids.go; IMPLEMENTATION_PLAN H3 steps 4 and 7),
// and the command lines that start a run's store and instances, which the orchestrator starts
// them with and faultctl restarts them with (store.go; plan H4 step 3).
package runlog
