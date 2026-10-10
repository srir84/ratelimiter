// Package storeproc starts and stops throwaway stores for tests (IMPLEMENTATION_PLAN S1,
// DESIGN §C1): it finds the store binary, renders the ACL file with fresh passwords,
// spawns the store on a free loopback port with a deploy/ profile, records the child in
// .out/procs/ for bin/trace procs, pauses and resumes it and fills it with padding (S9:
// Pause, Resume, LoadPadding), and offers a small RESP2 client.
//
// It is a test helper: production code never imports it.
package storeproc
