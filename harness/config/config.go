package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/srir84/ratelimiter/limiter"
)

// LibUser is the store user every hitapi's limiter connects as (DESIGN §A2).
const LibUser = "rl_lib"

// LibPasswordEnv names the variable that carries LibUser's password (DESIGN §A2, §B2): the
// only source of it, never the file.
const LibPasswordEnv = "RL_LIB_PASSWORD"

// MaxRefuseBP is the largest refuse_after_acquire_bp: every request (§B3, per 10,000).
const MaxRefuseBP = 10_000

// File is the decoded config: what one hitapi needs to build its limiter and serve /hit.
type File struct {
	Policy   limiter.Policy
	Instance limiter.Instance
	Store    limiter.StoreOptions // Addr from [store]; User LibUser; Password from LibPasswordEnv
	API      API
}

// API is the [api] table. Every key is required (plan choice: these settings have no
// library default, and a missing limiter_enabled or max_in_flight would change what a run
// measures).
type API struct {
	MaxInFlight          int    `toml:"max_in_flight"`           // HAR-14; no queue: full -> ERR-7
	LimiterEnabled       bool   `toml:"limiter_enabled"`         // HAR-17 baseline switch
	RefuseAfterAcquireBP int    `toml:"refuse_after_acquire_bp"` // ERR-15 refusals per 10,000 requests (§B3)
	DBDSN                string `toml:"db_dsn"`                  // harness PostgreSQL (pgxpool DSN)
}

// The four tables of the file (§B2). A scenario may override only policy, instance and api.
const (
	tablePolicy   = "policy"
	tableInstance = "instance"
	tableStore    = "store"
	tableAPI      = "api"
)

// rawFile holds each table undecoded, so [instance] can be decoded onto defaults that depend
// on the decoded [policy].
type rawFile struct {
	Policy   toml.Primitive `toml:"policy"`
	Instance toml.Primitive `toml:"instance"`
	Store    toml.Primitive `toml:"store"`
	API      toml.Primitive `toml:"api"`
}

// policyKeys is limiter.Policy with its TOML key names; the two enums use their SPEC names.
type policyKeys struct {
	RateTokens        int64       `toml:"rate_tokens"`
	RatePeriodNs      int64       `toml:"rate_period_ns"`
	Capacity          int64       `toml:"capacity"`
	FailureMode       failureMode `toml:"failure_mode"`
	MaxTrackedBuckets int64       `toml:"max_tracked_buckets"`
	MaxLiveMarkers    int64       `toml:"max_live_markers"`
	MaxMarkersPerKey  int64       `toml:"max_markers_per_key"`
	ReceiptLifetimeNs int64       `toml:"receipt_lifetime_ns"`
	RefundDeadlineNs  int64       `toml:"refund_deadline_ns"`
	OutageToleranceNs int64       `toml:"outage_tolerance_ns"`
	CleanupIntervalNs int64       `toml:"cleanup_interval_ns"`
	ClockSource       clockSource `toml:"clock_source"`
}

func policyKeysOf(p limiter.Policy) policyKeys {
	return policyKeys{
		RateTokens:        p.RateTokens,
		RatePeriodNs:      p.RatePeriodNs,
		Capacity:          p.Capacity,
		FailureMode:       failureMode(p.FailureMode),
		MaxTrackedBuckets: p.MaxTrackedBuckets,
		MaxLiveMarkers:    p.MaxLiveMarkers,
		MaxMarkersPerKey:  p.MaxMarkersPerKey,
		ReceiptLifetimeNs: p.ReceiptLifetimeNs,
		RefundDeadlineNs:  p.RefundDeadlineNs,
		OutageToleranceNs: p.OutageToleranceNs,
		CleanupIntervalNs: p.CleanupIntervalNs,
		ClockSource:       clockSource(p.ClockSource),
	}
}

func (k policyKeys) policy() limiter.Policy {
	return limiter.Policy{
		RateTokens:        k.RateTokens,
		RatePeriodNs:      k.RatePeriodNs,
		Capacity:          k.Capacity,
		FailureMode:       limiter.FailureMode(k.FailureMode),
		MaxTrackedBuckets: k.MaxTrackedBuckets,
		MaxLiveMarkers:    k.MaxLiveMarkers,
		MaxMarkersPerKey:  k.MaxMarkersPerKey,
		ReceiptLifetimeNs: k.ReceiptLifetimeNs,
		RefundDeadlineNs:  k.RefundDeadlineNs,
		OutageToleranceNs: k.OutageToleranceNs,
		CleanupIntervalNs: k.CleanupIntervalNs,
		ClockSource:       limiter.ClockSource(k.ClockSource),
	}
}

// instanceKeys is limiter.Instance with its TOML key names: the same fields in the same
// order, so the two convert into each other and a field added to Instance fails to compile
// here until it gets its key.
type instanceKeys struct {
	MaxReleaseDelayNs      int64 `toml:"max_release_delay_ns"`
	MarginNs               int64 `toml:"margin_ns"`
	ClockRateBoundPPM      int64 `toml:"clock_rate_bound_ppm"`
	DependencyDeadlineNs   int64 `toml:"dependency_deadline_ns"`
	EpochRereadIntervalNs  int64 `toml:"epoch_reread_interval_ns"`
	DrainTimeoutNs         int64 `toml:"drain_timeout_ns"`
	MaxHeldReceipts        int   `toml:"max_held_receipts"`
	MaxPendingRefunds      int   `toml:"max_pending_refunds"`
	ListenerQueueSize      int   `toml:"listener_queue_size"`
	RefundWorkers          int   `toml:"refund_workers"`
	RefundBackoffInitialNs int64 `toml:"refund_backoff_initial_ns"`
	RefundBackoffMaxNs     int64 `toml:"refund_backoff_max_ns"`
	StoreConns             int   `toml:"store_conns"`
	RefundConns            int   `toml:"refund_conns"`
	CleanupBatch           int   `toml:"cleanup_batch"`
	CleanupPipelines       int   `toml:"cleanup_pipelines"`
	ReclaimBatch           int   `toml:"reclaim_batch"`
}

// storeKeys is the [store] table: the address only. The user is LibUser and the password
// comes from LibPasswordEnv, so a user or password key is an unknown key.
type storeKeys struct {
	Addr string `toml:"addr"`
}

// fileKeys is the whole file as Encode writes it: the four tables, every key.
type fileKeys struct {
	Policy   policyKeys   `toml:"policy"`
	Instance instanceKeys `toml:"instance"`
	Store    storeKeys    `toml:"store"`
	API      API          `toml:"api"`
}

// failureMode decodes and encodes failure_mode by its SPEC name (API-20a, API-20b).
type failureMode limiter.FailureMode

func (m failureMode) MarshalText() ([]byte, error) {
	switch limiter.FailureMode(m) {
	case limiter.Closed:
		return []byte("closed"), nil
	case limiter.Open:
		return []byte("open"), nil
	}
	return nil, fmt.Errorf("failure_mode %d: want closed or open", int(m))
}

func (m *failureMode) UnmarshalText(text []byte) error {
	switch string(text) {
	case "closed":
		*m = failureMode(limiter.Closed)
	case "open":
		*m = failureMode(limiter.Open)
	default:
		return fmt.Errorf("failure_mode %q: want \"closed\" or \"open\"", text)
	}
	return nil
}

// clockSource decodes and encodes clock_source by its SPEC name (CLK-2).
type clockSource limiter.ClockSource

func (c clockSource) MarshalText() ([]byte, error) {
	switch limiter.ClockSource(c) {
	case limiter.Store:
		return []byte("store"), nil
	case limiter.Controlled:
		return []byte("controlled"), nil
	}
	return nil, fmt.Errorf("clock_source %d: want store or controlled", int(c))
}

func (c *clockSource) UnmarshalText(text []byte) error {
	switch string(text) {
	case "store":
		*c = clockSource(limiter.Store)
	case "controlled":
		*c = clockSource(limiter.Controlled)
	default:
		return fmt.Errorf("clock_source %q: want \"store\" or \"controlled\"", text)
	}
	return nil
}

// Load reads the config file at path (§B2). The store password comes from
// $RL_LIB_PASSWORD: it is "" when the variable is unset, and a process that connects to the
// store checks it. No error ever holds the password.
func Load(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("reading the harness config: %w", err)
	}
	f, err := decode(data)
	if err != nil {
		return File{}, fmt.Errorf("harness config %s: %w", path, err)
	}
	f.Store.Password = os.Getenv(LibPasswordEnv)
	return f, nil
}

// Parse decodes a config file's contents as Load does, without reading a file or the
// environment, so Store.Password stays "". The orchestrator checks a run's merged config with
// it before anything starts (IMPLEMENTATION_PLAN H3 step 1).
func Parse(data []byte) (File, error) {
	f, err := decode(data)
	if err != nil {
		return File{}, fmt.Errorf("harness config: %w", err)
	}
	return f, nil
}

// decode parses a config file: [policy] onto DefaultPolicy(), then [instance] onto
// DefaultInstance(policy) (the drain default depends on the policy), then [store] and [api].
// It leaves Store.Password "".
func decode(data []byte) (File, error) {
	var raw rawFile
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return File{}, fmt.Errorf("parsing: %w", err)
	}
	pk := policyKeysOf(limiter.DefaultPolicy())
	if err := md.PrimitiveDecode(raw.Policy, &pk); err != nil {
		return File{}, fmt.Errorf("[%s]: %w", tablePolicy, err)
	}
	policy := pk.policy()
	ik := instanceKeys(limiter.DefaultInstance(policy))
	if err := md.PrimitiveDecode(raw.Instance, &ik); err != nil {
		return File{}, fmt.Errorf("[%s]: %w", tableInstance, err)
	}
	var sk storeKeys
	if err := md.PrimitiveDecode(raw.Store, &sk); err != nil {
		return File{}, fmt.Errorf("[%s]: %w", tableStore, err)
	}
	var api API
	if err := md.PrimitiveDecode(raw.API, &api); err != nil {
		return File{}, fmt.Errorf("[%s]: %w", tableAPI, err)
	}
	if err := checkKeys(md); err != nil {
		return File{}, err
	}
	if err := checkRequired(md, sk, api); err != nil {
		return File{}, err
	}
	return File{
		Policy:   policy,
		Instance: limiter.Instance(ik),
		Store:    limiter.StoreOptions{Addr: sk.Addr, User: LibUser},
		API:      api,
	}, nil
}

// checkKeys fails on every key that is not one of the file's keys, spelled exactly: the
// decoder leaves unknown keys undecoded, and it matches a key to a field ignoring case,
// which would let two spellings of one key into a merged file.
func checkKeys(md toml.MetaData) error {
	var bad []string
	for _, k := range md.Undecoded() {
		bad = append(bad, k.String())
	}
	known := map[string]map[string]bool{
		tablePolicy:   tagNames(policyKeys{}),
		tableInstance: tagNames(instanceKeys{}),
		tableStore:    tagNames(storeKeys{}),
		tableAPI:      tagNames(API{}),
	}
	for _, k := range md.Keys() {
		keys, ok := known[k[0]]
		switch {
		case !ok:
			bad = append(bad, k.String())
		case len(k) == 2 && !keys[k[1]]:
			bad = append(bad, k.String())
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("unknown keys: %s", strings.Join(dedupe(bad), ", "))
}

// checkRequired fails on a missing [store] or [api] key and on [api] values out of range.
func checkRequired(md toml.MetaData, sk storeKeys, api API) error {
	var missing []string
	for _, k := range [][]string{
		{tableStore, "addr"},
		{tableAPI, "max_in_flight"},
		{tableAPI, "limiter_enabled"},
		{tableAPI, "refuse_after_acquire_bp"},
		{tableAPI, "db_dsn"},
	} {
		if !md.IsDefined(k...) {
			missing = append(missing, strings.Join(k, "."))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing keys: %s", strings.Join(missing, ", "))
	}
	var errs []error
	if sk.Addr == "" {
		errs = append(errs, errors.New("store.addr is empty"))
	}
	if api.MaxInFlight < 1 {
		errs = append(errs, fmt.Errorf("api.max_in_flight = %d: want at least 1", api.MaxInFlight))
	}
	if api.RefuseAfterAcquireBP < 0 || api.RefuseAfterAcquireBP > MaxRefuseBP {
		errs = append(errs, fmt.Errorf("api.refuse_after_acquire_bp = %d: want 0 to %d",
			api.RefuseAfterAcquireBP, MaxRefuseBP))
	}
	if api.DBDSN == "" {
		errs = append(errs, errors.New("api.db_dsn is empty"))
	}
	return errors.Join(errs...)
}

// tagNames returns the toml tag names of a struct's fields.
func tagNames(v any) map[string]bool {
	t := reflect.TypeOf(v)
	names := make(map[string]bool, t.NumField())
	for i := range t.NumField() {
		names[t.Field(i).Tag.Get("toml")] = true
	}
	return names
}

func dedupe(s []string) []string {
	seen := make(map[string]bool, len(s))
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Encode writes f as a config file that Load reads back as f (a test's edited stack config,
// IMPLEMENTATION_PLAN H1 step 12): every key of the four tables, the two enums by their SPEC
// names. The store user and password are never written (DESIGN §A2, §B2: the user is
// LibUser, the password comes only from LibPasswordEnv), so a File with another user is
// refused. The result must load: a value Load refuses, such as an empty store address, is an
// error here too.
func Encode(f File) ([]byte, error) {
	if f.Store.User != "" && f.Store.User != LibUser {
		return nil, fmt.Errorf("encoding the config: store user %q: the file's store user is always %s", f.Store.User, LibUser)
	}
	var buf bytes.Buffer
	keys := fileKeys{
		Policy:   policyKeysOf(f.Policy),
		Instance: instanceKeys(f.Instance),
		Store:    storeKeys{Addr: f.Store.Addr},
		API:      f.API,
	}
	if err := toml.NewEncoder(&buf).Encode(keys); err != nil {
		return nil, fmt.Errorf("encoding the config: %w", err)
	}
	if _, err := decode(buf.Bytes()); err != nil {
		return nil, fmt.Errorf("encoded config: %w", err)
	}
	return buf.Bytes(), nil
}

// Merge overlays a scenario's overrides on a base config file, key by key (§B4, §B5): each
// key of the overrides' [policy], [instance] and [api] tables replaces or adds that key of the
// base's table; every other key of the base stays. Overrides may hold only those three tables.
// The merged file must load (no unknown or missing key, valid values), and is returned
// re-encoded, without the base's comments.
func Merge(base, overrides []byte) ([]byte, error) {
	b, err := decodeTables(base)
	if err != nil {
		return nil, fmt.Errorf("base config: %w", err)
	}
	o, err := decodeTables(overrides)
	if err != nil {
		return nil, fmt.Errorf("overrides: %w", err)
	}
	names := make([]string, 0, len(o))
	for name := range o {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name != tablePolicy && name != tableInstance && name != tableAPI {
			return nil, fmt.Errorf("overrides: %s cannot be overridden (only [%s], [%s] and [%s])",
				name, tablePolicy, tableInstance, tableAPI)
		}
		ot, ok := o[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("overrides: %s is not a table", name)
		}
		bt, ok := b[name].(map[string]any)
		if !ok {
			if _, present := b[name]; present {
				return nil, fmt.Errorf("base config: %s is not a table", name)
			}
			bt = make(map[string]any, len(ot))
			b[name] = bt
		}
		for k, v := range ot {
			bt[k] = v
		}
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(b); err != nil {
		return nil, fmt.Errorf("encoding the merged config: %w", err)
	}
	if _, err := decode(buf.Bytes()); err != nil {
		return nil, fmt.Errorf("merged config: %w", err)
	}
	return buf.Bytes(), nil
}

// decodeTables parses a TOML document into its generic form.
func decodeTables(data []byte) (map[string]any, error) {
	var m map[string]any
	if _, err := toml.Decode(string(data), &m); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}
