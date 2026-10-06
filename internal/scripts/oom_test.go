//go:build scripttest

package scripts_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"ratelimiter/internal/keys"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/scriptrun"
	"ratelimiter/internal/testutil/storeproc"
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
