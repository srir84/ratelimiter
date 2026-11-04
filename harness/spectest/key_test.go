//go:build rltest

package spectest

import (
	"reflect"
	"strings"
	"testing"

	"github.com/srir84/ratelimiter/harness/driver"
	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/limiter"
)

// TestT_KEY_1 [D] (SPEC T-KEY-1; API-1, API-7, BUD-1): valid keys (1 and 256 bytes of ASCII,
// 256 bytes of 2-byte UTF-8, ":", "*", a space, non-ASCII, a pair differing in one byte) each
// get 20 allows then limited, unaffected by exhausting the others before them, with distinct
// encodings and their own Inspect record (present 1, s 20, u 0). Invalid keys ("", 257 bytes,
// invalid UTF-8) get validation_error with no acquire sent, and Status and Dump unchanged.
// The op log matches the reference model.
func TestT_KEY_1(t *testing.T) {
	d := driver.New(t, driver.Options{Instances: 2})
	valid := []string{
		"a", strings.Repeat("x", 256), strings.Repeat("é", 128),
		"a:b", "*", "a b", "ключ", "pair-0", "pair-1",
	}
	encs := map[string]string{}
	var ops []driver.Op
	for i, key := range valid {
		enc, err := keys.Encode(key)
		if err != nil {
			t.Fatalf("key %d (%d bytes): %v", i, len(key), err)
		}
		if prev, ok := encs[enc]; ok {
			t.Errorf("keys %q and %q share the encoding %s", prev, key, enc)
		}
		encs[enc] = key
		for n := range 20 {
			op := d.Acquire(n%2, key)
			ops = append(ops, op)
			if op.Kind != limiter.Allowed {
				t.Fatalf("key %d (%d bytes), acquire %d: %v (%+v), want allowed", i, len(key), n+1, op.Kind, op.Diag)
			}
		}
		if op := d.Acquire(i%2, key); op.Kind != limiter.Limited {
			t.Errorf("key %d (%d bytes), acquire 21: %v, want limited", i, len(key), op.Kind)
		}
	}
	doneAll(ops)
	dump := d.Dump()
	for i, key := range valid {
		if in := d.Inspect(key); in.Code != 40 || in.Present != 1 || in.S != 20 || in.U != 0 {
			t.Errorf("key %d (%d bytes): Inspect %+v, want present 1, s 20, u 0", i, len(key), in)
		}
		enc, _ := keys.Encode(key)
		if _, ok := dump[keys.BucketKey(d.ReadEpoch().ID, enc)]; !ok {
			t.Errorf("key %d (%d bytes): no bucket key in the store", i, len(key))
		}
	}

	status := d.Status()
	before := d.Dump()
	for _, key := range []string{"", strings.Repeat("y", 257), "\xff\xfe"} {
		for i := range 2 {
			n := len(d.Sent(i))
			if op := d.Acquire(i, key); op.Kind != limiter.ValidationError || op.Permit != nil {
				t.Errorf("invalid key of %d bytes on instance %d: %v, want validation_error", len(key), i, op.Kind)
			}
			if sent := d.Sent(i)[n:]; driver.Count(sent, "acquire") != 0 {
				t.Errorf("invalid key of %d bytes on instance %d: sent %v, want no acquire", len(key), i, sent)
			}
		}
	}
	if got := d.Status(); got != status {
		t.Errorf("Status after invalid keys %+v, want %+v", got, status)
	}
	if after := d.Dump(); !reflect.DeepEqual(after, before) {
		t.Error("the store changed during the invalid-key acquires")
	}
	d.CheckRefModel(t)
}
