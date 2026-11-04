//go:build scripttest

package scripts_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/srir84/ratelimiter/internal/keys"
	"github.com/srir84/ratelimiter/internal/store"
	"github.com/srir84/ratelimiter/internal/testutil/scriptrun"
	"github.com/srir84/ratelimiter/internal/testutil/storeproc"
)

// TestOOMRefusal (plan S4, L2; DESIGN §A3.0 row 6, §C2 bullet 2): with used memory above
// maxmemory under noeviction, the store refuses acquire (plain #!lua) before it runs:
// capacity_error with an OOM store error, and the store unchanged. The allow-oom scripts
// hold and admin_start_epoch run and write, and the no-writes scripts read_epoch,
// admin_status and admin_inspect run. maxmemory is restored afterwards.
func TestOOMRefusal(t *testing.T) {
	p := testPolicy()
	e := newEnv(t, p)
	e.record(healthy(e.fp).with("nb", "0"))
	e.clock(now0)

	// About 2 MB under the filler prefix the teardown audit accepts (plan choice).
	value := strings.Repeat("f", 10_000)
	for i := range 200 {
		e.do("SET", fmt.Sprintf("%s%03d", scriptrun.FillerPrefix, i), value)
	}
	ops := dial(t, e.h, "rl_ops")
	old := mustDo(t, ops, "CONFIG", "GET", "maxmemory")
	if old.Kind != '*' || len(old.Arr) != 2 {
		t.Fatalf("CONFIG GET maxmemory: %s", old)
	}
	restore := func() {
		if _, err := ops.Do("CONFIG", "SET", "maxmemory", old.Arr[1].Str); err != nil {
			t.Errorf("restoring maxmemory: %v", err)
		}
	}
	t.Cleanup(restore) // before the teardown audit, which Store registered earlier
	used, err := strconv.ParseInt(storeproc.ParseInfo(mustDo(t, ops, "INFO", "memory").Str)["used_memory"], 10, 64)
	if err != nil || used < 2_000_000 {
		t.Fatalf("used_memory %d (%v), want at least the 2 MB filler", used, err)
	}
	mustDo(t, ops, "CONFIG", "SET", "maxmemory", strconv.FormatInt(used/2, 10))

	before := e.snap()
	r := scriptrun.Run(t, e.la, acquireReq(p, e.fp, encA))
	if r.Outcome != store.CapacityError || !strings.HasPrefix(r.StoreError, "OOM") {
		t.Errorf("acquire under OOM: outcome %v, store error %q; want capacity_error with an OOM store error", r.Outcome, r.StoreError)
	}
	e.checkUnchanged("acquire under OOM", before)

	checkReply(t, "read_epoch", e.run(e.la, readEpochReq(qKey)), rep(0, 0, "", epochID, e.fp, hwm0, gen0, now0, 0, 0, 0))
	checkReply(t, "admin_status", e.run(e.aa, statusReq(epochID)), rep(40, 0, "", epochID, e.fp, "", hwm0, 0, 0, 0))
	checkReply(t, "admin_inspect", e.run(e.aa, inspectReq(epochID, encA)), rep(40, 0, "", 0, "", "", "", "", "", "", "", "", 0))
	e.checkUnchanged("the no-writes scripts", before)

	checkReply(t, "hold", e.run(e.la, holdReq(epochID, "store_rollback")), rep(40, 0, "", 0))
	want := cloneState(before)
	setField(want, keys.EpochKey, "hold", "store_rollback")
	checkState(t, "hold", e.snap(), want)

	checkReply(t, "admin_start_epoch", e.run(e.aa, startReq(e.fp, "controlled", "1", epochID)), rep(40, now0, "", newID))
	want[keys.EpochKey] = newRecord(e.fp, "controlled", now0)
	checkState(t, "admin_start_epoch", e.snap(), want)

	restore()
	if got := mustDo(t, ops, "CONFIG", "GET", "maxmemory"); len(got.Arr) != 2 || got.Arr[1].Str != old.Arr[1].Str {
		t.Errorf("maxmemory after the test: %s, want %s", got, old.Arr[1].Str)
	}
}

// TestMaintenanceScriptsUnderOOM (plan S7, L2; DESIGN §A4.8 rule 1, §C2 bullet 2): with used
// memory above maxmemory under noeviction (filler under rl:fill:), the store refuses acquire and
// refund (plain #!lua) before they run: capacity_error with an OOM store error, the store
// unchanged. The allow-oom maintenance scripts run and free memory: due_entries returns the due
// key and stores hwm, cleanup_entry removes its eligible bucket, and reclaim unlinks 100
// old-epoch keys of 10 KB each, so used_memory drops by at least 0.5 MB. maxmemory is restored
// afterwards.
func TestMaintenanceScriptsUnderOOM(t *testing.T) {
	p := testPolicy()
	e := newEnv(t, p)
	e.record(healthy(e.fp))
	e.clock(now0)
	e.fullKey(encA, pastX) // eligible at now0

	value := strings.Repeat("f", 10_000)
	for i := range 200 {
		e.do("SET", fmt.Sprintf("%s%03d", scriptrun.FillerPrefix, i), value)
	}
	old := make([]string, 100) // an old epoch's buckets, 10 KB each
	for i := range old {
		enc, err := keys.Encode(fmt.Sprintf("old-%03d", i))
		if err != nil {
			t.Fatal(err)
		}
		old[i] = keys.BucketKey(otherID, enc)
		e.do("HSET", old[i], "u", value)
	}
	ops := dial(t, e.h, "rl_ops")
	maxmem := mustDo(t, ops, "CONFIG", "GET", "maxmemory")
	if maxmem.Kind != '*' || len(maxmem.Arr) != 2 {
		t.Fatalf("CONFIG GET maxmemory: %s", maxmem)
	}
	restore := func() {
		if _, err := ops.Do("CONFIG", "SET", "maxmemory", maxmem.Arr[1].Str); err != nil {
			t.Errorf("restoring maxmemory: %v", err)
		}
	}
	t.Cleanup(restore) // before the teardown audit, which Store registered earlier
	usedMemory := func() int64 {
		t.Helper()
		used, err := strconv.ParseInt(storeproc.ParseInfo(mustDo(t, ops, "INFO", "memory").Str)["used_memory"], 10, 64)
		if err != nil {
			t.Fatalf("used_memory: %v", err)
		}
		return used
	}
	full := usedMemory()
	if full < 3_000_000 {
		t.Fatalf("used_memory %d, want at least the 3 MB of filler and old-epoch data", full)
	}
	mustDo(t, ops, "CONFIG", "SET", "maxmemory", strconv.FormatInt(full/2, 10))

	before := e.snap()
	for _, req := range []store.Request{acquireReq(p, e.fp, encB), refundReq(p, e.fp, encA, gen0, 2, now0+1_000_000)} {
		r := scriptrun.Run(t, e.la, req)
		if r.Outcome != store.CapacityError || !strings.HasPrefix(r.StoreError, "OOM") {
			t.Errorf("%s under OOM: outcome %v, store error %q; want capacity_error with an OOM store error", req.Script.Name, r.Outcome, r.StoreError)
		}
	}
	e.checkUnchanged("acquire and refund under OOM", before)

	checkReply(t, "due_entries", e.run(e.la, dueReq(e.fp, 10, 0)), rep(0, now0, "", pastX, encA))
	checkReply(t, "cleanup_entry", e.step(encA), rep(30, now0, "", 0))
	checkReply(t, "reclaim", e.run(e.la, reclaimReq(old...)), rep(40, 0, "", len(old)))
	if freed := full - usedMemory(); freed < 500_000 {
		t.Errorf("the maintenance scripts freed %d bytes of used_memory, want at least 500,000", freed)
	}
	want := removedState(t, before, encA, now0)
	for _, k := range old {
		delete(want, k)
	}
	checkState(t, "the maintenance scripts under OOM", e.snap(), want)

	restore()
	if got := mustDo(t, ops, "CONFIG", "GET", "maxmemory"); len(got.Arr) != 2 || got.Arr[1].Str != maxmem.Arr[1].Str {
		t.Errorf("maxmemory after the test: %s, want %s", got, maxmem.Arr[1].Str)
	}
}
