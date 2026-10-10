//go:build rltest

// Package respproxy is a loopback TCP proxy in front of a store (IMPLEMENTATION_PLAN S9 step 5,
// a plan choice: DESIGN §B6 has no fixture for arbitrary error replies). Each client connection
// it accepts gets its own connection to the store. The proxy parses the RESP2 commands one way
// and the replies the other, and passes every byte through unchanged, except:
//   - InjectError(script, line, count): the next count EVALSHA commands that name that script's
//     SHA-1 are answered with the error reply line and never reach the store. Each connection
//     keeps its commands in a FIFO in arrival order, forwarded or answered, and writes the
//     replies in that order, so under pipelining every reply reaches the command it answers;
//   - Hang(): every current client connection is closed, and connections accepted from then on
//     are kept open but never answered: a connect that hangs (the TCP connect completes and the
//     AUTH never gets a reply), so a dial fails only at its own deadline (DESIGN §A5.4).
//
// It binds 127.0.0.1 only. Each connection's queues (the FIFO, and the store's replies waiting
// to be written) have fixed capacities: a connection whose queue is full is closed and counted
// (Refused), never left to grow or block (AGENTS.md §1 Queues). The armed injections have a
// fixed capacity too: InjectError refuses one more than InjectCap with an error.
// Its goroutines belong to the Proxy and end with Close. It logs nothing, so the AUTH password
// it passes through goes nowhere else.
package respproxy
