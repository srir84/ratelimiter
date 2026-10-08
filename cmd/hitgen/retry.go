package main

// Retries after 429 (HARNESS TRF-5; DESIGN §B4 "Retries"; IMPLEMENTATION_PLAN H2 step 5): only
// a 429 is retried, at most max_extra times, with the same user, request ID and instance, once
// the chosen hint plus a seeded jitter has passed since the 429 arrived. Any other status, a
// timeout or a transport error ends the entry's attempts (sender.go).

import (
	"math/rand/v2"
	"strconv"
	"time"
)

// The hint sources gen.jsonl records (§B4 "Record").
const (
	hintRetryAfterMs = "retry_after_ms" // the 429 JSON body's retry_after_ms, in ms
	hintRetryAfter   = "Retry-After"    // the 429's Retry-After header, in whole seconds
)

// maxHintMs is the largest hint taken, in ms (plan choice): about 31 years, so the hint plus
// the largest jitter (60 s) is a valid time.Duration. A larger value counts as no hint.
const maxHintMs = 1_000_000_000_000

// streamJitter is the high bit of every retry jitter stream: the jitter of attempt a of entry
// i comes from rand.NewPCG(seed, streamJitter | i<<2 | a), above the schedule's streams (all
// below 1<<32), so it depends on (i, a) alone and never on the order attempts finish in.
const streamJitter uint64 = 1 << 32

// maxJitterEntry bounds the entry index in a jitter stream, so i<<2 stays below streamJitter.
// A schedule plans far fewer arrivals (harness/config maxPlannedArrivals).
const maxJitterEntry = 1 << 30

// hint is the wait a 429 asked for, and where it came from.
type hint struct {
	ms     int64
	source string // hintRetryAfterMs or hintRetryAfter
}

// chooseHint returns a 429's hint (TRF-5): the larger of retry_after_ms and Retry-After × 1,000
// ms, a tie going to retry_after_ms (plan choice). bodyMs is the body's retry_after_ms (nil when
// absent or not a whole number) and header the Retry-After values. A value that is missing or
// out of range does not count; with neither there is no hint (ok false), and the 429 ends the
// entry's attempts (plan choice: with no hint to wait for, a retry would only hammer the API).
func chooseHint(bodyMs *int64, header []string) (h hint, ok bool) {
	fromBody := bodyMs != nil && *bodyMs >= 0 && *bodyMs <= maxHintMs
	headerMs, fromHeader := parseRetryAfter(header)
	switch {
	case fromBody && (!fromHeader || *bodyMs >= headerMs):
		return hint{ms: *bodyMs, source: hintRetryAfterMs}, true
	case fromHeader:
		return hint{ms: headerMs, source: hintRetryAfter}, true
	default:
		return hint{}, false
	}
}

// parseRetryAfter reads the Retry-After header as whole seconds (HTTP-9; hitapi never sends
// the HTTP-date form) and returns it in ms. It takes exactly one value of 1 to 13 ASCII digits
// whose ms are at most maxHintMs; anything else is no hint.
func parseRetryAfter(values []string) (ms int64, ok bool) {
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 13 {
		return 0, false
	}
	for _, c := range values[0] {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	s, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || s > maxHintMs/1000 {
		return 0, false
	}
	return s * 1000, true
}

// jitterNs returns the jitter before attempt `attempt` (2 or 3) of entry i: uniform in
// [0, jitterMaxMs] ms at ns resolution, from that attempt's own generator (streamJitter). For
// arguments a schedule never has (i outside [0, maxJitterEntry), attempt outside [0, 3],
// jitterMaxMs outside [0, maxHintMs]) it returns 0.
func jitterNs(seed uint64, i, attempt int, jitterMaxMs int64) int64 {
	if i < 0 || i >= maxJitterEntry || attempt < 0 || attempt > 3 || jitterMaxMs < 0 || jitterMaxMs > maxHintMs {
		return 0
	}
	rng := rand.New(rand.NewPCG(seed, streamJitter|uint64(i)<<2|uint64(attempt)))
	return rng.Int64N(jitterMaxMs*int64(time.Millisecond) + 1)
}
