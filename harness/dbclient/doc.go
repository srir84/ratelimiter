// Package dbclient is the harness's only PostgreSQL client (AGENTS.md §1 "Database"; DESIGN
// §B3): hitapi's insert-or-read of one operation's row, classified by what is known about the
// row afterwards, plus the table and settings helpers the harness and its tests need, so no
// other package imports pgx or holds SQL. Nothing here retries a statement (AGENTS.md §6):
// the caller maps the class to done() or release().
package dbclient
