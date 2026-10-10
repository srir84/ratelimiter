//go:build rltest

// Package fixtures holds the test-build fixtures of DESIGN §B6 (HARNESS §9.4): the
// controlled clock driver, the store-adapter wrapper (refuse, drop reply, delay, barriers),
// the signer wrapper (fail, unusable, corrupt) and the state tamper tool (FLT-7, FLT-11).
// Every file carries //go:build rltest, so no benchmark or production build can reach them.
// Shapes are plan choices (IMPLEMENTATION_PLAN S5 step 9, S6 step 10, S8 step 4).
package fixtures
