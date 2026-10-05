package core

import "testing"

func TestRetryAfterNs(t *testing.T) {
	for _, c := range []struct {
		u, period, tokens, want int64
	}{
		{0, 1_000_000_000, 10, 100_000_000},
		{999_999_999, 1_000_000_000, 10, 1},
		{0, 7_000_000_000, 3, 2_333_333_334}, // 2,333,333,333.3… rounds up
		{1, 7_000_000_000, 3, 2_333_333_333}, // 6,999,999,999 / 3 is exact
		{500_000_000, 1_000_000_000, 1, 500_000_000},
		{0, 4_503_599_000_000_000, 2_147_483_647, 2_097_152}, // a 2^52-sized period, most tokens: 2,097,151.7… rounds up
	} {
		if got := RetryAfterNs(c.u, c.period, c.tokens); got != c.want {
			t.Errorf("RetryAfterNs(%d, %d, %d) = %d, want %d", c.u, c.period, c.tokens, got, c.want)
		}
	}
}

func TestDecisionNs(t *testing.T) {
	// TBOUND − 1 = 2^53 − 2^43 − 1 = 8,998,403,161,718,783 µs.
	if got := DecisionNs(8_998_403_161_718_783); got != 8_998_403_161_718_783_000 {
		t.Errorf("DecisionNs(TBOUND − 1) = %d, want 8,998,403,161,718,783,000", got)
	}
	if got := DecisionNs(1_760_000_000_000_000); got != 1_760_000_000_000_000_000 {
		t.Errorf("DecisionNs(1.76e15) = %d", got)
	}
	if got := DecisionNs(0); got != 0 {
		t.Errorf("DecisionNs(0) = %d", got)
	}
}
