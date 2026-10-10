// Package receipt holds the signed receipt the library keeps for each allowed acquire, its
// MAC input, its deduction ID and the epoch ID format (DESIGN §A5.9; SPEC RCP-1, RCP-4).
// Receipts are process-local: only the instance that signed one verifies it (signer.go).
// All integers are encoded big-endian.
package receipt
