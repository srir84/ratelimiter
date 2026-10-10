// Package fingerprint computes the policy fingerprint (DESIGN §A5.9; SPEC CFG-1): a hash
// of every fingerprinted setting plus the store schema version and the time origin. Every
// instance of a deployment must compute the same value, and the epoch record holds it.
// The epoch ID and the signing key are never part of it.
package fingerprint
