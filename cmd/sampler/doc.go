// Command sampler is the harness's resource sampler (DESIGN §B7; HARNESS HAR-7, PRF-4;
// IMPLEMENTATION_PLAN H3 step 7). Every interval it re-reads the run's process list
// (runs/<id>/pids.json) and takes CPU % and RSS (gopsutil) of each process in it
// (valkey-server, each hitapi, hitgen) and of the PostgreSQL postmaster with its children
// (PID from .data/pg/postmaster.pid); it reads each API's /health and /metrics, runs
// `rladmin status --json`, and reads the store's INFO persistence and INFO clients as rl_ops.
// It writes one samples.jsonl line per tick through harness/runlog.
//
//	sampler --run-dir DIR --apis HOST:PORT[,HOST:PORT…] --rladmin PATH
//	        [--interval 1s] [--pg-data .data/pg]
//
// The store address comes from RL_STORE_ADDR and the passwords from RL_OPS_PASSWORD (INFO) and
// RL_ADMIN_PASSWORD, which only rladmin's environment gets, with RL_STORE_ADDR (DESIGN §A2,
// §A6); no line, log or error shows a password. Every read of a tick ends by one deadline,
// 4/5 of the interval (plan choice), so a tick never runs into the next one: a read that fails
// or runs out of time records its error in the line, which is still written (plan choice). On
// SIGTERM the sampler finishes the tick in progress, writes its line and exits.
//
// Exit codes (plan choice): 0 after SIGTERM; 1 samples.jsonl could not be opened or written;
// 2 a bad command line or environment.
package main
