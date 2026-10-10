// Package store is the limiter's store adapter (DESIGN §A5.4, SPEC API-22): the only
// library path to the store. It keeps pipelined call, refund and maintenance connections,
// runs scripts with EVALSHA (one SCRIPT LOAD and resend after NOSCRIPT), tracks which
// command bytes were written so "nothing was sent" is provable, classifies every failure,
// and decodes replies against their §A4.9 layouts and range checks.
package store
