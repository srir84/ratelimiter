package storestate

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
)

// Type is a key's type as Snapshot reads it: one of the three types of the §A3.2 layout.
type Type int

const (
	// Hash is a hash, read by HGETALL.
	Hash Type = iota + 1
	// ZSet is a sorted set, read by ZRANGE … WITHSCORES.
	ZSet
	// String is a string, read by GET.
	String
)

// String returns the name the store's TYPE command gives the type.
func (t Type) String() string {
	switch t {
	case Hash:
		return "hash"
	case ZSet:
		return "zset"
	case String:
		return "string"
	}
	return "Type(" + strconv.Itoa(int(t)) + ")"
}

// Value is one key's type and content.
type Value struct {
	Type Type
	// Fields maps a hash's fields to their values, or a sorted set's members to their
	// scores as the store formats them ("1760000060000000", "1.5", "inf"). It is nil for a
	// string.
	Fields map[string]string
	// Str is a string's value.
	Str string
}

// State maps the name of every key a snapshot read to its value. Snapshots of an
// unchanged store are equal under reflect.DeepEqual.
type State map[string]Value

// Conn is the one method Snapshot needs from a *storeproc.Client (plan choice), so a test
// can wrap a client and fail its Nth call.
type Conn interface {
	Do(args ...string) (storeproc.Reply, error)
}

// Snapshot reads every rl:* key through conn, a storeproc connection as rl_lib: SCAN 0
// MATCH rl:* COUNT 1000 until the cursor is 0 again, then each key once, in name order,
// by HGETALL, then ZRANGE key 0 -1 WITHSCORES, then GET, keeping the first reply that is
// not a WRONGTYPE error (rl_lib may not call TYPE, DESIGN §A2). A key that SCAN returns
// twice is read once. A key that is gone by the time it is read is left out (plan
// choice): an empty HGETALL or ZRANGE reply, or a nil GET, since the store keeps no empty
// hash or sorted set. Any other error reply, a reply of the wrong shape, a key of another
// type or an I/O error fails the snapshot: it never returns a partial State.
func Snapshot(conn Conn) (State, error) {
	names, err := scan(conn)
	if err != nil {
		return nil, err
	}
	st := make(State, len(names))
	for _, name := range names {
		v, ok, err := read(conn, name)
		if err != nil {
			return nil, err
		}
		if ok {
			st[name] = v
		}
	}
	return st, nil
}

// scan runs SCAN until the cursor returns to 0 and returns the distinct key names, sorted.
func scan(conn Conn) ([]string, error) {
	seen := make(map[string]struct{})
	cursor := "0"
	for {
		rep, err := conn.Do("SCAN", cursor, "MATCH", "rl:*", "COUNT", "1000")
		if err != nil {
			return nil, fmt.Errorf("SCAN %s: %w", cursor, err)
		}
		next, names, err := scanReply(rep)
		if err != nil {
			return nil, fmt.Errorf("SCAN %s: %w", cursor, err)
		}
		for _, n := range names {
			seen[n] = struct{}{}
		}
		if next == 0 {
			return slices.Sorted(maps.Keys(seen)), nil
		}
		cursor = strconv.FormatUint(next, 10)
	}
}

// scanReply checks a SCAN reply, [cursor, [name …]], and returns its cursor and names.
func scanReply(rep storeproc.Reply) (uint64, []string, error) {
	if err := errorReply(rep); err != nil {
		return 0, nil, err
	}
	if rep.Kind != '*' || rep.Nil || len(rep.Arr) != 2 {
		return 0, nil, fmt.Errorf("reply %s, want [cursor, names]", rep)
	}
	cur := rep.Arr[0]
	if cur.Kind != '$' || cur.Nil {
		return 0, nil, fmt.Errorf("cursor %s, want a bulk string", cur)
	}
	next, err := strconv.ParseUint(cur.Str, 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("cursor %q, want a decimal number", cur.Str)
	}
	names, err := bulkStrings(rep.Arr[1])
	if err != nil {
		return 0, nil, err
	}
	return next, names, nil
}

// read reads one key by HGETALL, then ZRANGE … WITHSCORES, then GET, each sent only after
// the one before it replied WRONGTYPE. The bool is false when the key is gone.
func read(conn Conn, name string) (Value, bool, error) {
	rep, err := conn.Do("HGETALL", name)
	if err != nil {
		return Value{}, false, fmt.Errorf("HGETALL %q: %w", name, err)
	}
	if !wrongType(rep) {
		m, err := pairs(rep)
		if err != nil {
			return Value{}, false, fmt.Errorf("HGETALL %q: %w", name, err)
		}
		return Value{Type: Hash, Fields: m}, len(m) > 0, nil
	}
	rep, err = conn.Do("ZRANGE", name, "0", "-1", "WITHSCORES")
	if err != nil {
		return Value{}, false, fmt.Errorf("ZRANGE %q: %w", name, err)
	}
	if !wrongType(rep) {
		m, err := pairs(rep)
		if err != nil {
			return Value{}, false, fmt.Errorf("ZRANGE %q: %w", name, err)
		}
		return Value{Type: ZSet, Fields: m}, len(m) > 0, nil
	}
	rep, err = conn.Do("GET", name)
	if err != nil {
		return Value{}, false, fmt.Errorf("GET %q: %w", name, err)
	}
	if wrongType(rep) {
		return Value{}, false, fmt.Errorf("key %q is not a hash, sorted set or string", name)
	}
	if err := errorReply(rep); err != nil {
		return Value{}, false, fmt.Errorf("GET %q: %w", name, err)
	}
	if rep.Kind != '$' {
		return Value{}, false, fmt.Errorf("GET %q: reply %s, want a bulk string", name, rep)
	}
	if rep.Nil {
		return Value{}, false, nil
	}
	return Value{Type: String, Str: rep.Str}, true, nil
}

// wrongType reports whether rep is the store's WRONGTYPE error reply.
func wrongType(rep storeproc.Reply) bool {
	return rep.Kind == '-' && strings.HasPrefix(rep.Str, "WRONGTYPE")
}

// errorReply returns an error for an error reply, nil for any other reply.
func errorReply(rep storeproc.Reply) error {
	if rep.Kind == '-' {
		return fmt.Errorf("error reply %q", rep.Str)
	}
	return nil
}

// pairs reads an HGETALL or ZRANGE … WITHSCORES reply, a flat array of name, value pairs
// (RESP2), into a map. A name that appears twice is an error.
func pairs(rep storeproc.Reply) (map[string]string, error) {
	if err := errorReply(rep); err != nil {
		return nil, err
	}
	elems, err := bulkStrings(rep)
	if err != nil {
		return nil, err
	}
	if len(elems)%2 != 0 {
		return nil, fmt.Errorf("%d elements, want name, value pairs", len(elems))
	}
	m := make(map[string]string, len(elems)/2)
	for i := 0; i+1 < len(elems); i += 2 {
		if _, dup := m[elems[i]]; dup {
			return nil, fmt.Errorf("%q appears twice", elems[i])
		}
		m[elems[i]] = elems[i+1]
	}
	return m, nil
}

// bulkStrings returns the elements of rep, which must be an array of bulk strings.
func bulkStrings(rep storeproc.Reply) ([]string, error) {
	if rep.Kind != '*' || rep.Nil {
		return nil, fmt.Errorf("reply %s, want an array", rep)
	}
	out := make([]string, len(rep.Arr))
	for i, e := range rep.Arr {
		if e.Kind != '$' || e.Nil {
			return nil, fmt.Errorf("element %d is %s, want a bulk string", i, e)
		}
		out[i] = e.Str
	}
	return out, nil
}
