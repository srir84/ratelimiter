// Package fingerprint computes the policy fingerprint (DESIGN §A5.9; SPEC CFG-1): a hash
// of every fingerprinted setting plus the store schema version and the time origin. Every
// instance of a deployment must compute the same value, and the epoch record holds it.
// The epoch ID and the signing key are never part of it.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"ratelimiter/internal/settings"
)

// Lines returns one name=value line per fingerprinted setting of CFG-8, named as in the
// harness TOML (§B2), plus store_schema_version=1 and time_origin=unix_us (§A3.1, §A3.2).
// Values are plain decimal or the enum's String(); the lines are byte-sorted and each ends
// in "\n".
func Lines(p settings.Policy) string {
	num := func(v int64) string { return strconv.FormatInt(v, 10) }
	lines := []string{
		"rate_tokens=" + num(p.RateTokens),
		"rate_period_ns=" + num(p.RatePeriodNs),
		"capacity=" + num(p.Capacity),
		"failure_mode=" + p.FailureMode.String(),
		"max_tracked_buckets=" + num(p.MaxTrackedBuckets),
		"max_live_markers=" + num(p.MaxLiveMarkers),
		"max_markers_per_key=" + num(p.MaxMarkersPerKey),
		"receipt_lifetime_ns=" + num(p.ReceiptLifetimeNs),
		"refund_deadline_ns=" + num(p.RefundDeadlineNs),
		"outage_tolerance_ns=" + num(p.OutageToleranceNs),
		"cleanup_interval_ns=" + num(p.CleanupIntervalNs),
		"clock_source=" + p.ClockSource.String(),
		"store_schema_version=1",
		"time_origin=unix_us",
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// Of returns the fingerprint: the first 32 lowercase hex characters of SHA-256(Lines(p)).
func Of(p settings.Policy) string {
	sum := sha256.Sum256([]byte(Lines(p)))
	return hex.EncodeToString(sum[:16])
}
