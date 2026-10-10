//go:build rltest

// Package pgtest starts the harness PostgreSQL cluster for tests (IMPLEMENTATION_PLAN H1 step
// 5): the cluster make setup initialized in .data/pg (127.0.0.1:5433, role rl, database hits;
// DESIGN §C8). The server is shared by every test and left running; tests never record it as
// a child process. A missing pg_ctl or a server that cannot start is a failure, never a skip.
package pgtest
