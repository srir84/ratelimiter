// Command hitgen is the harness's load generator (DESIGN §B1, §B4; HARNESS §5, §7;
// IMPLEMENTATION_PLAN H2): it builds a scenario's schedule from its seed, writes it to
// schedule.jsonl before anything is sent, sends it open loop to the hitapi instances, retries
// only after 429, and records every attempt in gen.jsonl, ending with a summary line.
//
//	hitgen --scenario FILE --targets HOST:PORT[,HOST:PORT…] --out DIR
//	       [--thresholds FILE] [--baseline] [--http-timeout 30s] [--max-conns 512]
//
// --targets names one loopback address per instance, in order (HAR-20). With the scenario's
// stop_early, each step is judged while the next one runs (stopearly.go); --thresholds adds
// the frozen per-step thresholds to that, except with --baseline (a baseline run, §B9). On
// SIGTERM hitgen stops launching, waits at most the HTTP timeout for the attempts in flight,
// writes the summary and exits 0.
//
// Exit codes (plan choice): 0 after the run, or after SIGTERM; 1 a runtime failure (the out
// directory, schedule.jsonl, gen.jsonl); 2 a bad command line, a refused scenario or a refused
// thresholds file.
package main
