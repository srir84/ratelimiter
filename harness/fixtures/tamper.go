//go:build rltest

package fixtures

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
)

// TamperConn is the one method Tamper needs from a connection authenticated as rl_fixture
// (plan choice): a *storeproc.Client, or the driver's recording Fixture connection, which
// declares every key a write names for the teardown audit.
type TamperConn interface {
	Do(args ...string) (storeproc.Reply, error)
}

// Tamper is the state tamper tool (DESIGN §B6; HARNESS FLT-7, FLT-11, §9.4; IMPLEMENTATION_PLAN
// S8 step 4): one method per §B6 operation, each making exactly its change to the limiter's
// store data and nothing else. It sends plain commands as rl_fixture, never a script: rl_fixture
// has no inner commands (§A2), so a script that calls redis.call would be refused. Per-key
// operations name the budget key, in the namespace of the epoch the Tamper was made for.
//
// A deletion that finds nothing to delete, or only part of it, is an error: the store changed
// less than the operation says (or nothing), and the test learns so at once. Its own epoch
// cannot be read as rl_fixture (no HGET, §A2), so the caller names it.
type Tamper struct {
	conn TamperConn
	id   string
}

// NewTamper returns a tamper tool that writes through conn, a connection as rl_fixture, in the
// namespace of epoch id (32 lowercase hex characters; the epoch-wide operations need none).
func NewTamper(conn TamperConn, epochID string) *Tamper {
	return &Tamper{conn: conn, id: epochID}
}

// SharedKey names one of the shared keys ChangeSharedKeyType replaces.
type SharedKey int

const (
	// SharedIndex is the epoch's bucket index, rl:e:<id>:ix (a sorted set).
	SharedIndex SharedKey = iota + 1
	// SharedQuarantine is the epoch's quarantine hash, rl:e:<id>:q.
	SharedQuarantine
	// SharedEpoch is the epoch record, rl:epoch (a hash).
	SharedEpoch
)

// TamperedValue is the string ChangeSharedKeyType stores in place of a shared key (plan choice).
const TamperedValue = "tampered"

// CorruptSchema is the schema value CorruptRecord writes: record_wellformed accepts only "1"
// (DESIGN §A4.2), so the record is no longer well-formed (plan choice).
const CorruptSchema = "corrupt"

// bucketFields are the six bucket fields of k:<enc> (DESIGN §A3.2).
func bucketFields() []string { return []string{"u", "t", "g", "s", "x", "mc"} }

// DeleteBucketFields deletes bucket fields of key's bucket hash k:<enc> with HDEL: the given
// ones, each one of u, t, g, s, x and mc, or all six when none is given. Marker fields stay.
// Every field named must exist.
func (tm *Tamper) DeleteBucketFields(key string, fields ...string) error {
	if len(fields) == 0 {
		fields = bucketFields()
	}
	for i, f := range fields {
		if !slices.Contains(bucketFields(), f) || slices.Contains(fields[:i], f) {
			return fmt.Errorf("fixtures: tamper DeleteBucketFields: %q is not a bucket field, or named twice", f)
		}
	}
	k, err := tm.perKey(key, keys.BucketKey)
	if err != nil {
		return err
	}
	return tm.count("DeleteBucketFields", len(fields), append([]string{"HDEL", k}, fields...)...)
}

// DeleteBucket deletes key's bucket hash k:<enc>, its bucket and marker fields (DEL k).
func (tm *Tamper) DeleteBucket(key string) error {
	k, err := tm.perKey(key, keys.BucketKey)
	if err != nil {
		return err
	}
	return tm.count("DeleteBucket", 1, "DEL", k)
}

// DeleteMarkerIndex deletes key's marker index mi:<enc> (DEL mi).
func (tm *Tamper) DeleteMarkerIndex(key string) error {
	k, err := tm.perKey(key, keys.MarkerIndexKey)
	if err != nil {
		return err
	}
	return tm.count("DeleteMarkerIndex", 1, "DEL", k)
}

// DeleteMarkerIndexMember removes the member <g>:<s> from key's marker index mi:<enc> (ZREM);
// the marker field and mc stay.
func (tm *Tamper) DeleteMarkerIndexMember(key string, g, s int64) error {
	k, err := tm.perKey(key, keys.MarkerIndexKey)
	if err != nil {
		return err
	}
	return tm.count("DeleteMarkerIndexMember", 1, "ZREM", k, markerID(g, s))
}

// DeleteIndexMember removes key's member from the bucket index ix (ZREM ix enc).
func (tm *Tamper) DeleteIndexMember(key string) error {
	enc, err := keys.Encode(key)
	if err != nil {
		return fmt.Errorf("fixtures: tamper: %w", err)
	}
	ix, err := tm.shared(keys.IndexKey)
	if err != nil {
		return err
	}
	return tm.count("DeleteIndexMember", 1, "ZREM", ix, enc)
}

// DeleteMarkerField deletes the marker field m:<g>:<s> of key's bucket hash k:<enc> (HDEL);
// mc and the marker index entry stay.
func (tm *Tamper) DeleteMarkerField(key string, g, s int64) error {
	k, err := tm.perKey(key, keys.BucketKey)
	if err != nil {
		return err
	}
	return tm.count("DeleteMarkerField", 1, "HDEL", k, "m:"+markerID(g, s))
}

// CorruptField writes value into one field of storeKey, which must be rl:epoch or a key of the
// Tamper's epoch namespace (its ix, q, k:<enc> or mi:<enc>, named as the keys package names
// them). For a hash (rl:epoch, q, k:<enc>) field is the hash field (HSET storeKey field value);
// for a sorted set (ix, mi:<enc>) field is the member and value its score (ZADD storeKey value
// field), which the store takes only as a float ("1.5", "inf", "-1").
func (tm *Tamper) CorruptField(storeKey, field, value string) error {
	cmd := "HSET"
	switch {
	case storeKey == keys.EpochKey:
	case tm.id == "" || !strings.HasPrefix(storeKey, "rl:e:"+tm.id+":"):
		return fmt.Errorf("fixtures: tamper CorruptField: %q is neither rl:epoch nor a key of epoch %q", storeKey, tm.id)
	default:
		switch rest := strings.TrimPrefix(storeKey, "rl:e:"+tm.id+":"); {
		case rest == "q", strings.HasPrefix(rest, "k:"):
		case rest == "ix", strings.HasPrefix(rest, "mi:"):
			cmd = "ZADD"
		default:
			return fmt.Errorf("fixtures: tamper CorruptField: %q is no key of the §A3.2 layout", storeKey)
		}
	}
	args := []string{cmd, storeKey, field, value}
	if cmd == "ZADD" {
		args = []string{cmd, storeKey, value, field}
	}
	return tm.added("CorruptField", args...)
}

// AddMalformedIndexMember adds member to the bucket index ix with score (ZADD ix score member).
// member must fail is_enc (DESIGN §A3.2), so no key can claim it: cleanup sets the integrity
// hold unattributable_entry when it reaches it (STA-9).
func (tm *Tamper) AddMalformedIndexMember(member string, score int64) error {
	if keys.IsEnc(member) {
		return fmt.Errorf("fixtures: tamper AddMalformedIndexMember: %q is a valid encoding", member)
	}
	ix, err := tm.shared(keys.IndexKey)
	if err != nil {
		return err
	}
	return tm.added("AddMalformedIndexMember", "ZADD", ix, strconv.FormatInt(score, 10), member)
}

// ChangeSharedKeyType replaces a shared key with the string TamperedValue: DEL, then SET
// (IMPLEMENTATION_PLAN S8 step 4). The key need not exist before (q exists only once a key is
// quarantined).
func (tm *Tamper) ChangeSharedKeyType(which SharedKey) error {
	var k string
	var err error
	switch which {
	case SharedIndex:
		k, err = tm.shared(keys.IndexKey)
	case SharedQuarantine:
		k, err = tm.shared(keys.QuarantineKey)
	case SharedEpoch:
		k = keys.EpochKey
	default:
		err = fmt.Errorf("fixtures: tamper ChangeSharedKeyType: unknown shared key %d", which)
	}
	if err != nil {
		return err
	}
	if r, err := tm.do("ChangeSharedKeyType", "DEL", k); err != nil {
		return err
	} else if r.Kind != ':' || (r.Int != 0 && r.Int != 1) {
		return fmt.Errorf("fixtures: tamper ChangeSharedKeyType: DEL replied %s", r)
	}
	return tm.ok("ChangeSharedKeyType", "SET", k, TamperedValue)
}

// DeleteHwm deletes the high-water mark, rl:epoch's hwm field (HDEL), as FLT-11 does.
func (tm *Tamper) DeleteHwm() error {
	return tm.count("DeleteHwm", 1, "HDEL", keys.EpochKey, "hwm")
}

// CorruptRecord makes the epoch record malformed: its schema field becomes CorruptSchema
// (HSET), which record_wellformed refuses (DESIGN §A4.2). The record must exist as a hash
// (TYPE, read first, so nothing is written otherwise).
func (tm *Tamper) CorruptRecord() error {
	return tm.corruptRecord("CorruptRecord", "schema", CorruptSchema)
}

// CorruptRecordField writes value into the epoch record's field (HSET rl:epoch field value),
// as faultctl's corrupt-record --field does (IMPLEMENTATION_PLAN H4 step 3): with a value the
// field's grammar refuses, such as TamperedValue for any field of DESIGN §A3.2, the record is
// no longer well-formed (hwm: hwm_malformed). The record must exist as a hash (TYPE, read
// first, so nothing is written otherwise).
func (tm *Tamper) CorruptRecordField(field, value string) error {
	if field == "" {
		return errors.New("fixtures: tamper CorruptRecordField: no field")
	}
	return tm.corruptRecord("CorruptRecordField", field, value)
}

// corruptRecord checks that rl:epoch is a hash (TYPE), then writes value into its field.
func (tm *Tamper) corruptRecord(op, field, value string) error {
	r, err := tm.do(op, "TYPE", keys.EpochKey)
	if err != nil {
		return err
	}
	if r.Kind != '+' || r.Str != "hash" {
		return fmt.Errorf("fixtures: tamper %s: rl:epoch is %s, want a hash", op, r)
	}
	return tm.added(op, "HSET", keys.EpochKey, field, value)
}

// Wipe deletes every key (FLUSHALL), as a lost store would, rl:ctl:time included (FLT-7).
func (tm *Tamper) Wipe() error {
	return tm.ok("Wipe", "FLUSHALL")
}

// markerID is a marker's <g>:<s> (DESIGN §A3.2).
func markerID(g, s int64) string {
	return strconv.FormatInt(g, 10) + ":" + strconv.FormatInt(s, 10)
}

// perKey names key's per-key store key through name (keys.BucketKey or keys.MarkerIndexKey).
func (tm *Tamper) perKey(key string, name func(id, enc string) string) (string, error) {
	enc, err := keys.Encode(key)
	if err != nil {
		return "", fmt.Errorf("fixtures: tamper: %w", err)
	}
	if tm.id == "" {
		return "", errNoEpoch
	}
	return name(tm.id, enc), nil
}

// shared names a shared key of the Tamper's epoch through name (keys.IndexKey or
// keys.QuarantineKey).
func (tm *Tamper) shared(name func(id string) string) (string, error) {
	if tm.id == "" {
		return "", errNoEpoch
	}
	return name(tm.id), nil
}

// errNoEpoch refuses a namespace operation of a Tamper made without an epoch ID.
var errNoEpoch = errors.New("fixtures: tamper: no epoch ID for a key of the epoch namespace")

// do sends one command; an error reply is an error.
func (tm *Tamper) do(op string, args ...string) (storeproc.Reply, error) {
	r, err := tm.conn.Do(args...)
	if err != nil {
		return r, fmt.Errorf("fixtures: tamper %s: %s: %w", op, args[0], err)
	}
	if r.Kind == '-' {
		return r, fmt.Errorf("fixtures: tamper %s: %s replied %s", op, args[0], r.Str)
	}
	return r, nil
}

// count sends a deletion that must remove exactly want items.
func (tm *Tamper) count(op string, want int, args ...string) error {
	r, err := tm.do(op, args...)
	if err != nil {
		return err
	}
	if r.Kind != ':' || r.Int != int64(want) {
		return fmt.Errorf("fixtures: tamper %s: %s removed %s, want %d", op, args[0], r, want)
	}
	return nil
}

// added sends an HSET or ZADD of one field or member: it replies 1 when it added it, 0 when
// it overwrote it.
func (tm *Tamper) added(op string, args ...string) error {
	r, err := tm.do(op, args...)
	if err != nil {
		return err
	}
	if r.Kind != ':' || (r.Int != 0 && r.Int != 1) {
		return fmt.Errorf("fixtures: tamper %s: %s replied %s", op, args[0], r)
	}
	return nil
}

// ok sends a command that replies +OK.
func (tm *Tamper) ok(op string, args ...string) error {
	r, err := tm.do(op, args...)
	if err != nil {
		return err
	}
	if r.Kind != '+' || r.Str != "OK" {
		return fmt.Errorf("fixtures: tamper %s: %s replied %s", op, args[0], r)
	}
	return nil
}
