// Command rladmin is the rate limiter's admin tool (DESIGN §A6; SPEC ADM-1…ADM-4): it starts
// and replaces epochs, restores and clears quarantined keys, shows the epoch's status and a
// key's state, and reports and rewrites the store's persistence. It connects as rl_admin
// (DESIGN §A2) and runs only the admin scripts and read_epoch, decoding every reply with the
// store's §A4.9 decoder.
//
// Commands (rladmin --help prints one line each):
//
//	rladmin start-epoch --policy FILE [--replace --expect-epoch ID]
//	rladmin status
//	rladmin inspect --key KEY
//	rladmin restore --policy FILE --key KEY [--balance-units U | --balance-tokens T] --expect-epoch ID
//	rladmin clear --policy FILE --key KEY --expect-epoch ID
//	rladmin store-info
//	rladmin store-rewrite
//
// Every command also takes --store ADDR and --json (a table by default).
//
//   - start-epoch (ADM-1) creates the epoch record, with a new 128-bit ID from crypto/rand and
//     the fingerprint of the policy file's [policy] table, and the high-water mark, and prints
//     epoch_id. Without --replace it needs no readable epoch record (a fresh or wiped store, or
//     a malformed record); with --replace it needs the current record's ID to equal
//     --expect-epoch. Every budget of the new epoch starts full.
//   - status (ADM-4) shows epoch_id, the fingerprint, the integrity hold, the high-water mark,
//     and the tracked-bucket, live-marker and quarantined-key counts. Read-only, unfenced.
//   - inspect (ADM-4) shows one key's stored state: present, balance in units, last update,
//     generation, sequence, latest receipt expiry, marker count and expiries, index entry and
//     quarantine flag. Read-only, unfenced.
//   - restore (ADM-2) deletes every stored entry of a quarantined key except its flag and
//     writes a bucket with a new generation and the given balance: 0 units by default,
//     --balance-units U with 0 ≤ U ≤ capacity × rate_period_ns, or --balance-tokens T, sent as
//     T × rate_period_ns.
//   - clear (ADM-3) removes a key's quarantine flag once a bucket is present and the key
//     passes every consistency check.
//   - store-info shows the store's INFO persistence, clients, memory and stats sections and
//     SLOWLOG GET 20; monitoring runs it on a schedule (DESIGN §A8).
//   - store-rewrite runs BGREWRITEAOF and waits for the rewrite to finish (before a planned
//     store restart, DESIGN §A8).
//
// start-epoch --replace, restore and clear are fenced by --expect-epoch in the same atomic
// step as the change; status shows the current ID. restore derives the fingerprint, cap_units
// and lifetime_us (receipt_lifetime_ns / 1000) from --policy; clear derives the fingerprint
// and cap_units only. The policy file is the TOML file whose [policy] table the instances use
// (DESIGN §B2); its absent keys keep limiter.DefaultPolicy's values. On a missing or
// malformed epoch record, status and inspect print the reason and point to start-epoch
// without --replace.
//
// The store address comes from --store or RL_STORE_ADDR, the user from RL_ADMIN_USER
// (default rl_admin) and the password from RL_ADMIN_PASSWORD, which no output, error or raw
// reply ever shows.
//
// Exit codes (all commands):
//
//   - 0: ok.
//   - 2: refused ({41}, the reason printed); also an invalid policy file or a usage error
//     (plan choices).
//   - 3: a store or integrity problem: {20}, {22}, {25}, an error reply, a reply that fails
//     the §A4.9 layout or range checks (printed raw), "epoch changed during read", or a
//     failed connection, AUTH or exchange, including the command's deadline (30 s; 10 min
//     for store-rewrite) or a signal ending it.
//   - 4: clock_error ({21}).
package main
