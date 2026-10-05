package core

// DecisionNs converts a budget time t_us (µs since the Unix epoch, from the store) to the
// decision_ns the library reports (§A3.1, API-4, API-6). t_us < 2^53 − 2^43 (§A3.1), so
// the product stays below 2^63.
func DecisionNs(tUs int64) int64 { return tUs * 1000 }

// RetryAfterNs is a limited acquire's exact wait, rounded up to the next whole nanosecond:
// ceil((ratePeriodNs − u) / rateTokens), where u < ratePeriodNs is the bucket's units at
// the decision (one token = ratePeriodNs units, BUD-13, API-6). With validated settings
// (ratePeriodNs < 2^52, 1 ≤ rateTokens < 2^31) nothing overflows.
func RetryAfterNs(u, ratePeriodNs, rateTokens int64) int64 {
	return (ratePeriodNs - u + rateTokens - 1) / rateTokens
}
