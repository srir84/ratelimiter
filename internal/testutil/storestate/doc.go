// Package storestate reads and audits the store's state in tests (IMPLEMENTATION_PLAN S4
// steps 10 and 13; DESIGN §C3, §C4). Snapshot reads every rl:* key through an rl_lib
// connection, and Audit checks a snapshot against the §A3.1–§A3.3 rules and the keys a
// test declares. It is a test helper: production code never imports it.
package storestate
