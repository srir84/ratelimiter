//go:build rltest

package fixtures

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"ratelimiter/harness/runlog"
	"ratelimiter/internal/keys"
)

// faultctl's command grammar (DESIGN §B6; IMPLEMENTATION_PLAN H4 step 3). cmd/faultctl parses
// its command line with ParseFaultCommand, and the orchestrator's fault runner parses a
// scenario's "faultctl …" cmd with it (plan H4 step 7), so both accept the same commands; each
// then runs a command on its own targets: StoreDown, StorePause, KillAPI, Tamper.Apply and
// Clock.Apply.
//
//	store down <duration>          FLT-1 (a): StoreDown
//	store pause <duration>         FLT-2: StorePause
//	api kill <N> [--restart]       FLT-6: KillAPI
//	tamper <op> [flags]            FLT-7, FLT-11: Tamper.Apply (TamperOps, parseTamper)
//	clock set|advance|back <ns>    controlled time: Clock.Apply

// ErrUsage is in ParseFaultCommand's error for a command line it refuses: an unknown command
// or tamper operation, a missing or extra argument, or a value out of range. faultctl exits 2
// for it (plan choice).
const ErrUsage Error = "fixtures: bad faultctl command"

// FaultOp is a faultctl command.
type FaultOp string

// The faultctl commands.
const (
	OpStoreDown    FaultOp = "store down"
	OpStorePause   FaultOp = "store pause"
	OpAPIKill      FaultOp = "api kill"
	OpTamper       FaultOp = "tamper"
	OpClockSet     FaultOp = "clock set"
	OpClockAdvance FaultOp = "clock advance"
	OpClockBack    FaultOp = "clock back"
)

// MaxFaultDuration is the longest store down or store pause ParseFaultCommand takes (plan
// choice): scenarios arm 4 s and 4.5 s (DESIGN §B6), and a cap keeps a typo such as 45m from
// holding the store for most of an hour.
const MaxFaultDuration = 5 * time.Minute

// TamperOp is a tamper operation: one per DESIGN §B6 FLT-7/FLT-11 item (names: plan H4 step 3).
type TamperOp string

// The tamper operations, in DESIGN §B6's order, with the Tamper method each runs.
const (
	TamperDelBucketFields TamperOp = "del-bucket-fields" // delete bucket fields: DeleteBucketFields
	TamperDelK            TamperOp = "del-k"             // delete k: DeleteBucket
	TamperDelMI           TamperOp = "del-mi"            // delete mi: DeleteMarkerIndex
	TamperDelMIMember     TamperOp = "del-mi-member"     // delete one of mi's members: DeleteMarkerIndexMember
	TamperDelIX           TamperOp = "del-ix"            // delete the ix member: DeleteIndexMember
	TamperDelMarker       TamperOp = "del-marker"        // delete a marker field: DeleteMarkerField
	TamperCorruptField    TamperOp = "corrupt-field"     // corrupt a field: CorruptField on k:<enc>
	TamperAddBadIX        TamperOp = "add-bad-ix"        // add a malformed ix member: AddMalformedIndexMember
	TamperRetypeShared    TamperOp = "retype-shared"     // change a shared key's type: ChangeSharedKeyType
	TamperDelHwm          TamperOp = "del-hwm"           // delete hwm: DeleteHwm
	TamperCorruptRecord   TamperOp = "corrupt-record"    // corrupt the record: CorruptRecord(Field)
	TamperWipe            TamperOp = "wipe"              // wipe: Wipe
)

// TamperOps returns the tamper operations in DESIGN §B6's order.
func TamperOps() []TamperOp {
	return []TamperOp{
		TamperDelBucketFields, TamperDelK, TamperDelMI, TamperDelMIMember, TamperDelIX, TamperDelMarker,
		TamperCorruptField, TamperAddBadIX, TamperRetypeShared, TamperDelHwm, TamperCorruptRecord, TamperWipe,
	}
}

// DefaultBadMember is the member add-bad-ix adds without --member (plan choice): it fails
// is_enc, so no key can claim it (DESIGN §A3.2).
const DefaultBadMember = "!bad"

// FaultCommand is one faultctl command line, parsed and checked by ParseFaultCommand.
type FaultCommand struct {
	Op       FaultOp
	Line     string        // the words as given, joined by single spaces: the command's record names it
	Duration time.Duration // store down, store pause: above 0, at most MaxFaultDuration
	Instance int           // api kill: 1 to runlog.MaxInstances
	Restart  bool          // api kill --restart
	Ns       int64         // clock: ns, a multiple of 1,000, at least 0 (DESIGN §B6)
	Tamper   TamperCommand // tamper
}

// TamperCommand is one tamper operation and its arguments. Each operation takes these flags
// (plan choice; any other flag, or a missing one, is ErrUsage), and every operation also takes
// --epoch ID:
//
//	del-bucket-fields --key K [--field u|t|g|s|x|mc]   one bucket field, or all six
//	del-k, del-mi, del-ix --key K
//	del-mi-member, del-marker --key K --member g:s
//	corrupt-field --key K --field f --value v           HSET k:<enc> f v
//	add-bad-ix [--member M] [--value score]             M fails is_enc (DefaultBadMember); score 0
//	retype-shared --target ix|q|epoch
//	del-hwm, wipe
//	corrupt-record [--field f [--value v]]              schema=corrupt; or f=v (TamperedValue)
//
// The operations that change the epoch's namespace (NeedsEpoch) need its ID: rl_fixture cannot
// read the epoch record (no HGET, DESIGN §A2), so faultctl takes it from --epoch, or else from
// the epoch_id a run's instance serves on GET /health, and the orchestrator from its run.
type TamperCommand struct {
	Op      TamperOp
	Key     string    // --key: a per-key operation's budget key (a valid key, API-1)
	G, S    int64     // --member g:s of del-mi-member and del-marker: the marker's generation and sequence
	Member  string    // add-bad-ix's member: --member, or DefaultBadMember
	Field   string    // --field
	Value   string    // --value of corrupt-field and corrupt-record (TamperedValue by default)
	Score   int64     // add-bad-ix's score: --value, 0 by default
	Target  SharedKey // retype-shared's --target
	EpochID string    // --epoch: 32 lowercase hex characters, or "" when not given
}

// NeedsEpoch reports whether the operation changes a key of the epoch's namespace, so the
// Tamper that runs it needs the epoch's ID: every per-key operation, add-bad-ix, and
// retype-shared of ix or q.
func (c TamperCommand) NeedsEpoch() bool {
	switch c.Op {
	case TamperDelBucketFields, TamperDelK, TamperDelMI, TamperDelMIMember, TamperDelIX, TamperDelMarker,
		TamperCorruptField, TamperAddBadIX:
		return true
	case TamperRetypeShared:
		return c.Target != SharedEpoch
	case TamperDelHwm, TamperCorruptRecord, TamperWipe:
	}
	return false
}

// usage returns an ErrUsage error with the reason.
func usage(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUsage, fmt.Sprintf(format, args...))
}

// ParseFaultCommand parses one faultctl command (the words after faultctl's own flags, or
// after "faultctl " in a scenario's cmd) and checks every value; its errors wrap ErrUsage.
func ParseFaultCommand(args []string) (FaultCommand, error) {
	c := FaultCommand{Line: strings.Join(args, " ")}
	if len(args) == 0 {
		return FaultCommand{}, usage("no command: want store, api, tamper or clock")
	}
	var err error
	switch args[0] {
	case "store":
		err = c.parseStore(args[1:])
	case "api":
		err = c.parseAPI(args[1:])
	case "tamper":
		if len(args) < 2 {
			return FaultCommand{}, usage("tamper needs an operation: one of %s", tamperOpList())
		}
		c.Op = OpTamper
		c.Tamper, err = parseTamper(args[1], args[2:])
	case "clock":
		err = c.parseClock(args[1:])
	default:
		err = usage("unknown command %q: want store, api, tamper or clock", args[0])
	}
	if err != nil {
		return FaultCommand{}, err
	}
	return c, nil
}

// parseStore parses "down|pause <duration>".
func (c *FaultCommand) parseStore(args []string) error {
	if len(args) != 2 {
		return usage("want store down <duration> or store pause <duration>")
	}
	switch args[0] {
	case "down":
		c.Op = OpStoreDown
	case "pause":
		c.Op = OpStorePause
	default:
		return usage("unknown store fault %q: want down or pause", args[0])
	}
	d, err := time.ParseDuration(args[1])
	if err != nil || d <= 0 || d > MaxFaultDuration {
		return usage("store %s: duration %q: want a duration above 0 and at most %v, such as 4s", args[0], args[1], MaxFaultDuration)
	}
	c.Duration = d
	return nil
}

// parseAPI parses "kill <N> [--restart]".
func (c *FaultCommand) parseAPI(args []string) error {
	if len(args) < 2 || len(args) > 3 || args[0] != "kill" || (len(args) == 3 && args[2] != "--restart") {
		return usage("want api kill <N> [--restart]")
	}
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 1 || n > runlog.MaxInstances {
		return usage("api kill: instance %q: want 1 to %d", args[1], runlog.MaxInstances)
	}
	c.Op, c.Instance, c.Restart = OpAPIKill, n, len(args) == 3
	return nil
}

// parseClock parses "set|advance|back <ns>": ns, a whole number of µs, at least 0 (§B6).
func (c *FaultCommand) parseClock(args []string) error {
	if len(args) != 2 {
		return usage("want clock set|advance|back <ns>")
	}
	switch args[0] {
	case "set":
		c.Op = OpClockSet
	case "advance":
		c.Op = OpClockAdvance
	case "back":
		c.Op = OpClockBack
	default:
		return usage("unknown clock command %q: want set, advance or back", args[0])
	}
	ns, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return usage("clock %s: %q is not a whole number of ns", args[0], args[1])
	}
	if err := checkNs(ns); err != nil {
		return usage("clock %s %d: %v", args[0], ns, err)
	}
	c.Ns = ns
	return nil
}

// tamperFlags are an operation's flags: those it requires, then those it may take; --epoch is
// open to every operation. ok is false for an unknown operation.
func tamperFlags(op TamperOp) (required, optional []string, ok bool) {
	switch op {
	case TamperDelBucketFields:
		return []string{"key"}, []string{"field"}, true
	case TamperDelK, TamperDelMI, TamperDelIX:
		return []string{"key"}, nil, true
	case TamperDelMIMember, TamperDelMarker:
		return []string{"key", "member"}, nil, true
	case TamperCorruptField:
		return []string{"key", "field", "value"}, nil, true
	case TamperAddBadIX:
		return nil, []string{"member", "value"}, true
	case TamperRetypeShared:
		return []string{"target"}, nil, true
	case TamperCorruptRecord:
		return nil, []string{"field", "value"}, true
	case TamperDelHwm, TamperWipe:
		return nil, nil, true
	}
	return nil, nil, false
}

// tamperOpList names the operations for an error message.
func tamperOpList() string {
	names := make([]string, 0, len(TamperOps()))
	for _, op := range TamperOps() {
		names = append(names, string(op))
	}
	return strings.Join(names, ", ")
}

// parseTamper parses one tamper operation and its flags (TamperCommand), checking each value.
func parseTamper(name string, args []string) (TamperCommand, error) {
	op := TamperOp(name)
	required, optional, ok := tamperFlags(op)
	if !ok {
		return TamperCommand{}, usage("unknown tamper operation %q: want one of %s", name, tamperOpList())
	}
	fs := flag.NewFlagSet("tamper "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var key, member, field, value, target, epoch string
	fs.StringVar(&key, "key", "", "")
	fs.StringVar(&member, "member", "", "")
	fs.StringVar(&field, "field", "", "")
	fs.StringVar(&value, "value", "", "")
	fs.StringVar(&target, "target", "", "")
	fs.StringVar(&epoch, "epoch", "", "")
	if err := fs.Parse(args); err != nil {
		return TamperCommand{}, usage("tamper %s: %v", name, err)
	}
	if fs.NArg() > 0 {
		return TamperCommand{}, usage("tamper %s: unexpected arguments %q", name, fs.Args())
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, f := range required {
		if !set[f] {
			return TamperCommand{}, usage("tamper %s needs --%s", name, f)
		}
	}
	for f := range set {
		if f != "epoch" && !slices.Contains(required, f) && !slices.Contains(optional, f) {
			return TamperCommand{}, usage("tamper %s takes no --%s", name, f)
		}
	}
	c := TamperCommand{Op: op, Key: key, Field: field, Value: value}
	if set["epoch"] {
		if b, err := hex.DecodeString(epoch); err != nil || len(b) != 16 || strings.ToLower(epoch) != epoch {
			return TamperCommand{}, usage("tamper %s: --epoch %q: want 32 lowercase hex characters", name, epoch)
		}
		c.EpochID = epoch
	}
	if err := c.check(set, member, target); err != nil {
		return TamperCommand{}, err
	}
	return c, nil
}

// check checks the values of c's flags (set names those given) and fills the parsed ones.
func (c *TamperCommand) check(set map[string]bool, member, target string) error {
	if set["key"] {
		if _, err := keys.Encode(c.Key); err != nil {
			return usage("tamper %s: --key: %v", c.Op, err)
		}
	}
	switch c.Op {
	case TamperDelBucketFields:
		if set["field"] && !slices.Contains(bucketFields(), c.Field) {
			return usage("tamper %s: --field %q: want one of %s", c.Op, c.Field, strings.Join(bucketFields(), ", "))
		}
	case TamperDelMIMember, TamperDelMarker:
		g, s, err := parseMarkerID(member)
		if err != nil {
			return usage("tamper %s: --member %q: %v", c.Op, member, err)
		}
		c.G, c.S = g, s
	case TamperCorruptField:
		if c.Field == "" {
			return usage("tamper %s: --field must not be empty", c.Op)
		}
	case TamperAddBadIX:
		c.Member = DefaultBadMember
		if set["member"] {
			c.Member = member
		}
		if c.Member == "" || keys.IsEnc(c.Member) {
			return usage("tamper %s: --member %q: want a member that is not a valid encoding (is_enc)", c.Op, c.Member)
		}
		if set["value"] {
			score, err := strconv.ParseInt(c.Value, 10, 64)
			if err != nil {
				return usage("tamper %s: --value %q: want a whole-number score", c.Op, c.Value)
			}
			c.Score = score
		}
		c.Value = ""
	case TamperRetypeShared:
		switch target {
		case "ix":
			c.Target = SharedIndex
		case "q":
			c.Target = SharedQuarantine
		case "epoch":
			c.Target = SharedEpoch
		default:
			return usage("tamper %s: --target %q: want ix, q or epoch", c.Op, target)
		}
	case TamperCorruptRecord:
		switch {
		case set["value"] && !set["field"]:
			return usage("tamper %s: --value needs --field", c.Op)
		case set["field"] && c.Field == "":
			return usage("tamper %s: --field must not be empty", c.Op)
		case set["field"] && !set["value"]:
			c.Value = TamperedValue
		}
	case TamperDelK, TamperDelMI, TamperDelIX, TamperDelHwm, TamperWipe:
	}
	return nil
}

// parseMarkerID parses a marker's <g>:<s> (DESIGN §A3.2): two decimal numbers, each at least
// 0, without a sign.
func parseMarkerID(id string) (g, s int64, err error) {
	gs, ss, ok := strings.Cut(id, ":")
	if !ok {
		return 0, 0, errors.New("want g:s")
	}
	if g, err = parseUint(gs); err != nil {
		return 0, 0, fmt.Errorf("g: %w", err)
	}
	if s, err = parseUint(ss); err != nil {
		return 0, 0, fmt.Errorf("s: %w", err)
	}
	return g, s, nil
}

// parseUint parses a decimal number of at least 0 that fits an int64, without a sign.
func parseUint(s string) (int64, error) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not a decimal number", s)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is out of range", s)
	}
	return n, nil
}

// Apply runs one parsed tamper operation (TamperCommand) through tm, in the namespace of tm's
// epoch; c.EpochID is not read (the caller made tm with it, or with its run's epoch). The
// writes that would succeed on any key read their target's TYPE first and change nothing
// unless it is the live epoch's (plan choice; the deletions show it by what they removed):
// corrupt-field needs k:<enc> to be a hash, and add-bad-ix and retype-shared of ix or q need
// the epoch's bucket index ix to be a sorted set (q may be absent, so ix shows that the
// namespace is live). With a wrong epoch ID they would only add stray keys the limiter never
// reads, and report success.
func (tm *Tamper) Apply(c TamperCommand) error {
	switch c.Op {
	case TamperDelBucketFields:
		if c.Field == "" {
			return tm.DeleteBucketFields(c.Key)
		}
		return tm.DeleteBucketFields(c.Key, c.Field)
	case TamperDelK:
		return tm.DeleteBucket(c.Key)
	case TamperDelMI:
		return tm.DeleteMarkerIndex(c.Key)
	case TamperDelMIMember:
		return tm.DeleteMarkerIndexMember(c.Key, c.G, c.S)
	case TamperDelIX:
		return tm.DeleteIndexMember(c.Key)
	case TamperDelMarker:
		return tm.DeleteMarkerField(c.Key, c.G, c.S)
	case TamperCorruptField:
		k, err := tm.perKey(c.Key, keys.BucketKey)
		if err != nil {
			return err
		}
		if err := tm.wantType("CorruptField", k, "hash"); err != nil {
			return err
		}
		return tm.CorruptField(k, c.Field, c.Value)
	case TamperAddBadIX:
		if err := tm.wantIndex("AddMalformedIndexMember"); err != nil {
			return err
		}
		return tm.AddMalformedIndexMember(c.Member, c.Score)
	case TamperRetypeShared:
		if c.Target != SharedEpoch {
			if err := tm.wantIndex("ChangeSharedKeyType"); err != nil {
				return err
			}
		}
		return tm.ChangeSharedKeyType(c.Target)
	case TamperDelHwm:
		return tm.DeleteHwm()
	case TamperCorruptRecord:
		if c.Field == "" {
			return tm.CorruptRecord()
		}
		return tm.CorruptRecordField(c.Field, c.Value)
	case TamperWipe:
		return tm.Wipe()
	}
	return usage("unknown tamper operation %q", c.Op)
}

// wantType reads storeKey's type (TYPE) for operation op and refuses unless it is want: a key
// of another epoch's namespace, or one never written, is "none".
func (tm *Tamper) wantType(op, storeKey, want string) error {
	r, err := tm.do(op, "TYPE", storeKey)
	if err != nil {
		return err
	}
	if r.Kind != '+' || r.Str != want {
		return fmt.Errorf("fixtures: tamper %s: TYPE %s replied %s, want %s (nothing written: is the epoch ID the live one?)", op, storeKey, r, want)
	}
	return nil
}

// wantIndex refuses operation op unless the epoch's bucket index ix is a sorted set (wantType).
func (tm *Tamper) wantIndex(op string) error {
	ix, err := tm.shared(keys.IndexKey)
	if err != nil {
		return err
	}
	return tm.wantType(op, ix, "zset")
}

// Apply runs a parsed clock command: Set, Advance or Back by ns. Advance and Back step from the
// value this clock wrote or loaded (Load).
func (c *Clock) Apply(op FaultOp, ns int64) error {
	switch op {
	case OpClockSet:
		return c.Set(ns)
	case OpClockAdvance:
		return c.Advance(ns)
	case OpClockBack:
		return c.Back(ns)
	case OpStoreDown, OpStorePause, OpAPIKill, OpTamper:
	}
	return usage("%q is not a clock command", op)
}
