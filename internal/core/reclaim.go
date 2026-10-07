package core

import (
	"context"
	"strings"

	"ratelimiter/internal/metrics"
	"ratelimiter/internal/store"
)

// reclaimScans is the most SCAN calls the reclaimer makes after one completed pass (DESIGN
// §A4.7).
const reclaimScans = 10

// reclaim runs after every completed cleanup pass, and only then (DESIGN §A4.7; SPEC ADM-1):
// it continues the instance's SCAN cursor for at most 10 calls, stopping early when a full
// iteration ends (cursor 0), and sends the keys outside the joined epoch's namespace id to
// reclaim.lua in batches of ReclaimBatch, the rest in one last batch, adding the keys each call
// unlinked to rl_reclaim_keys_total. The kept cursor moves past a SCAN reply only once every
// candidate up to it is sent, so a key is never skipped. A failed call ends reclaim for this
// pass and leaves the pass complete (plan choice). reclaim.lua reads rl:epoch again, so a key
// of the current epoch is never unlinked, even when the joined epoch is old. It runs under the
// pass lock, which guards the cursor.
func (l *Limiter) reclaim(ctx context.Context, id string) {
	batch := max(l.in.ReclaimBatch, 1)
	cursor := l.reclaimCursor
	var pending []string // candidates not sent yet, in SCAN order
	for range reclaimScans {
		next, found, r := store.Scan(ctx, l.adapter, cursor)
		if r.Outcome != store.OK {
			return
		}
		pending = append(pending, reclaimCandidates(found, id)...)
		cursor = next
		for len(pending) >= batch {
			if !l.reclaimBatch(ctx, pending[:batch]) {
				return
			}
			pending = pending[batch:]
		}
		if len(pending) == 0 {
			l.reclaimCursor = cursor
		}
		if next == "0" {
			break
		}
	}
	if len(pending) > 0 && l.reclaimBatch(ctx, pending) {
		l.reclaimCursor = cursor
	}
}

// reclaimBatch runs reclaim.lua for one batch and adds the count its OK reply gives (code 40,
// §A4.9) to rl_reclaim_keys_total; false for any other reply or outcome.
func (l *Limiter) reclaimBatch(ctx context.Context, cands []string) bool {
	r := store.Reclaim(ctx, l.adapter, cands)
	if r.StoreError != "" {
		l.setStoreError(r.StoreError)
	}
	n, ok := elemUint(r.Reply.Elems, 3)
	if r.Outcome != store.OK || r.Reply.Code != 40 || len(r.Reply.Elems) != 4 || !ok {
		return false
	}
	if n > 0 {
		l.reg.Add(metrics.ReclaimKeys, 0, uint64(n))
	}
	return true
}

// reclaimCandidates returns the keys whose epoch namespace is not id: the text between rl:e:
// and the next ':' differs from it (§A4.7). A key with no namespace ID is no candidate.
func reclaimCandidates(found []string, id string) []string {
	var out []string
	for _, k := range found {
		if ns, ok := namespaceID(k); ok && ns != id {
			out = append(out, k)
		}
	}
	return out
}

// namespaceID returns the epoch ID of a key rl:e:<id>:…, and false for a key that does not
// start with rl:e: or has no ':' after the ID.
func namespaceID(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, "rl:e:")
	if !ok {
		return "", false
	}
	ns, _, found := strings.Cut(rest, ":")
	if !found {
		return "", false
	}
	return ns, true
}
