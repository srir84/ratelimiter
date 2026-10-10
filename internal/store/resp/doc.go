// Package resp is the limiter's minimal RESP2 client (DESIGN §0 store client row, §A1):
// command encoding, a bounded reply reader and the AUTH handshake. It speaks RESP2 only
// and never sends HELLO. The pipelined connections built on it live in internal/store.
package resp
