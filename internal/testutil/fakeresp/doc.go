// Package fakeresp is a fake RESP2 store for the L1 tests of internal/store
// (IMPLEMENTATION_PLAN S3 step 9, DESIGN §C1): a TCP server on 127.0.0.1 that answers
// AUTH, PING, SCRIPT LOAD and EVALSHA, with knobs that delay, fail, stall, hold or drop
// replies and connections, and counters of what arrived.
//
// It is a test helper: production code never imports it.
package fakeresp
