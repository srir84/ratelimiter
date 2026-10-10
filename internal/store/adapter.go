package store

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"ratelimiter/internal/metrics"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store/resp"
)

// Kind names a connection set (DESIGN §A5.4).
type Kind int

const (
	Call        Kind = iota // Options.StoreConns connections: acquire, the epoch watch, hold
	Refund                  // Options.RefundConns connections, used only by refund workers
	Maintenance             // one connection: cleanup and reclaim
)

// String returns the rl_store_connections label: call, refund or maintenance.
func (k Kind) String() string {
	switch k {
	case Call:
		return "call"
	case Refund:
		return "refund"
	case Maintenance:
		return "maintenance"
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// Outcome is how one store call ended (API-22); internal/core maps it to a result.
type Outcome int

const (
	// OK: a reply arrived; for Call and CallBatch it passed Decode and is in Response.Reply.
	OK Outcome = iota
	// Unavailable: nothing was sent (no usable connection, refused, connect or AUTH timeout,
	// the dependency deadline before sending, the adapter closed), or the store refused the
	// command before running it with LOADING or BUSY.
	Unavailable
	// Cancelled: the host's own cancellation or deadline ended the call before anything was
	// sent, and before the dependency deadline did.
	Cancelled
	// OutcomeUnknown: a command was sent without a confirmed result (deadline or lost
	// connection after sending), an error reply, a failed NOSCRIPT reload or retry, or a
	// reply that failed Decode. It may have applied.
	OutcomeUnknown
	// CapacityError: OOM on acquire or refund, refused before running (§A3.0 row 6).
	CapacityError
)

// String returns the SPEC name of the outcome.
func (o Outcome) String() string {
	switch o {
	case OK:
		return "ok"
	case Unavailable:
		return "unavailable"
	case Cancelled:
		return "cancelled"
	case OutcomeUnknown:
		return "outcome_unknown"
	case CapacityError:
		return "capacity_error"
	}
	return "outcome(" + strconv.Itoa(int(o)) + ")"
}

// Request is one script call. ReceiptExpiryUs is set by refund calls, for the
// E_ALREADY_APPLIED range check (§A4.9).
type Request struct {
	Kind            Kind
	Script          scripts.Script
	Keys, Args      []string
	ReceiptExpiryUs int64
}

// Response is how a call ended.
//   - SentCommands counts the script commands (EVALSHA) written, at least one byte each: 0
//     or 1 normally, 2 after a NOSCRIPT reload and resend; SCRIPT LOAD is not counted, and a
//     command refused with LOADING or BUSY counts as not sent. Callers count API-1d attempts
//     from it. For Do it counts the one command (plan choice). Sent = SentCommands > 0.
//   - NoScript is set whenever a NOSCRIPT reply came back, whatever the resend's outcome.
//   - StoreError is the raw text of the error reply that decided the outcome; for a command
//     not sent because its kind fails fast after the store refused the last dial, the text
//     of that refusal (the AUTH reply, password redacted). Raw is the reply that failed
//     Decode, for Diag.
type Response struct {
	Outcome      Outcome
	Reply        Reply
	Sent         bool
	SentCommands int
	NoScript     bool
	StoreError   string
	Raw          string
}

// Adapter is the store adapter's interface (DESIGN §A5.4); limitertest aliases it, so test
// wrappers can stand between the library and the store (§A5.12).
type Adapter interface {
	// Call runs one script within min(ctx deadline, start + D), resending it once after
	// NOSCRIPT, and decodes the reply. ctx is the host's own context, never one already
	// capped at start + D: a call that sent nothing is cancelled only when the host's end
	// came first, else unavailable (API-1).
	Call(ctx context.Context, req Request) Response
	// CallBatch writes the scripts as one pipeline on one connection of reqs[0].Kind, with
	// no reload: a NOSCRIPT reply sets NoScript for the caller to resend. ctx may carry the
	// start of the call the pipeline continues (a resend), whose deadline it keeps; a wrapper
	// passes ctx on.
	CallBatch(ctx context.Context, reqs []Request) []Response
	// Do sends one non-script command (SCAN, PING) and returns its raw reply, classified
	// like a script's; like CallBatch, it keeps the call start ctx may carry.
	Do(ctx context.Context, kind Kind, args ...string) (resp.Value, Response)
	// ReadyConns returns the number of ready connections of a kind.
	ReadyConns(kind Kind) int
	// Close fails the calls in flight, stops every connection goroutine and waits for them;
	// later calls return Unavailable and send nothing.
	Close()
}

// Options configure New. The counts and the deadline come from validated settings
// (settings.Validate); a count below 1 is taken as 1.
type Options struct {
	Addr, User, Password string
	StoreConns           int           // call connections (8 by default)
	RefundConns          int           // refund connections (2 by default)
	DependencyDeadline   time.Duration // D: bounds each call, each dial with its AUTH, and the stall rule
	Limits               Limits        // the instance's reply range limits (LimitsFor)
	// Dial opens a TCP connection; nil means net.Dialer's DialContext. Tests inject
	// connections through it.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// SendObserver, when not nil, hears once per command how many of its bytes were written:
	// cmd is the script name for EVALSHA ("acquire"), else the command ("SCRIPT LOAD", "AUTH",
	// "PING"). A command that moves to another connection after a write that sent none of it
	// is heard for each write, with 0 for the first. It runs on the writing goroutine and must
	// not call the adapter.
	SendObserver func(cmd string, bytesWritten int)
	// Metrics receives rl_store_connections and rl_store_call_seconds; nil records nothing.
	Metrics *metrics.Registry
	// Rand draws the reconnect jitter from [0, 1); nil means math/rand/v2's Float64.
	Rand func() float64
	// Start, when not nil, holds every connection slot back until it is closed: no dial
	// begins before, and a slot whose lifetime ends first ends without dialing. internal/core
	// passes one while a test's store wrapper runs, so a limiter whose wrapper returns no
	// adapter opens no connection (plan S5 step 2). nil, as in production, dials at once.
	Start <-chan struct{}
}

// StoreAdapter is the production Adapter: a pipelined connection set per Kind.
type StoreAdapter struct {
	addr, user, pass string
	d                time.Duration
	limits           Limits
	dialFn           func(ctx context.Context, network, addr string) (net.Conn, error)
	obs              func(string, int)
	metrics          *metrics.Registry
	rand             func() float64
	start            <-chan struct{} // Options.Start: nil, or closed when the slots may dial

	life      context.Context // the owner's lifetime context; connection goroutines run on it
	stop      context.CancelFunc
	sets      [3]*connSet // indexed by Kind
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// New starts an adapter: every connection slot (StoreConns call, RefundConns refund and one
// maintenance) begins dialing at once (plan choice), or once Options.Start is closed when it
// is set. ctx is the owner's lifetime context
// (internal/core's, or t.Context() in tests); when it ends the connections close and stop
// redialing. Close stops them and waits.
func New(ctx context.Context, o Options) *StoreAdapter {
	life, stop := context.WithCancel(ctx)
	a := &StoreAdapter{
		addr:    o.Addr,
		user:    o.User,
		pass:    o.Password,
		d:       o.DependencyDeadline,
		limits:  o.Limits,
		dialFn:  o.Dial,
		obs:     o.SendObserver,
		metrics: o.Metrics,
		rand:    o.Rand,
		start:   o.Start,
		life:    life,
		stop:    stop,
	}
	if a.dialFn == nil {
		var d net.Dialer
		a.dialFn = d.DialContext
	}
	if a.rand == nil {
		a.rand = rand.Float64
	}
	counts := [3]int{max(o.StoreConns, 1), max(o.RefundConns, 1), 1}
	for k := range a.sets {
		a.sets[k] = newConnSet(Kind(k), life, o.Metrics)
	}
	for k, n := range counts {
		for range n {
			a.wg.Add(1)
			go a.runSlot(a.sets[k])
		}
	}
	return a
}

// set returns the connection set of k, or nil for a value outside the closed set.
func (a *StoreAdapter) set(k Kind) *connSet {
	switch k {
	case Call, Refund, Maintenance:
		return a.sets[k]
	}
	return nil
}

// Call runs one script (DESIGN §A5.4) and observes rl_store_call_seconds{script} once.
func (a *StoreAdapter) Call(ctx context.Context, req Request) Response {
	start := time.Now()
	r := a.call(ctx, start, req)
	if i, ok := scriptLabel(req.Script.Name); ok && a.metrics != nil {
		a.metrics.Observe(metrics.StoreCallSeconds, i, time.Since(start))
	}
	return r
}

func (a *StoreAdapter) call(ctx context.Context, start time.Time, req Request) Response {
	set := a.set(req.Kind)
	if set == nil || req.Script.SHA == "" {
		return Response{Outcome: OutcomeUnknown} // a library defect: nothing sent, no bypass
	}
	dep := start.Add(a.d)
	cctx, cancel := context.WithDeadlineCause(ctx, dep, errDependencyDeadline)
	defer cancel()
	lim := a.limits
	lim.ReceiptExpiryUs = req.ReceiptExpiryUs
	eval := command{label: req.Script.Name, args: evalshaArgs(req)}

	r := a.settle(ctx, dep, req.Script.Name, a.exchange(cctx, set, []command{eval})[0], lim)
	if !r.NoScript {
		return r
	}
	// NOSCRIPT proves the first EVALSHA did nothing: load the script, then send it once
	// more, within the same deadline. From here on, anything unconfirmed is
	// outcome_unknown, never unavailable (API-22). r is already OutcomeUnknown.
	load := a.exchange(cctx, set, []command{{label: "SCRIPT LOAD", args: []string{"SCRIPT", "LOAD", req.Script.Text}}})[0]
	switch {
	case !load.sent || load.err != nil:
		return r
	case load.v.Kind == resp.Error:
		r.StoreError = load.v.Str
		return r
	case load.v.Kind != resp.Bulk || load.v.Null || load.v.Str != req.Script.SHA:
		r.Raw = load.v.String() // the store loaded something else
		return r
	}
	retry := a.exchange(cctx, set, []command{eval})[0]
	if !retry.sent {
		return r
	}
	r2 := a.settle(ctx, dep, req.Script.Name, retry, lim)
	// The first EVALSHA counts, and so does the resend unless the store refused it before
	// running it (LOADING, BUSY: settle counted it as not sent, API-22), so 2 or 1 (API-1d).
	r2.Sent, r2.SentCommands, r2.NoScript = true, 1+r2.SentCommands, true
	if r2.Outcome == Unavailable || r2.Outcome == Cancelled {
		r2.Outcome = OutcomeUnknown
	}
	return r2
}

// CallBatch writes reqs as one pipeline on one connection of reqs[0].Kind, every command
// with its own byte range, within one deadline min(ctx deadline, start + D), start being now
// or the call start ctx carries (withCallStart). There is no reload: a NOSCRIPT response is
// OutcomeUnknown with NoScript set, for the cleanup runner to resend (§A5.4). Each
// SentCommands is 0 or 1. It observes rl_store_call_seconds{script} once per request, each
// with the pipeline's time.
func (a *StoreAdapter) CallBatch(ctx context.Context, reqs []Request) []Response {
	start := time.Now()
	out := a.callBatch(ctx, callStart(ctx, start), reqs)
	if a.metrics != nil {
		d := time.Since(start)
		for _, req := range reqs {
			if i, ok := scriptLabel(req.Script.Name); ok {
				a.metrics.Observe(metrics.StoreCallSeconds, i, d)
			}
		}
	}
	return out
}

func (a *StoreAdapter) callBatch(ctx context.Context, start time.Time, reqs []Request) []Response {
	out := make([]Response, len(reqs))
	if len(reqs) == 0 {
		return out
	}
	set := a.set(reqs[0].Kind)
	cmds := make([]command, len(reqs))
	for i, req := range reqs {
		if set == nil || req.Script.SHA == "" {
			for j := range out {
				out[j] = Response{Outcome: OutcomeUnknown} // a library defect: nothing sent
			}
			return out
		}
		cmds[i] = command{label: req.Script.Name, args: evalshaArgs(req)}
	}
	dep := start.Add(a.d)
	cctx, cancel := context.WithDeadlineCause(ctx, dep, errDependencyDeadline)
	defer cancel()
	xs := a.exchange(cctx, set, cmds)
	for i, req := range reqs {
		lim := a.limits
		lim.ReceiptExpiryUs = req.ReceiptExpiryUs
		out[i] = a.settle(ctx, dep, req.Script.Name, xs[i], lim)
	}
	return out
}

// Do sends one non-script command (SCAN, PING) within min(ctx deadline, start + D), start
// being now or the call start ctx carries (withCallStart), and returns its raw reply. The
// Response classifies it like a script's, with no reload and no decoding; Outcome is OK when
// the reply is not an error reply.
func (a *StoreAdapter) Do(ctx context.Context, kind Kind, args ...string) (resp.Value, Response) {
	return a.do(ctx, callStart(ctx, time.Now()), kind, args)
}

// callStartKey is the context key of the call start withCallStart sets.
type callStartKey struct{}

// withCallStart returns ctx carrying start, when the call that the commands sent with it
// continue began: CallBatch and Do then end them at that call's deadline, min(ctx deadline,
// start + D), instead of a deadline of their own, since one deadline covers the whole call, a
// NOSCRIPT reload and retry included (§A5.4, API-22). CleanupEntries sends a step's SCRIPT
// LOAD and resend with it. ctx stays the host's own context, so a command that sent nothing
// is classified as in any call.
func withCallStart(ctx context.Context, start time.Time) context.Context {
	return context.WithValue(ctx, callStartKey{}, start)
}

// callStart returns the call start ctx carries (withCallStart), else now.
func callStart(ctx context.Context, now time.Time) time.Time {
	if s, ok := ctx.Value(callStartKey{}).(time.Time); ok {
		return s
	}
	return now
}

func (a *StoreAdapter) do(ctx context.Context, start time.Time, kind Kind, args []string) (resp.Value, Response) {
	set := a.set(kind)
	if set == nil || len(args) == 0 {
		return resp.Value{}, Response{Outcome: OutcomeUnknown} // a library defect: nothing sent
	}
	dep := start.Add(a.d)
	cctx, cancel := context.WithDeadlineCause(ctx, dep, errDependencyDeadline)
	defer cancel()
	x := a.exchange(cctx, set, []command{{label: commandLabel(args), args: args}})[0]
	r, ok := classify(ctx, dep, "", x)
	if ok {
		r.Outcome = OK
	}
	return x.v, r
}

// ReadyConns returns the number of ready connections of kind.
func (a *StoreAdapter) ReadyConns(kind Kind) int {
	if s := a.set(kind); s != nil {
		return s.readyCount()
	}
	return 0
}

// Close shuts every set (waiting picks return Unavailable, handshakes end), closes the
// ready connections (their calls in flight become OutcomeUnknown), ends the lifetime and
// waits for every connection goroutine. It is safe to call more than once.
func (a *StoreAdapter) Close() {
	a.closeOnce.Do(func() {
		var open []*pipeConn
		for _, s := range a.sets {
			open = append(open, s.shutdown()...)
		}
		a.stop()
		for _, pc := range open {
			pc.close()
		}
	})
	a.wg.Wait()
}

// command is one command to send: its observer label and its words.
type command struct {
	label string
	args  []string
}

// exchanged is what happened to one command of an exchange: sent says at least one byte
// left; v is the reply when err is nil.
type exchanged struct {
	sent bool
	v    resp.Value
	err  error
}

// exchange writes cmds as one pipeline on one ready connection of set and waits for their
// replies until ctx ends (DESIGN §A5.4). A command that never left may move to another
// connection: when the picked one closes before the write starts, or while a write that has
// sent none of its bytes is under way, exchange picks again within the same deadline, with
// fresh call records (close answers the ones in its FIFO with errConnLost). Nothing was sent,
// so nothing is resent (AGENTS.md §6). One that started writing never moves.
func (a *StoreAdapter) exchange(ctx context.Context, set *connSet, cmds []command) []exchanged {
	out := make([]exchanged, len(cmds))
	var buf []byte
	calls := make([]*call, len(cmds))
	for i, c := range cmds {
		start := len(buf)
		buf = resp.AppendCommand(buf, c.args...)
		calls[i] = &call{label: c.label, start: start, end: len(buf), res: make(chan result, 1)}
	}
	k := int64(len(calls))
	for {
		pc, err := set.pick(ctx, len(calls))
		if err != nil {
			return notSentAll(out, err)
		}
		if err := pc.lockWrite(ctx); err != nil {
			pc.load.Add(-k)
			if errors.Is(err, errConnLost) {
				continue
			}
			return notSentAll(out, err)
		}
		if ctx.Err() != nil { // never start a write after the deadline
			pc.unlockWrite()
			pc.load.Add(-k)
			return notSentAll(out, context.Cause(ctx))
		}
		n, closed, werr := pc.write(ctx, calls, buf)
		pc.unlockWrite()
		if closed {
			pc.load.Add(-k)
			continue
		}
		if werr != nil && n == 0 && pc.isClosed() {
			// The connection closed after the calls joined its FIFO and before any byte left:
			// close took them out, with their load, and answered them with errConnLost.
			calls = renewed(calls)
			continue
		}
		unsent := werr
		if werr != nil && ctx.Err() != nil {
			unsent = context.Cause(ctx) // the call had ended: its cause decides, as before a write
		}
		for i, c := range calls {
			out[i].sent = werr == nil || c.start < n
			if !out[i].sent {
				out[i].err = unsent
			}
		}
		for i, c := range calls {
			if out[i].sent {
				out[i].v, out[i].err = await(ctx, c)
			}
		}
		return out
	}
}

// renewed returns fresh records of calls, with the same labels and byte ranges, for a write
// on another connection.
func renewed(calls []*call) []*call {
	out := make([]*call, len(calls))
	for i, c := range calls {
		out[i] = &call{label: c.label, start: c.start, end: c.end, res: make(chan result, 1)}
	}
	return out
}

func notSentAll(out []exchanged, err error) []exchanged {
	for i := range out {
		out[i] = exchanged{err: err}
	}
	return out
}

// settle turns a script command's exchange into its Response, decoding a reply with lim;
// dep is the call's dependency deadline, start + D.
func (a *StoreAdapter) settle(host context.Context, dep time.Time, script string, x exchanged, lim Limits) Response {
	r, ok := classify(host, dep, script, x)
	if !ok {
		return r
	}
	rep, err := Decode(script, x.v, lim)
	if err != nil {
		r.Raw = x.v.String() // outcome_unknown, with the raw reply for Diag
		return r
	}
	r.Outcome, r.Reply = OK, rep
	return r
}

// classify settles a command that was not sent, sent without a reply, or answered with an
// error reply (the §A5.4 diagram). ok reports that x holds a reply value, still to check. A
// command not sent because the kind fails fast after the store refused its last dial with an
// error reply keeps that reply's text as StoreError (plan S9 step 1).
func classify(host context.Context, dep time.Time, script string, x exchanged) (r Response, ok bool) {
	if !x.sent {
		r = Response{Outcome: notSent(host, dep, x.err)}
		var re *refusedError
		if errors.As(x.err, &re) {
			r.StoreError = re.reply
		}
		return r, false
	}
	r = Response{Outcome: OutcomeUnknown, Sent: true, SentCommands: 1}
	switch {
	case x.err != nil: // deadline after sending, or the connection closed
		return r, false
	case x.v.Kind == resp.Error:
		out, reload := classifyErrorReply(script, x.v.Str)
		r.Outcome, r.NoScript, r.StoreError = out, reload, x.v.Str
		if out == Unavailable { // LOADING or BUSY: refused before running, so not sent
			r.Sent, r.SentCommands = false, 0
		}
		return r, false
	}
	return r, true
}

// notSent is the outcome of a command that never left (SPEC API-1, API-22; DESIGN §A5.4):
// cancelled only when the host's own cancellation or deadline ended the call before its
// dependency deadline dep (start + D) did; else unavailable: dep came first, the adapter is
// closed, no connection was usable, or nothing was written while the host's ctx was live.
// err is the call context's cause when that context had ended. The goroutines that end
// the host's context and the call's run in either order, so a host deadline is compared
// with dep instead of trusting which end arrived first. The host's state is read once.
func notSent(host context.Context, dep time.Time, err error) Outcome {
	hd, hasDeadline := host.Deadline()
	hostErr := host.Err()
	switch {
	case errors.Is(err, errDependencyDeadline), errors.Is(err, errAdapterClosed), errors.Is(err, errFailFast):
		return Unavailable
	case hasDeadline && hd.Before(dep) && !time.Now().Before(hd):
		return Cancelled // the host's deadline came first, and has passed
	case hasDeadline && !hd.Before(dep) && errors.Is(hostErr, context.DeadlineExceeded):
		return Unavailable // dep came no later than the host's deadline
	case hostErr != nil:
		return Cancelled // the host cancelled
	}
	return Unavailable
}

// evalshaArgs is EVALSHA sha nkeys keys… args….
func evalshaArgs(r Request) []string {
	args := make([]string, 0, 3+len(r.Keys)+len(r.Args))
	args = append(args, "EVALSHA", r.Script.SHA, strconv.Itoa(len(r.Keys)))
	args = append(args, r.Keys...)
	return append(args, r.Args...)
}

// commandLabel is the send-observer label of a non-script command (§A5.12): its name in
// upper case, with the subcommand for SCRIPT ("SCRIPT LOAD").
func commandLabel(args []string) string {
	name := strings.ToUpper(args[0])
	if name == "SCRIPT" && len(args) > 1 {
		return name + " " + strings.ToUpper(args[1])
	}
	return name
}

// scriptLabel is the rl_store_call_seconds label index of a script rl_lib runs (§A2);
// other scripts have no series.
func scriptLabel(name string) (int, bool) {
	switch name {
	case "acquire":
		return metrics.ScriptAcquire, true
	case "refund":
		return metrics.ScriptRefund, true
	case "due_entries":
		return metrics.ScriptDueEntries, true
	case "cleanup_entry":
		return metrics.ScriptCleanupEntry, true
	case "reclaim":
		return metrics.ScriptReclaim, true
	case "read_epoch":
		return metrics.ScriptReadEpoch, true
	case "hold":
		return metrics.ScriptHold, true
	}
	return 0, false
}
