//go:build rltest

package limitertest

import (
	"io"

	"github.com/srir84/ratelimiter/internal/core"
	"github.com/srir84/ratelimiter/internal/receipt"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/limiter"
)

// Adapter is the store adapter interface that store wrappers implement (plan choice).
type Adapter = store.Adapter

// Signer is the receipt signer interface that signer wrappers implement (plan choice).
type Signer = receipt.Signer

// Option configures New.
type Option func(*options)

// options collect what Options set: the hooks, and limiter options passed through.
type options struct {
	hooks core.Hooks
	opts  []limiter.Option
}

// New builds an instance like limiter.New, with the hooks its options set (DESIGN §A5.12).
// Its Health reports fixtures: true. It never returns nil.
func New(p limiter.Policy, in limiter.Instance, st limiter.StoreOptions, opts ...Option) (*limiter.Limiter, error) {
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return core.NewWithHooks(p, in, st, o.hooks, o.opts...)
}

// WithControlledClock allows ClockSource = Controlled.
func WithControlledClock() Option {
	return func(o *options) { o.hooks.ControlledClock = true }
}

// WithStoreWrapper puts f's adapter between the limiter and the store adapter. Store
// wrappers compose (plan choice): a later option's f wraps the adapter the earlier ones
// returned, so it sits nearer the limiter. A nil f adds nothing.
func WithStoreWrapper(f func(Adapter) Adapter) Option {
	return func(o *options) {
		switch prev := o.hooks.StoreWrapper; {
		case f == nil:
		case prev == nil:
			o.hooks.StoreWrapper = f
		default:
			o.hooks.StoreWrapper = func(a Adapter) Adapter { return f(prev(a)) }
		}
	}
}

// WithSignerWrapper puts f's signer between the limiter and its signer. Signer wrappers
// compose like store wrappers: a later option's f wraps the signer the earlier ones returned.
// A nil f adds nothing.
func WithSignerWrapper(f func(Signer) Signer) Option {
	return func(o *options) {
		switch prev := o.hooks.SignerWrapper; {
		case f == nil:
		case prev == nil:
			o.hooks.SignerWrapper = f
		default:
			o.hooks.SignerWrapper = func(s Signer) Signer { return f(prev(s)) }
		}
	}
}

// WithSendObserver has f hear each write of a command: cmd is the script name for EVALSHA
// ("acquire"), else the command ("SCRIPT LOAD", "AUTH", "PING"), and bytesWritten is how
// many of its bytes that write sent. A command has one write, unless it moves to another
// connection after a write that sent none of it: then f hears it for each write, with 0 for
// the first. f runs on the writing goroutine and must not call the limiter. Observers
// compose: every one given hears every write, in option order. A nil f adds nothing.
func WithSendObserver(f func(cmd string, bytesWritten int)) Option {
	return func(o *options) {
		switch prev := o.hooks.SendObserver; {
		case f == nil:
		case prev == nil:
			o.hooks.SendObserver = f
		default:
			o.hooks.SendObserver = func(cmd string, n int) { prev(cmd, n); f(cmd, n) }
		}
	}
}

// WithKeyReader makes r the source of the signing key bytes, so a test can make key
// generation fail (T-CFG-3). A key has one source: the last WithKeyReader given wins.
func WithKeyReader(r io.Reader) Option {
	return func(o *options) { o.hooks.KeyReader = r }
}

// With passes limiter options through (plan choice).
func With(opts ...limiter.Option) Option {
	return func(o *options) { o.opts = append(o.opts, opts...) }
}

// Kill stops l at once, as a process kill would: its goroutines stop and every connection
// closes, with no drain, so pending refunds are lost. Afterwards Acquire returns not_ready and
// Health reports closing.
func Kill(l *limiter.Limiter) { core.Kill(l) }

// ListenerQueueLen returns how many refund events wait in l's listener queue, not yet taken
// by its dispatcher; 0 without a listener. hitapi's test build serves it on /health as
// listener_queue_len (IMPLEMENTATION_PLAN H1 step 9); limiter.Health is unchanged.
func ListenerQueueLen(l *limiter.Limiter) int { return core.ListenerQueueLen(l) }

// PermitEpoch returns the epoch_id (lowercase hex) of an allowed permit's receipt, for the
// driver's op log (plan choice); "" for a degraded or nil permit.
func PermitEpoch(p *limiter.Permit) string { return core.PermitEpoch(p) }
