package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"

	"ratelimiter/internal/fingerprint"
	"ratelimiter/internal/settings"
	"ratelimiter/internal/store"
	"ratelimiter/limiter"
)

// policyTable is the [policy] table of the shared config file (DESIGN §B2): every key is a
// fingerprinted setting, named as fingerprint.Lines names it.
type policyTable struct {
	RateTokens        int64  `toml:"rate_tokens"`
	RatePeriodNs      int64  `toml:"rate_period_ns"`
	Capacity          int64  `toml:"capacity"`
	FailureMode       string `toml:"failure_mode"`
	MaxTrackedBuckets int64  `toml:"max_tracked_buckets"`
	MaxLiveMarkers    int64  `toml:"max_live_markers"`
	MaxMarkersPerKey  int64  `toml:"max_markers_per_key"`
	ReceiptLifetimeNs int64  `toml:"receipt_lifetime_ns"`
	RefundDeadlineNs  int64  `toml:"refund_deadline_ns"`
	OutageToleranceNs int64  `toml:"outage_tolerance_ns"`
	CleanupIntervalNs int64  `toml:"cleanup_interval_ns"`
	ClockSource       string `toml:"clock_source"`
}

// policyFile holds only the [policy] table: the file's other tables are not decoded.
type policyFile struct {
	Policy policyTable `toml:"policy"`
}

// isPolicyKey reports whether name is exactly one of the [policy] keys of DESIGN §B2, the
// toml tags of policyTable.
func isPolicyKey(name string) bool {
	switch name {
	case "rate_tokens", "rate_period_ns", "capacity", "failure_mode", "max_tracked_buckets", "max_live_markers",
		"max_markers_per_key", "receipt_lifetime_ns", "refund_deadline_ns", "outage_tolerance_ns",
		"cleanup_interval_ns", "clock_source":
		return true
	}
	return false
}

// policyInfo is a validated policy and what the admin scripts take from it (DESIGN §A6):
// the fingerprint (§A5.9), cap_units and lifetime_us (in Limits).
type policyInfo struct {
	p   limiter.Policy
	fp  string
	lim store.Limits
}

// maxPolicyBytes caps the policy file rladmin reads (plan choice, 1 MiB): a larger file is an
// invalid policy.
const maxPolicyBytes = 1 << 20

// loadPolicy reads and decodes the policy file at path (decodePolicy). Only a regular file is
// read, so loading cannot wait without bound (AGENTS.md §2): a FIFO, a device or any other
// file is refused unopened, and the open (O_NONBLOCK: a FIFO swapped in meanwhile does not
// wait for a writer) is checked again before reading. At most maxPolicyBytes are read.
func loadPolicy(path string) (policyInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return policyInfo{}, fmt.Errorf("reading the policy file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return policyInfo{}, fmt.Errorf("reading the policy file: %s is not a regular file (mode %v)", path, fi.Mode())
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return policyInfo{}, fmt.Errorf("reading the policy file: %w", err)
	}
	defer func() { _ = f.Close() }() // opened read-only: closing cannot change what was read
	if fi, err = f.Stat(); err != nil {
		return policyInfo{}, fmt.Errorf("reading the policy file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return policyInfo{}, fmt.Errorf("reading the policy file: %s is not a regular file (mode %v)", path, fi.Mode())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPolicyBytes+1))
	if err != nil {
		return policyInfo{}, fmt.Errorf("reading the policy file: %w", err)
	}
	if len(data) > maxPolicyBytes {
		return policyInfo{}, fmt.Errorf("reading the policy file: larger than %d bytes", maxPolicyBytes)
	}
	return decodePolicy(data)
}

// decodePolicy decodes the [policy] table of a TOML file onto limiter.DefaultPolicy() (plan
// S8 step 3): absent keys keep their defaults, other tables are ignored, and a key of
// [policy] that is not exactly a §B2 setting name, or the table in another casing, is an
// error. The policy is validated with
// settings.ValidatePolicy(p, true), which accepts clock_source = "controlled" (only
// limiter.New refuses it, §A5.12). Any failure is an invalid policy (exit 2, plan choice).
func decodePolicy(data []byte) (policyInfo, error) {
	def := limiter.DefaultPolicy()
	f := policyFile{Policy: policyTable{
		RateTokens:        def.RateTokens,
		RatePeriodNs:      def.RatePeriodNs,
		Capacity:          def.Capacity,
		FailureMode:       def.FailureMode.String(),
		MaxTrackedBuckets: def.MaxTrackedBuckets,
		MaxLiveMarkers:    def.MaxLiveMarkers,
		MaxMarkersPerKey:  def.MaxMarkersPerKey,
		ReceiptLifetimeNs: def.ReceiptLifetimeNs,
		RefundDeadlineNs:  def.RefundDeadlineNs,
		OutageToleranceNs: def.OutageToleranceNs,
		CleanupIntervalNs: def.CleanupIntervalNs,
		ClockSource:       def.ClockSource.String(),
	}}
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return policyInfo{}, fmt.Errorf("decoding the policy file: %w", err)
	}
	// TOML names are case-sensitive, but BurntSushi/toml matches a field in any casing when no
	// name matches exactly: Capacity would set capacity, [Policy] would be read as [policy],
	// and a key given in two casings would take whichever map order decodes last. So every
	// key of the file is checked by its exact name: any other casing of the policy table, and
	// any key of [policy] that is not exactly a §B2 name, is an unknown key.
	for _, k := range md.Keys() { // every key has at least one element
		switch {
		case k[0] != "policy" && strings.EqualFold(k[0], "policy"):
			return policyInfo{}, fmt.Errorf("decoding the policy file: unknown key %s (TOML names are case-sensitive: the table is [policy])", k)
		case k[0] == "policy" && len(k) > 1 && !isPolicyKey(k[1]):
			return policyInfo{}, fmt.Errorf("decoding the policy file: unknown key %s", k)
		}
	}
	t := f.Policy
	p := limiter.Policy{
		RateTokens:        t.RateTokens,
		RatePeriodNs:      t.RatePeriodNs,
		Capacity:          t.Capacity,
		MaxTrackedBuckets: t.MaxTrackedBuckets,
		MaxLiveMarkers:    t.MaxLiveMarkers,
		MaxMarkersPerKey:  t.MaxMarkersPerKey,
		ReceiptLifetimeNs: t.ReceiptLifetimeNs,
		RefundDeadlineNs:  t.RefundDeadlineNs,
		OutageToleranceNs: t.OutageToleranceNs,
		CleanupIntervalNs: t.CleanupIntervalNs,
	}
	switch t.FailureMode {
	case "closed":
		p.FailureMode = limiter.Closed
	case "open":
		p.FailureMode = limiter.Open
	default:
		return policyInfo{}, fmt.Errorf("policy failure_mode %q: want \"closed\" or \"open\"", t.FailureMode)
	}
	switch t.ClockSource {
	case "store":
		p.ClockSource = limiter.Store
	case "controlled":
		p.ClockSource = limiter.Controlled
	default:
		return policyInfo{}, fmt.Errorf("policy clock_source %q: want \"store\" or \"controlled\"", t.ClockSource)
	}
	if err := settings.ValidatePolicy(p, true); err != nil {
		return policyInfo{}, err
	}
	return policyInfo{p: p, fp: fingerprint.Of(p), lim: store.LimitsFor(p)}, nil
}
