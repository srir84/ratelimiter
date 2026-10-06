// Package store is the TestAPI fixture's store adapter interface.
package store

import "context"

type Request struct{ Script string }

type Response struct{ Outcome int }

type Adapter interface {
	Call(ctx context.Context, req Request) Response
	CallBatch(ctx context.Context, reqs []Request) []Response
	Do(ctx context.Context, args ...string) Response
	ReadyConns(kind int) int
	Close()
}
