package core

import (
	"testing"

	"ratelimiter/internal/metrics"
)

// familyLabels returns the label values of the named family in a fresh registry.
func familyLabels(t *testing.T, name string) []string {
	t.Helper()
	for _, f := range metrics.NewRegistry().Snapshot().Families {
		if f.Name == name {
			out := make([]string, len(f.Series))
			for i, s := range f.Series {
				out[i] = s.LabelValue
			}
			return out
		}
	}
	t.Fatalf("no family %s", name)
	return nil
}

func TestResultKindLabels(t *testing.T) {
	// SPEC names in DESIGN §A5.2 order.
	want := []string{
		"allowed", "degraded", "limited", "validation_error", "clock_error", "busy", "cancelled",
		"config_error", "not_ready", "state_integrity_error", "capacity_error", "unavailable",
		"outcome_unknown", "receipt_error",
	}
	labels := familyLabels(t, "rl_acquire_results_total")
	if len(labels) != 14 {
		t.Fatalf("%d acquire result labels, want 14", len(labels))
	}
	for i, w := range want {
		k := ResultKind(i)
		if k.String() != w {
			t.Errorf("ResultKind(%d) = %q, want %q", i, k.String(), w)
		}
		if labels[i] != k.String() {
			t.Errorf("acquire label %d = %q, but ResultKind(%d) = %q", i, labels[i], i, k.String())
		}
	}
	if ReceiptError != 13 || Allowed != 0 {
		t.Errorf("ResultKind spans %d … %d, want 0 … 13", Allowed, ReceiptError)
	}
}

func TestRefundResultLabels(t *testing.T) {
	want := []string{"applied", "already_applied", "receipt_invalid", "receipt_expired", "unresolved"}
	labels := familyLabels(t, "rl_refund_results_total")
	if len(labels) != len(want) {
		t.Fatalf("%d refund result labels, want %d", len(labels), len(want))
	}
	for i, w := range want {
		r := RefundResult(i)
		if r.String() != w || labels[i] != w {
			t.Errorf("RefundResult(%d) = %q, label %q, want %q", i, r.String(), labels[i], w)
		}
	}
}

func TestEnumNames(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{UnresolvedClosed.String(), "closed"},
		{UnresolvedRetention.String(), "retention"},
		{UnresolvedExpired.String(), "expired"},
		{UnresolvedReason(0).String(), ""},
		{UnresolvedReason(9).String(), "UnresolvedReason(9)"},
		{ResultKind(14).String(), "ResultKind(14)"},
		{RefundResult(-1).String(), "RefundResult(-1)"},
		{Closed.String(), "closed"},
		{Open.String(), "open"},
		{Store.String(), "store"},
		{Controlled.String(), "controlled"},
	} {
		if c.got != c.want {
			t.Errorf("String() = %q, want %q", c.got, c.want)
		}
	}
}
