//go:build scripttest

package scripts_test

import (
	"testing"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/keys"
	"ratelimiter/internal/scripts"
	"ratelimiter/internal/store"
	"ratelimiter/internal/testutil/scriptrun"
)

// reclaimReq is a reclaim call (§A4.1): KEYS rl:epoch, then the candidates; no ARGV.
func reclaimReq(candidates ...string) store.Request {
	return store.Request{
		Kind:   store.Maintenance,
		Script: scripts.Reclaim,
		Keys:   append([]string{keys.EpochKey}, candidates...),
	}
}

// TestReclaimScript (plan S7, L2; DESIGN §A4.7; SPEC ADM-1): with a well-formed record (also
// under a hold) reclaim unlinks every candidate outside the current epoch's namespace, of two
// old namespaces, and keeps the current namespace's keys, though they are candidates too; a
// candidate already gone counts 0, so the reply {40, 0, "", 5} counts the five keys unlinked.
// Candidates with no rl:e:<id>: prefix at all (rl:epoch, rl:ctl:time, a filler key under
// rl:fill:) are kept and not counted. With the record missing, not well-formed or of the wrong
// type it unlinks nothing and replies {40, 0, "", 0}. Nothing else is written.
func TestReclaimScript(t *testing.T) {
	p := testPolicy()
	fp := fingerprint.Of(p)
	base := healthy(fp)
	old := []string{
		keys.BucketKey(otherID, encA), keys.MarkerIndexKey(otherID, encA), keys.IndexKey(otherID),
		keys.QuarantineKey(otherID), keys.BucketKey(newID, encB),
	}
	gone := keys.BucketKey(otherID, encC) // an old-namespace candidate that no longer exists
	filler := scriptrun.FillerPrefix + "reclaim"
	cases := []struct {
		name    string
		record  rec  // nil: no record
		str     bool // rl:epoch is a string instead
		damaged bool
		extra   []string // more candidates, none in an rl:e:<id>: namespace
		want    int      // keys unlinked
	}{
		{name: "current record", record: base, want: len(old)},
		{name: "record under a hold", record: base.with("hold", "store_rollback"), want: len(old)},
		{name: "candidates outside every namespace", record: base, extra: []string{keys.EpochKey, keys.CtlTimeKey, filler}, want: len(old)},
		{name: "record missing", want: 0},
		{name: "schema 2", record: base.with("schema", "2"), damaged: true, want: 0},
		{name: "id upper case", record: base.with("id", "9C56CC51B374C3BA189210D5B6D4BF57"), damaged: true, want: 0},
		{name: "rl:epoch a string", str: true, damaged: true, want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, p)
			switch {
			case c.str:
				e.do("SET", keys.EpochKey, "x")
			case c.record != nil:
				e.record(c.record)
			}
			if c.damaged {
				e.h.Damaged(keys.EpochKey)
			}
			e.clock(now0)
			e.bucketA(encA) // the current namespace: encA's bucket and ix
			e.do("HSET", old[0], "u", "1")
			e.do("ZADD", old[1], "5", "7:1")
			e.do("ZADD", old[2], "5", encA)
			e.do("HSET", old[3], encA, "malformed_bucket|5")
			e.do("HSET", old[4], "u", "2")
			e.do("SET", filler, "x")
			before := e.snap()
			cands := append([]string{kKey(encA)}, old...)
			cands = append(cands, gone, ixKey)
			cands = append(cands, c.extra...)
			checkReply(t, c.name, e.run(e.la, reclaimReq(cands...)), rep(40, 0, "", c.want))
			want := cloneState(before)
			if c.want > 0 {
				for _, k := range old {
					delete(want, k)
				}
			}
			checkState(t, c.name, e.snap(), want)
		})
	}
}
