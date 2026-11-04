package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/srir84/ratelimiter/internal/settings"
	"github.com/srir84/ratelimiter/limiter"
)

// b2Policy, b2Instance and storeAndAPI are the tables of the DESIGN §B2 file.
const b2Policy = `
[policy]
rate_tokens = 10
rate_period_ns = 1_000_000_000
capacity = 20
failure_mode = "closed"
max_tracked_buckets = 100_000
max_live_markers = 50_000
max_markers_per_key = 1_000
receipt_lifetime_ns = 60_000_000_000
refund_deadline_ns = 30_000_000_000
outage_tolerance_ns = 5_000_000_000
cleanup_interval_ns = 5_000_000_000
clock_source = "store"
`

const b2Instance = `
[instance]
max_release_delay_ns = 5_000_000_000
margin_ns = 1_000_000_000
max_held_receipts = 1152
max_pending_refunds = 1152
refund_workers = 128
`

const storeAndAPI = `
[store]
addr = "127.0.0.1:6390"

[api]
max_in_flight = 512
limiter_enabled = true
refuse_after_acquire_bp = 0
db_dsn = "postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64"
`

const b2DSN = "postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64"

// b2File is the §B2 file as Load returns it with RL_LIB_PASSWORD unset: every value from
// the file, and the DESIGN §A5.2 defaults for the absent [instance] keys.
func b2File() File {
	return File{
		Policy: limiter.Policy{
			RateTokens: 10, RatePeriodNs: 1_000_000_000, Capacity: 20, FailureMode: limiter.Closed,
			MaxTrackedBuckets: 100_000, MaxLiveMarkers: 50_000, MaxMarkersPerKey: 1_000,
			ReceiptLifetimeNs: 60_000_000_000, RefundDeadlineNs: 30_000_000_000,
			OutageToleranceNs: 5_000_000_000, CleanupIntervalNs: 5_000_000_000, ClockSource: limiter.Store,
		},
		Instance: limiter.Instance{
			MaxReleaseDelayNs: 5_000_000_000, MarginNs: 1_000_000_000,
			ClockRateBoundPPM: 1000, DependencyDeadlineNs: 2_000_000_000,
			EpochRereadIntervalNs: 1_000_000_000, DrainTimeoutNs: 35_000_000_000,
			MaxHeldReceipts: 1152, MaxPendingRefunds: 1152, ListenerQueueSize: 10_000, RefundWorkers: 128,
			RefundBackoffInitialNs: 50_000_000, RefundBackoffMaxNs: 1_000_000_000,
			StoreConns: 8, RefundConns: 2, CleanupBatch: 1000, CleanupPipelines: 4, ReclaimBatch: 1000,
		},
		Store: limiter.StoreOptions{Addr: "127.0.0.1:6390", User: "rl_lib"},
		API:   API{MaxInFlight: 512, LimiterEnabled: true, RefuseAfterAcquireBP: 0, DBDSN: b2DSN},
	}
}

// writeConfig writes a config file into a fresh temporary directory and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// load writes content and loads it, failing the test on an error.
func load(t *testing.T, content string) File {
	t.Helper()
	f, err := Load(writeConfig(t, content))
	if err != nil {
		t.Fatalf("Load: %v\nfile:\n%s", err, content)
	}
	return f
}

// loadErr writes content and loads it, failing the test unless Load fails; it returns the error text.
func loadErr(t *testing.T, content string) string {
	t.Helper()
	f, err := Load(writeConfig(t, content))
	if err == nil {
		t.Fatalf("Load succeeded (%+v), want an error\nfile:\n%s", f, content)
	}
	return err.Error()
}

func TestConfig_AbsentKeysKeepDefaults(t *testing.T) {
	t.Setenv(LibPasswordEnv, "")
	// The DESIGN §A5.2 instance defaults; the required fields stay 0.
	defaults := limiter.Instance{
		ClockRateBoundPPM: 1000, DependencyDeadlineNs: 2_000_000_000, EpochRereadIntervalNs: 1_000_000_000,
		DrainTimeoutNs: 35_000_000_000, ListenerQueueSize: 10_000, RefundWorkers: 128,
		RefundBackoffInitialNs: 50_000_000, RefundBackoffMaxNs: 1_000_000_000,
		StoreConns: 8, RefundConns: 2, CleanupBatch: 1000, CleanupPipelines: 4, ReclaimBatch: 1000,
	}

	t.Run("empty instance table", func(t *testing.T) {
		f := load(t, b2Policy+"\n[instance]\n"+storeAndAPI)
		if f.Instance != limiter.DefaultInstance(f.Policy) {
			t.Errorf("Instance = %+v, want DefaultInstance(policy) = %+v", f.Instance, limiter.DefaultInstance(f.Policy))
		}
		if f.Instance != defaults {
			t.Errorf("Instance = %+v, want %+v", f.Instance, defaults)
		}
		if f.Instance.DrainTimeoutNs != 35_000_000_000 {
			t.Errorf("DrainTimeoutNs = %d, want 35 s (refund_deadline 30 s + outage_tolerance 5 s)", f.Instance.DrainTimeoutNs)
		}
	})

	t.Run("no instance table", func(t *testing.T) {
		f := load(t, b2Policy+storeAndAPI)
		if f.Instance != defaults {
			t.Errorf("Instance = %+v, want %+v", f.Instance, defaults)
		}
	})

	t.Run("drain default follows the decoded policy", func(t *testing.T) {
		f := load(t, `
[policy]
refund_deadline_ns = 10_000_000_000
outage_tolerance_ns = 2_000_000_000
[instance]
max_release_delay_ns = 4_000_000_000
`+storeAndAPI)
		if f.Instance.DrainTimeoutNs != 12_000_000_000 {
			t.Errorf("DrainTimeoutNs = %d, want 12 s (10 s + 2 s from this file's policy)", f.Instance.DrainTimeoutNs)
		}
		if f.Instance.MaxReleaseDelayNs != 4_000_000_000 {
			t.Errorf("MaxReleaseDelayNs = %d, want 4 s", f.Instance.MaxReleaseDelayNs)
		}
	})

	t.Run("absent policy keys keep DefaultPolicy", func(t *testing.T) {
		f := load(t, `
[policy]
max_tracked_buckets = 100_000
max_live_markers = 50_000
`+storeAndAPI)
		want := limiter.Policy{
			RateTokens: 10, RatePeriodNs: 1_000_000_000, Capacity: 20, FailureMode: limiter.Closed,
			MaxTrackedBuckets: 100_000, MaxLiveMarkers: 50_000, MaxMarkersPerKey: 1000,
			ReceiptLifetimeNs: 60_000_000_000, RefundDeadlineNs: 30_000_000_000,
			OutageToleranceNs: 5_000_000_000, CleanupIntervalNs: 5_000_000_000, ClockSource: limiter.Store,
		}
		if f.Policy != want {
			t.Errorf("Policy = %+v, want %+v", f.Policy, want)
		}
	})

	t.Run("zero is a value, not absent", func(t *testing.T) {
		f := load(t, b2Policy+"\n[instance]\ndrain_timeout_ns = 0\nclock_rate_bound_ppm = 0\n"+storeAndAPI)
		if f.Instance.DrainTimeoutNs != 0 || f.Instance.ClockRateBoundPPM != 0 {
			t.Errorf("DrainTimeoutNs = %d, ClockRateBoundPPM = %d, want 0 and 0",
				f.Instance.DrainTimeoutNs, f.Instance.ClockRateBoundPPM)
		}
	})
}

func TestConfig_MergeOverrides(t *testing.T) {
	t.Setenv(LibPasswordEnv, "")
	base, err := os.ReadFile(filepath.Join("..", "..", "harness.toml"))
	if err != nil {
		t.Fatalf("reading the repository's harness.toml: %v", err)
	}
	if got := load(t, string(base)); got != b2File() {
		t.Fatalf("harness.toml loads as %+v, want the §B2 values %+v", got, b2File())
	}

	cases := []struct {
		name      string
		overrides string
		edit      func(*File)
	}{
		{"none", "", func(*File) {}},
		{"failure_mode open wins", "[policy]\nfailure_mode = \"open\"\n", func(f *File) {
			f.Policy.FailureMode = limiter.Open
		}},
		{"one key of each table", `
[policy]
capacity = 5
[instance]
max_held_receipts = 64
[api]
refuse_after_acquire_bp = 250
`, func(f *File) {
			f.Policy.Capacity = 5
			f.Instance.MaxHeldReceipts = 64
			f.API.RefuseAfterAcquireBP = 250
		}},
		{"a key the base lacks is added", "[instance]\ndrain_timeout_ns = 0\n", func(f *File) {
			f.Instance.DrainTimeoutNs = 0
		}},
		{"baseline switch", "[api]\nlimiter_enabled = false\n", func(f *File) {
			f.API.LimiterEnabled = false
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			merged, err := Merge(base, []byte(c.overrides))
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			want := b2File()
			c.edit(&want)
			if got := load(t, string(merged)); got != want {
				t.Errorf("merged config loads as\n%+v\nwant\n%+v\nmerged file:\n%s", got, want, merged)
			}
		})
	}

	bad := []struct{ name, overrides, want string }{
		{"store table", "[store]\naddr = \"127.0.0.1:1\"\n", "store cannot be overridden"},
		{"top-level key", "seed = 1\n", "seed cannot be overridden"},
		{"policy not a table", "policy = 1\n", "policy is not a table"},
		{"unknown key", "[policy]\ncapacty = 5\n", "policy.capacty"},
		{"other spelling of a key", "[policy]\nCapacity = 5\n", "policy.Capacity"},
		{"wrong type", "[api]\nmax_in_flight = \"many\"\n", "max_in_flight"},
		{"bad enum name", "[policy]\nclock_source = \"wall\"\n", "clock_source"},
		{"not TOML", "[policy\n", "overrides"},
	}
	for _, c := range bad {
		t.Run("refuses "+c.name, func(t *testing.T) {
			merged, err := Merge(base, []byte(c.overrides))
			if err == nil {
				t.Fatalf("Merge succeeded:\n%s\nwant an error naming %q", merged, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("Merge error %q does not name %q", err, c.want)
			}
		})
	}
}

func TestConfig_RejectsUnknownKey(t *testing.T) {
	t.Setenv(LibPasswordEnv, "")
	// The store's user and password never come from the file (DESIGN §A2, §B2): with the
	// [store] table last, a key appended to the file lands in it.
	apiThenStore := "\n[api]\nmax_in_flight = 512\nlimiter_enabled = true\nrefuse_after_acquire_bp = 0\ndb_dsn = \"" +
		b2DSN + "\"\n\n[store]\naddr = \"127.0.0.1:6390\"\n"
	cases := []struct{ name, content, key string }{
		{"policy", b2Policy + "capacty = 20\n" + b2Instance + storeAndAPI, "policy.capacty"},
		{"instance", b2Policy + b2Instance + "max_held_reciepts = 1\n" + storeAndAPI, "instance.max_held_reciepts"},
		{"api", b2Policy + b2Instance + storeAndAPI + "max_inflight = 1\n", "api.max_inflight"},
		{"store password", b2Policy + b2Instance + apiThenStore + "password = \"hunter2-secret\"\n", "store.password"},
		{"store user", b2Policy + b2Instance + apiThenStore + "user = \"rl_admin\"\n", "store.user"},
		{"table", b2Policy + b2Instance + storeAndAPI + "\n[extra]\nx = 1\n", "extra"},
		{"top-level key", "name = \"x\"\n" + b2Policy + b2Instance + storeAndAPI, "name"},
		{"nested table", b2Policy + b2Instance + storeAndAPI + "\n[policy.sub]\nx = 1\n", "policy.sub"},
		{"dotted key", b2Policy + "sub.x = 1\n" + b2Instance + storeAndAPI, "policy.sub.x"},
		{"other spelling", b2Policy + "Capacity = 20\n" + b2Instance + storeAndAPI, "policy.Capacity"},
		{"table spelling", strings.Replace(b2Policy, "[policy]", "[Policy]", 1) + b2Instance + storeAndAPI, "Policy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := loadErr(t, c.content)
			if !strings.Contains(msg, "unknown keys") || !strings.Contains(msg, c.key) {
				t.Errorf("Load error %q, want unknown keys naming %q", msg, c.key)
			}
			if strings.Contains(msg, "hunter2-secret") {
				t.Errorf("Load error %q holds the password from the file", msg)
			}
		})
	}
}

func TestConfig_EnumNames(t *testing.T) {
	t.Setenv(LibPasswordEnv, "")
	withPolicy := func(lines string) string {
		return "[policy]\n" + lines + "\n" + b2Instance + storeAndAPI
	}
	good := []struct {
		name, lines string
		mode        limiter.FailureMode
		clock       limiter.ClockSource
	}{
		{"closed", `failure_mode = "closed"`, limiter.Closed, limiter.Store},
		{"open", `failure_mode = "open"`, limiter.Open, limiter.Store},
		{"store", `clock_source = "store"`, limiter.Closed, limiter.Store},
		{"controlled", `clock_source = "controlled"`, limiter.Closed, limiter.Controlled},
		{"both", "failure_mode = \"open\"\nclock_source = \"controlled\"", limiter.Open, limiter.Controlled},
	}
	for _, c := range good {
		t.Run(c.name, func(t *testing.T) {
			f := load(t, withPolicy(c.lines))
			if f.Policy.FailureMode != c.mode || f.Policy.ClockSource != c.clock {
				t.Errorf("FailureMode, ClockSource = %v, %v; want %v, %v",
					f.Policy.FailureMode, f.Policy.ClockSource, c.mode, c.clock)
			}
		})
	}
	bad := []struct{ name, lines, key string }{
		{"capitalized mode", `failure_mode = "Closed"`, "failure_mode"},
		{"empty mode", `failure_mode = ""`, "failure_mode"},
		{"other mode", `failure_mode = "fail_open"`, "failure_mode"},
		{"numeric mode", `failure_mode = 1`, "failure_mode"},
		{"capitalized clock", `clock_source = "Store"`, "clock_source"},
		{"wall clock", `clock_source = "wall"`, "clock_source"},
		{"empty clock", `clock_source = ""`, "clock_source"},
	}
	for _, c := range bad {
		t.Run("refuses "+c.name, func(t *testing.T) {
			if msg := loadErr(t, withPolicy(c.lines)); !strings.Contains(msg, c.key) {
				t.Errorf("Load error %q does not name %q", msg, c.key)
			}
		})
	}
}

// TestConfig_RejectsBadValues: missing [store] and [api] keys, [api] values out of range,
// and values of the wrong TOML type fail Load.
func TestConfig_RejectsBadValues(t *testing.T) {
	t.Setenv(LibPasswordEnv, "")
	full := b2Policy + b2Instance + storeAndAPI
	drop := func(line string) string {
		if !strings.Contains(full, line+"\n") {
			t.Fatalf("test bug: %q is not a line of the file", line)
		}
		return strings.Replace(full, line+"\n", "", 1)
	}
	set := func(line, to string) string {
		if !strings.Contains(full, line+"\n") {
			t.Fatalf("test bug: %q is not a line of the file", line)
		}
		return strings.Replace(full, line+"\n", to+"\n", 1)
	}
	cases := []struct{ name, content, want string }{
		{"no addr", drop(`addr = "127.0.0.1:6390"`), "store.addr"},
		{"no max_in_flight", drop("max_in_flight = 512"), "api.max_in_flight"},
		{"no limiter_enabled", drop("limiter_enabled = true"), "api.limiter_enabled"},
		{"no refuse_after_acquire_bp", drop("refuse_after_acquire_bp = 0"), "api.refuse_after_acquire_bp"},
		{"no db_dsn", drop(`db_dsn = "` + b2DSN + `"`), "api.db_dsn"},
		{"no api table", b2Policy + b2Instance + "\n[store]\naddr = \"127.0.0.1:6390\"\n", "api.max_in_flight"},
		{"empty addr", set(`addr = "127.0.0.1:6390"`, `addr = ""`), "store.addr"},
		{"empty dsn", set(`db_dsn = "`+b2DSN+`"`, `db_dsn = ""`), "api.db_dsn"},
		{"max_in_flight 0", set("max_in_flight = 512", "max_in_flight = 0"), "api.max_in_flight"},
		{"bp above 10,000", set("refuse_after_acquire_bp = 0", "refuse_after_acquire_bp = 10_001"), "refuse_after_acquire_bp"},
		{"bp below 0", set("refuse_after_acquire_bp = 0", "refuse_after_acquire_bp = -1"), "refuse_after_acquire_bp"},
		{"string for a number", set("capacity = 20", `capacity = "20"`), "policy"},
		{"number for a bool", set("limiter_enabled = true", "limiter_enabled = 1"), "api"},
		{"overflowing int", set("max_held_receipts = 1152", "max_held_receipts = 99_999_999_999_999_999_999"), "max_held_receipts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if msg := loadErr(t, c.content); !strings.Contains(msg, c.want) {
				t.Errorf("Load error %q does not name %q", msg, c.want)
			}
		})
	}
	if f := load(t, set("refuse_after_acquire_bp = 0", "refuse_after_acquire_bp = 10_000")); f.API.RefuseAfterAcquireBP != 10_000 {
		t.Errorf("RefuseAfterAcquireBP = %d, want 10000 (every request)", f.API.RefuseAfterAcquireBP)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Error("Load of a file that does not exist succeeded")
	}
}

// TestConfig_StoreCredentials: the user is rl_lib and the password comes only from
// RL_LIB_PASSWORD (DESIGN §A2, §B2).
func TestConfig_StoreCredentials(t *testing.T) {
	t.Setenv(LibPasswordEnv, "pw-from-env")
	f := load(t, b2Policy+b2Instance+storeAndAPI)
	want := limiter.StoreOptions{Addr: "127.0.0.1:6390", User: "rl_lib", Password: "pw-from-env"}
	if f.Store != want {
		t.Errorf("Store = %+v, want %+v", f.Store, want)
	}
}

// TestConfig_EveryPolicyFieldHasAKey: policyKeys mirrors limiter.Policy field by field, so a
// field added to Policy cannot be silently left out of the file (instanceKeys is checked by
// the compiler: it converts to limiter.Instance).
func TestConfig_EveryPolicyFieldHasAKey(t *testing.T) {
	p, k := reflect.TypeOf(limiter.Policy{}), reflect.TypeOf(policyKeys{})
	if p.NumField() != k.NumField() {
		t.Fatalf("limiter.Policy has %d fields, policyKeys %d", p.NumField(), k.NumField())
	}
	for i := range p.NumField() {
		if p.Field(i).Name != k.Field(i).Name {
			t.Errorf("field %d: limiter.Policy has %s, policyKeys %s", i, p.Field(i).Name, k.Field(i).Name)
		}
		if k.Field(i).Tag.Get("toml") == "" {
			t.Errorf("policyKeys.%s has no toml key", k.Field(i).Name)
		}
	}
}

// numericFields returns the int and int64 fields of p and in by name ("Policy.Capacity", …);
// the two enums are left out.
func numericFields(p limiter.Policy, in limiter.Instance) map[string]int64 {
	out := map[string]int64{}
	for prefix, v := range map[string]reflect.Value{"Policy": reflect.ValueOf(p), "Instance": reflect.ValueOf(in)} {
		for i := range v.NumField() {
			switch f := v.Field(i); f.Type() {
			case reflect.TypeFor[int](), reflect.TypeFor[int64]():
				out[prefix+"."+v.Type().Field(i).Name] = f.Int()
			}
		}
	}
	return out
}

// TestConfig_EncodeRoundTrip: Encode writes every key of a File, the enums by their SPEC
// names, and Load reads the result back as the same File; the store password is never
// written, a store user other than rl_lib and an enum outside its set are refused, and so is
// a File that Load would refuse. In the edited File every policy and instance field holds a
// valid value (settings.Validate) that is neither its default nor its §B2 value nor any other
// field's, so a key written with a default or with another field's value fails the round
// trip; the enums also round-trip apart (open with the store clock).
func TestConfig_EncodeRoundTrip(t *testing.T) {
	t.Setenv(LibPasswordEnv, "")
	edited := b2File()
	edited.Policy = limiter.Policy{
		RateTokens: 7, RatePeriodNs: 1_000_000_001_000, Capacity: 13, FailureMode: limiter.Open,
		MaxTrackedBuckets: 90_001, MaxLiveMarkers: 40_003, MaxMarkersPerKey: 900,
		ReceiptLifetimeNs: 70_000_000_000, RefundDeadlineNs: 40_000_000_000,
		OutageToleranceNs: 4_000_000_000, CleanupIntervalNs: 6_000_000_000, ClockSource: limiter.Controlled,
	}
	edited.Instance = limiter.Instance{
		MaxReleaseDelayNs: 3_000_000_000, MarginNs: 1_500_000_000,
		ClockRateBoundPPM: 500, DependencyDeadlineNs: 1_250_000_000,
		EpochRereadIntervalNs: 750_000_000, DrainTimeoutNs: 7_000_000_000,
		MaxHeldReceipts: 1_100, MaxPendingRefunds: 1_200, ListenerQueueSize: 3, RefundWorkers: 60,
		RefundBackoffInitialNs: 25_000_000, RefundBackoffMaxNs: 2_000_000_000,
		StoreConns: 5, RefundConns: 6, CleanupBatch: 400, CleanupPipelines: 11, ReclaimBatch: 300,
	}
	edited.API = API{MaxInFlight: 2, LimiterEnabled: false, RefuseAfterAcquireBP: 10_000, DBDSN: "postgres://x"}
	if err := settings.Validate(edited.Policy, edited.Instance, true); err != nil {
		t.Fatalf("the edited settings are invalid: %v", err)
	}
	vals := numericFields(edited.Policy, edited.Instance)
	if len(vals) != 27 {
		t.Fatalf("%d numeric policy and instance fields, want 27: give a new field an edited value here", len(vals))
	}
	defaults := numericFields(limiter.DefaultPolicy(), limiter.DefaultInstance(edited.Policy))
	b2 := numericFields(b2File().Policy, b2File().Instance)
	owner := map[int64]string{}
	for name, v := range vals {
		if v == defaults[name] || v == b2[name] {
			t.Errorf("edited %s = %d: its default is %d and its §B2 value %d, want neither", name, v, defaults[name], b2[name])
		}
		if other, taken := owner[v]; taken {
			t.Errorf("edited %s and %s are both %d, want every field distinct", name, other, v)
		}
		owner[v] = name
	}
	if p := b2File().Policy; edited.Policy.FailureMode == p.FailureMode || edited.Policy.ClockSource == p.ClockSource {
		t.Error("the edited enums are the §B2 file's (and the defaults)")
	}
	apart := edited
	apart.Policy.ClockSource = limiter.Store // failure_mode open, clock_source store: a swap of the two shows
	for name, f := range map[string]File{"the B2 file": b2File(), "every field edited": edited, "enums apart": apart} {
		t.Run(name, func(t *testing.T) {
			in := f
			in.Store.Password = "secret-password-123" // a loaded File carries it; Encode must drop it
			data, err := Encode(in)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if strings.Contains(string(data), "secret-password-123") || strings.Contains(string(data), "rl_lib") {
				t.Errorf("the encoded file holds the store password or user:\n%s", data)
			}
			if got := load(t, string(data)); !reflect.DeepEqual(got, f) {
				t.Errorf("Load(Encode(f)) = %+v\nwant %+v\nfile:\n%s", got, f, data)
			}
		})
	}
	data, err := Encode(edited)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`failure_mode = "open"`, `clock_source = "controlled"`, "drain_timeout_ns = 7000000000",
		"listener_queue_size = 3", "limiter_enabled = false", "[store]", `addr = "127.0.0.1:6390"`} {
		if !strings.Contains(string(data), line) {
			t.Errorf("the encoded file lacks %q:\n%s", line, data)
		}
	}

	bad := func(edit func(*File)) File {
		f := b2File()
		edit(&f)
		return f
	}
	for name, f := range map[string]File{
		"another store user":    bad(func(f *File) { f.Store.User = "rl_admin" }),
		"failure mode 2":        bad(func(f *File) { f.Policy.FailureMode = 2 }),
		"clock source -1":       bad(func(f *File) { f.Policy.ClockSource = -1 }),
		"no store address":      bad(func(f *File) { f.Store.Addr = "" }),
		"max_in_flight 0":       bad(func(f *File) { f.API.MaxInFlight = 0 }),
		"refuse bp over 10,000": bad(func(f *File) { f.API.RefuseAfterAcquireBP = 10_001 }),
	} {
		if data, err := Encode(f); err == nil {
			t.Errorf("%s: Encode succeeded, want an error:\n%s", name, data)
		}
	}
}

// TestConfig_Parse: Parse decodes a file's contents as Load does, refuses what Load refuses,
// and never reads the password from the environment (IMPLEMENTATION_PLAN H3 step 1: the
// orchestrator checks the merged config before anything starts).
func TestConfig_Parse(t *testing.T) {
	t.Setenv(LibPasswordEnv, "pw-from-env")
	got, err := Parse([]byte(b2Policy + b2Instance + storeAndAPI))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := b2File(); got != want {
		t.Errorf("Parse = %+v, want the §B2 values with no password %+v", got, want)
	}
	if _, err := Parse([]byte(b2Policy + b2Instance + storeAndAPI + "extra = 1\n")); err == nil ||
		!strings.Contains(err.Error(), "api.extra") {
		t.Errorf("Parse of a file with an unknown key: error %v, want one naming api.extra", err)
	}
}
