package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// The scenario file's fixed values (DESIGN §B4 "Scenario file"; IMPLEMENTATION_PLAN H2 step 1).
const (
	RunBench = "bench" // run_kind of a benchmark run (bin/bench, §B5)
	RunTest  = "test"  // run_kind of a test-build run (bin/test, §B5)

	VerdictCapacity = "capacity" // T-RPT-2 capacity verdict (PRF-11)
	VerdictSpike    = "spike"    // T-RPT-2 spike verdict (PRF-6)
	VerdictNone     = "none"     // no T-RPT-2 verdict; the default

	DefaultInstances   = 3   // hitapi processes when instances is absent (§B4)
	DefaultMaxExtra    = 2   // [retries] max_extra when absent (§B4)
	MaxExtraLimit      = 2   // the most extra attempts after 429 (HARNESS TRF-5)
	DefaultJitterMaxMs = 50  // [retries] jitter_max_ms when absent (§B4)
	DefaultZipfS       = 1.1 // [users] zipf_s when absent (§B4)

	// MaxUsers is the most users a scenario may have (plan choice): user IDs are u%06d, so
	// every ID keeps its six digits.
	MaxUsers = 1_000_000
	// FaultctlPrefix starts a [[faults]] cmd that runs faultctl; any other cmd is a JSON
	// object, a /_test/faults body (plan choice for the cmd forms).
	FaultctlPrefix = "faultctl "
)

// Input bounds that keep a scenario's schedule finite and small enough to build in memory
// (plan choices, AGENTS.md §1 Input): the longest scenario (TRF-S7) runs 10 min and the
// largest plans a few million arrivals.
const (
	maxRunS            = 86_400      // a run lasts at most a day
	maxJitterMs        = 60_000      // retry jitter of at most a minute (DESIGN: 50 ms)
	maxPlannedArrivals = 50_000_000  // expected aggregate plus stream arrivals
	maxStateMoves      = 100_000_000 // expected quiet/active/burst moves over the run
)

// nameRe is a scenario name: it becomes part of the run ID and of the frozen thresholds file's
// name (§B5, §B9), so it is a path-safe word (plan choice).
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Scenario is one scenario file (DESIGN §B4 "Scenario file"), decoded onto its defaults and
// checked by LoadScenario. hitgen builds its schedule from it (H2) and the orchestrator its run
// (H3).
type Scenario struct {
	Name      string
	Seed      uint64 // drives every random choice (TRF-1)
	RunKind   string // RunBench or RunTest
	Instances int    // 1 or 3: hitapi processes started; routing uses this count (TRF-4)
	// Overrides holds the scenario's [policy], [instance] and [api] tables as TOML, the
	// overrides Merge lays over harness.toml for the run's config.toml (§B5); nil when the
	// scenario has none of the three.
	Overrides []byte
	Steps     []Step   // at least one; together they fix the run length
	Users     Users    // who receives each aggregate arrival
	Streams   []Stream // per-user Poisson streams on top of the steps
	Cohorts   []Cohort // users active only in their windows; at most one cohort per user
	// Duplicates is the [duplicates] table: the share of aggregate arrivals that repeat an
	// earlier entry's IDs (TRF-6).
	Duplicates         Duplicates
	Retries            Retries   // the 429 retry policy (TRF-5)
	Faults             []Fault   // run by the orchestrator (test runs only, H3)
	ForcedAOFRewritesS []float64 // offsets, in s, of the forced BGREWRITEAOF runs (TRF-S7)
	StopEarly          bool      // stop after the first generator-limited or failing step (§B4)
	Verdict            string    // VerdictCapacity, VerdictSpike or VerdictNone
	// SpikeStep is the spike step's 0-based index (like stopped_after_step; plan choice); set
	// exactly when Verdict is VerdictSpike, and never the last step: the spike needs the step
	// after it (§B9 "Spike recovery").
	SpikeStep *int
}

// Step is one [[steps]] table: a Poisson process at RatePerS for DurationS seconds. A rate of
// 0 is allowed: the step plans no aggregate arrivals but still lasts (plan choice).
type Step struct {
	RatePerS  float64
	DurationS float64
}

// Users is the [users] table: Count users, u000000 to u(Count−1), drawn for each aggregate
// arrival with weight 1/rank^ZipfS (u000000 has rank 1) times their state's weight.
type Users struct {
	Count int
	ZipfS float64
	// States is [users.states]; nil when absent, and then every user is always active with
	// weight 1 (plan choice).
	States *States
}

// States is [users.states]: each user starts active, stays in a state for an exponential time
// with that state's mean, then moves to one of the other two at random (plan choice).
type States struct {
	Quiet, Active, Burst State
}

// State is one [users.states.<quiet|active|burst>] table. Weight is the state's relative
// arrival weight: 0 for quiet, more than 0 for active and burst.
type State struct {
	Weight     float64
	MeanDwellS float64
}

// Range is an inclusive range of user indexes, [First, Last] (plan choice).
type Range struct {
	First, Last int
}

// Stream is one [[streams]] table: every user in Users gets its own Poisson stream at
// RatePerS for the whole run.
type Stream struct {
	Users    Range
	RatePerS float64
}

// Cohort is one [[cohorts]] table: its users are active only in [StartS, StopS) and again from
// RevisitS (§B4). StartS defaults to 0; with no StopS they never stop, and with no RevisitS
// they never return.
type Cohort struct {
	Users    Range
	StartS   float64
	StopS    *float64
	RevisitS *float64
}

// Duplicates is the [duplicates] table: Fraction is the probability that an aggregate arrival
// repeats an earlier entry's user and request ID instead (0 when absent).
type Duplicates struct {
	Fraction float64
}

// Retries is the [retries] table (TRF-5).
type Retries struct {
	MaxExtra    int   // extra attempts after 429: 0 to 2
	JitterMaxMs int64 // seeded jitter is uniform in [0, JitterMaxMs] ms
}

// Fault is one [[faults]] table: Cmd is a faultctl command line (FaultctlPrefix …) or a
// /_test/faults JSON body, run AtS seconds after hitgen starts (H3). Targets lists the
// instances it goes to: the one named by instance, or every instance when absent.
type Fault struct {
	AtS     float64
	Cmd     string
	Targets []int
}

// Faultctl reports whether the fault's Cmd runs faultctl; otherwise it is a JSON body.
func (f Fault) Faultctl() bool { return strings.HasPrefix(f.Cmd, FaultctlPrefix) }

// SecondsToNs converts a scenario offset or duration in seconds to nanoseconds, rounded to
// the nearest.
func SecondsToNs(s float64) int64 { return int64(math.Round(s * 1e9)) }

// OverridesKey reports whether the scenario's overrides set key in table ("policy",
// "instance" or "api"), whatever the value: a benchmark run refuses any clock_source override
// (DESIGN §B5 "Benchmark guard"; IMPLEMENTATION_PLAN H3 step 1).
func (sc Scenario) OverridesKey(table, key string) (bool, error) {
	if len(sc.Overrides) == 0 {
		return false, nil
	}
	m, err := decodeTables(sc.Overrides)
	if err != nil {
		return false, fmt.Errorf("overrides: %w", err)
	}
	t, ok := m[table].(map[string]any)
	if !ok {
		return false, nil
	}
	_, set := t[key]
	return set, nil
}

// RunNs is the run length: the sum of the steps' durations, each rounded to whole ns.
func (sc Scenario) RunNs() int64 {
	var n int64
	for _, st := range sc.Steps {
		n += SecondsToNs(st.DurationS)
	}
	return n
}

// The decoded file. Pointers tell an absent key from a zero value.
type scenarioFile struct {
	Name               *string         `toml:"name"`
	Seed               *int64          `toml:"seed"`
	RunKind            *string         `toml:"run_kind"`
	Instances          *int            `toml:"instances"`
	Policy             toml.Primitive  `toml:"policy"`
	Instance           toml.Primitive  `toml:"instance"`
	API                toml.Primitive  `toml:"api"`
	Steps              []stepFile      `toml:"steps"`
	Users              *usersFile      `toml:"users"`
	Streams            []streamFile    `toml:"streams"`
	Cohorts            []cohortFile    `toml:"cohorts"`
	Duplicates         *duplicatesFile `toml:"duplicates"`
	Retries            *retriesFile    `toml:"retries"`
	Faults             []faultFile     `toml:"faults"`
	ForcedAOFRewritesS []float64       `toml:"forced_aof_rewrites_s"`
	StopEarly          *bool           `toml:"stop_early"`
	Verdict            *string         `toml:"verdict"`
	SpikeStep          *int            `toml:"spike_step"`
}

type stepFile struct {
	RatePerS  *float64 `toml:"rate_per_s"`
	DurationS *float64 `toml:"duration_s"`
}

type usersFile struct {
	Count  *int        `toml:"count"`
	ZipfS  *float64    `toml:"zipf_s"`
	States *statesFile `toml:"states"`
}

type statesFile struct {
	Quiet  *stateFile `toml:"quiet"`
	Active *stateFile `toml:"active"`
	Burst  *stateFile `toml:"burst"`
}

type stateFile struct {
	Weight     *float64 `toml:"weight"`
	MeanDwellS *float64 `toml:"mean_dwell_s"`
}

type streamFile struct {
	Users    []int    `toml:"users"`
	RatePerS *float64 `toml:"rate_per_s"`
}

type cohortFile struct {
	Users    []int    `toml:"users"`
	StartS   *float64 `toml:"start_s"`
	StopS    *float64 `toml:"stop_s"`
	RevisitS *float64 `toml:"revisit_s"`
}

type duplicatesFile struct {
	Fraction *float64 `toml:"fraction"`
}

type retriesFile struct {
	MaxExtra    *int   `toml:"max_extra"`
	JitterMaxMs *int64 `toml:"jitter_max_ms"`
}

type faultFile struct {
	AtS      *float64 `toml:"at_s"`
	Cmd      *string  `toml:"cmd"`
	Instance *int     `toml:"instance"`
}

// LoadScenario reads the scenario file at path (§B4) and applies the defaults: instances 3,
// [retries] max_extra 2 and jitter_max_ms 50, [users] zipf_s 1.1, verdict "none". It refuses,
// with an error naming each field: an unknown or misspelled key; a missing name, seed or
// run_kind, a scenario without [[steps]] or without [users] count, and any other missing key
// a table needs; then, once nothing is missing, every value Validate refuses.
func LoadScenario(path string) (Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, fmt.Errorf("reading the scenario: %w", err)
	}
	sc, err := decodeScenario(data)
	if err != nil {
		return Scenario{}, fmt.Errorf("scenario %s: %w", path, err)
	}
	return sc, nil
}

// decodeScenario parses a scenario file, fills the defaults and checks it.
func decodeScenario(data []byte) (Scenario, error) {
	var f scenarioFile
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return Scenario{}, fmt.Errorf("parsing: %w", err)
	}
	overrides, err := overrideTables(md, f)
	if err != nil {
		return Scenario{}, err
	}
	if err := checkScenarioKeys(md); err != nil {
		return Scenario{}, err
	}
	var errs []error
	missing := func(field string) { errs = append(errs, fmt.Errorf("%s is missing", field)) }
	sc := Scenario{
		Instances: DefaultInstances,
		Overrides: overrides,
		Users:     Users{ZipfS: DefaultZipfS},
		Retries:   Retries{MaxExtra: DefaultMaxExtra, JitterMaxMs: DefaultJitterMaxMs},
		Verdict:   VerdictNone,
		SpikeStep: f.SpikeStep,
	}
	if f.Name == nil {
		missing("name")
	} else {
		sc.Name = *f.Name
	}
	if f.Seed == nil {
		missing("seed")
	} else if s := *f.Seed; s < 0 {
		errs = append(errs, fmt.Errorf("seed = %d: want 0 or more", s))
	} else {
		sc.Seed = uint64(s)
	}
	if f.RunKind == nil {
		missing(`run_kind ("bench" or "test")`)
	} else {
		sc.RunKind = *f.RunKind
	}
	if f.Instances != nil {
		sc.Instances = *f.Instances
	}
	if len(f.Steps) == 0 {
		errs = append(errs, errors.New("steps: no [[steps]] table; the steps fix the run length"))
	}
	for i, st := range f.Steps {
		if st.RatePerS == nil {
			missing(fmt.Sprintf("steps[%d].rate_per_s", i))
		}
		if st.DurationS == nil {
			missing(fmt.Sprintf("steps[%d].duration_s", i))
		}
		sc.Steps = append(sc.Steps, Step{RatePerS: deref(st.RatePerS), DurationS: deref(st.DurationS)})
	}
	errs = append(errs, sc.Users.fill(f.Users)...)
	for i, s := range f.Streams {
		field := fmt.Sprintf("streams[%d]", i)
		r, err := userRange(field+".users", s.Users)
		if err != nil {
			errs = append(errs, err)
		}
		if s.RatePerS == nil {
			missing(field + ".rate_per_s")
		}
		sc.Streams = append(sc.Streams, Stream{Users: r, RatePerS: deref(s.RatePerS)})
	}
	for i, c := range f.Cohorts {
		r, err := userRange(fmt.Sprintf("cohorts[%d].users", i), c.Users)
		if err != nil {
			errs = append(errs, err)
		}
		sc.Cohorts = append(sc.Cohorts, Cohort{Users: r, StartS: deref(c.StartS), StopS: c.StopS, RevisitS: c.RevisitS})
	}
	if d := f.Duplicates; d != nil {
		if d.Fraction == nil {
			missing("duplicates.fraction")
		}
		sc.Duplicates.Fraction = deref(d.Fraction)
	}
	if r := f.Retries; r != nil {
		if r.MaxExtra != nil {
			sc.Retries.MaxExtra = *r.MaxExtra
		}
		if r.JitterMaxMs != nil {
			sc.Retries.JitterMaxMs = *r.JitterMaxMs
		}
	}
	for i, ft := range f.Faults {
		field := fmt.Sprintf("faults[%d]", i)
		if ft.AtS == nil {
			missing(field + ".at_s")
		}
		if ft.Cmd == nil {
			missing(field + ".cmd")
		}
		fault := Fault{AtS: deref(ft.AtS), Cmd: derefString(ft.Cmd)}
		switch {
		case ft.Instance != nil:
			fault.Targets = []int{*ft.Instance}
		case validInstances(sc.Instances):
			for n := 1; n <= sc.Instances; n++ {
				fault.Targets = append(fault.Targets, n)
			}
		}
		sc.Faults = append(sc.Faults, fault)
	}
	sc.ForcedAOFRewritesS = f.ForcedAOFRewritesS
	if f.StopEarly != nil {
		sc.StopEarly = *f.StopEarly
	}
	if f.Verdict != nil {
		sc.Verdict = *f.Verdict
	}
	if err := errors.Join(errs...); err != nil {
		return Scenario{}, err
	}
	if err := sc.Validate(); err != nil {
		return Scenario{}, err
	}
	return sc, nil
}

// fill decodes [users] onto u, which holds the defaults; it returns the missing keys.
func (u *Users) fill(f *usersFile) []error {
	if f == nil || f.Count == nil {
		return []error{errors.New("users.count is missing")}
	}
	u.Count = *f.Count
	if f.ZipfS != nil {
		u.ZipfS = *f.ZipfS
	}
	if f.States == nil {
		return nil
	}
	var errs []error
	state := func(name string, sf *stateFile) State {
		field := "users.states." + name
		if sf == nil {
			errs = append(errs, fmt.Errorf("%s is missing: [users.states] needs quiet, active and burst", field))
			return State{}
		}
		if sf.Weight == nil {
			errs = append(errs, fmt.Errorf("%s.weight is missing", field))
		}
		if sf.MeanDwellS == nil {
			errs = append(errs, fmt.Errorf("%s.mean_dwell_s is missing", field))
		}
		return State{Weight: deref(sf.Weight), MeanDwellS: deref(sf.MeanDwellS)}
	}
	u.States = &States{
		Quiet:  state("quiet", f.States.Quiet),
		Active: state("active", f.States.Active),
		Burst:  state("burst", f.States.Burst),
	}
	return errs
}

// userRange turns a users = [first, last] array into a Range; its bounds are checked by
// Validate, which knows the user count.
func userRange(field string, v []int) (Range, error) {
	if len(v) != 2 {
		return Range{}, fmt.Errorf("%s = %v: want [first, last], two user indexes", field, v)
	}
	return Range{First: v[0], Last: v[1]}, nil
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Validate checks the scenario's values (§B4; IMPLEMENTATION_PLAN H2 step 1) and returns
// every problem, each naming its field: the name; run_kind "bench" or "test"; instances 1 or
// 3; at least one step, each with a finite rate ≥ 0 and a duration > 0, the run at most a
// day; 1 to MaxUsers users and a finite zipf_s ≥ 0; states with quiet weight 0, active and
// burst weights > 0 and mean dwell times > 0; stream and cohort user ranges inside the users,
// stream rates > 0, cohort windows in order and no user in two cohorts; a duplicates fraction
// in [0, 1); max_extra 0 to 2 (TRF-5) and jitter_max_ms 0 to 60,000; each fault at an offset
// inside the run with a faultctl or JSON-object cmd and targets among 1…instances; rewrite
// offsets inside the run, in increasing order; verdict "capacity", "spike" or "none", with
// spike_step set exactly for "spike" and never the last step; the overrides only the
// [policy], [instance] and [api] keys of the harness config, of the right types; and a
// schedule small enough to build (maxPlannedArrivals, maxStateMoves).
func (sc Scenario) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !nameRe.MatchString(sc.Name) {
		bad("name = %q: want 1-64 characters from [A-Za-z0-9_-]", sc.Name)
	}
	if sc.RunKind != RunBench && sc.RunKind != RunTest {
		bad("run_kind = %q: want %q or %q", sc.RunKind, RunBench, RunTest)
	}
	if !validInstances(sc.Instances) {
		bad("instances = %d: want 1 or 3", sc.Instances)
	}
	if len(sc.Steps) == 0 {
		bad("steps: no [[steps]] table; the steps fix the run length")
	}
	runS, arrivals := 0.0, 0.0
	for i, st := range sc.Steps {
		if !finite(st.RatePerS) || st.RatePerS < 0 {
			bad("steps[%d].rate_per_s = %v: want a finite rate of 0 or more", i, st.RatePerS)
		}
		if !finite(st.DurationS) || st.DurationS <= 0 {
			bad("steps[%d].duration_s = %v: want a finite duration above 0", i, st.DurationS)
		}
		runS += st.DurationS
		arrivals += st.RatePerS * st.DurationS
	}
	if len(errs) == 0 && runS > maxRunS {
		bad("steps: the run lasts %v s: want at most %d s", runS, maxRunS)
	}
	errs = append(errs, sc.Users.validate(runS)...)
	userOK := func(field string, r Range) {
		if r.First < 0 || r.First > r.Last || r.Last >= sc.Users.Count {
			bad("%s = [%d, %d]: want 0 ≤ first ≤ last < users.count (%d)", field, r.First, r.Last, sc.Users.Count)
		}
	}
	for i, s := range sc.Streams {
		userOK(fmt.Sprintf("streams[%d].users", i), s.Users)
		if !finite(s.RatePerS) || s.RatePerS <= 0 {
			bad("streams[%d].rate_per_s = %v: want a finite rate above 0", i, s.RatePerS)
		}
		arrivals += float64(s.Users.Last-s.Users.First+1) * s.RatePerS * runS
	}
	for i, c := range sc.Cohorts {
		field := fmt.Sprintf("cohorts[%d]", i)
		userOK(field+".users", c.Users)
		errs = append(errs, c.validate(field)...)
		for j, d := range sc.Cohorts[:i] {
			if c.Users.First <= d.Users.Last && d.Users.First <= c.Users.Last {
				bad("%s.users = [%d, %d] overlaps cohorts[%d].users = [%d, %d]: a user belongs to at most one cohort",
					field, c.Users.First, c.Users.Last, j, d.Users.First, d.Users.Last)
			}
		}
	}
	if f := sc.Duplicates.Fraction; !finite(f) || f < 0 || f >= 1 {
		bad("duplicates.fraction = %v: want 0 ≤ fraction < 1", f)
	}
	if m := sc.Retries.MaxExtra; m < 0 || m > MaxExtraLimit {
		bad("retries.max_extra = %d: want 0 to %d (TRF-5)", m, MaxExtraLimit)
	}
	if j := sc.Retries.JitterMaxMs; j < 0 || j > maxJitterMs {
		bad("retries.jitter_max_ms = %d: want 0 to %d", j, maxJitterMs)
	}
	for i, f := range sc.Faults {
		errs = append(errs, f.validate(fmt.Sprintf("faults[%d]", i), sc.Instances, runS)...)
	}
	for i, at := range sc.ForcedAOFRewritesS {
		if !finite(at) || at < 0 || at >= runS {
			bad("forced_aof_rewrites_s[%d] = %v: want an offset in [0, %v) s, inside the run", i, at, runS)
		}
		if i > 0 && !(at > sc.ForcedAOFRewritesS[i-1]) {
			bad("forced_aof_rewrites_s[%d] = %v: want the offsets in increasing order", i, at)
		}
	}
	errs = append(errs, sc.validateVerdict()...)
	if err := checkOverrides(sc.Overrides); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 && arrivals > maxPlannedArrivals {
		bad("steps and streams: about %.0f planned arrivals: want at most %d", arrivals, maxPlannedArrivals)
	}
	return errors.Join(errs...)
}

// validate checks [users] and [users.states]; runS is the run length, in seconds.
func (u Users) validate(runS float64) []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if u.Count < 1 || u.Count > MaxUsers {
		bad("users.count = %d: want 1 to %d (user IDs u000000 to u999999)", u.Count, MaxUsers)
	}
	if !finite(u.ZipfS) || u.ZipfS < 0 {
		bad("users.zipf_s = %v: want a finite exponent of 0 or more", u.ZipfS)
	}
	if u.States == nil {
		return errs
	}
	if w := u.States.Quiet.Weight; w != 0 {
		bad("users.states.quiet.weight = %v: want 0 (a quiet user receives no aggregate arrivals)", w)
	}
	minDwell := math.Inf(1)
	for _, s := range []struct {
		name string
		st   State
	}{{"quiet", u.States.Quiet}, {"active", u.States.Active}, {"burst", u.States.Burst}} {
		if s.name != "quiet" && (!finite(s.st.Weight) || s.st.Weight <= 0) {
			bad("users.states.%s.weight = %v: want a finite weight above 0", s.name, s.st.Weight)
		}
		if d := s.st.MeanDwellS; !finite(d) || d <= 0 {
			bad("users.states.%s.mean_dwell_s = %v: want a finite mean above 0", s.name, d)
		} else {
			minDwell = min(minDwell, d)
		}
	}
	if moves := float64(u.Count) * runS / minDwell; len(errs) == 0 && moves > maxStateMoves {
		bad("users.states: about %.0f state moves over the run (users.count × run ÷ the shortest mean_dwell_s): want at most %d",
			moves, maxStateMoves)
	}
	return errs
}

// validate checks a cohort's window; field names it.
func (c Cohort) validate(field string) []error {
	var errs []error
	if !finite(c.StartS) || c.StartS < 0 || c.StartS > maxRunS {
		errs = append(errs, fmt.Errorf("%s.start_s = %v: want an offset of 0 to %d s", field, c.StartS, maxRunS))
	}
	if c.StopS != nil {
		if s := *c.StopS; !finite(s) || s <= c.StartS || s > maxRunS {
			errs = append(errs, fmt.Errorf("%s.stop_s = %v: want an offset after start_s (%v) and at most %d s", field, s, c.StartS, maxRunS))
		}
	}
	if c.RevisitS != nil {
		switch r := *c.RevisitS; {
		case c.StopS == nil:
			errs = append(errs, fmt.Errorf("%s.revisit_s = %v: a cohort returns only after stop_s", field, r))
		case !finite(r) || r <= *c.StopS || r > maxRunS:
			errs = append(errs, fmt.Errorf("%s.revisit_s = %v: want an offset after stop_s (%v) and at most %d s", field, r, *c.StopS, maxRunS))
		}
	}
	return errs
}

// validate checks one fault: an offset inside the run, a cmd in one of its two forms, and
// targets among 1…instances (once instances itself is valid).
func (f Fault) validate(field string, instances int, runS float64) []error {
	var errs []error
	if !finite(f.AtS) || f.AtS < 0 || f.AtS >= runS {
		errs = append(errs, fmt.Errorf("%s.at_s = %v: want an offset in [0, %v) s, inside the run", field, f.AtS, runS))
	}
	if !validCmd(f.Cmd) {
		errs = append(errs, fmt.Errorf("%s.cmd = %q: want a command starting with %q or a JSON object (a /_test/faults body)",
			field, f.Cmd, FaultctlPrefix))
	}
	if !validInstances(instances) {
		return errs
	}
	if len(f.Targets) == 0 {
		errs = append(errs, fmt.Errorf("%s: no target instance", field))
	}
	for _, n := range f.Targets {
		if n < 1 || n > instances {
			errs = append(errs, fmt.Errorf("%s.instance = %d: want 1 to %d (instances)", field, n, instances))
		}
	}
	return errs
}

// validCmd reports whether cmd is a faultctl command line with arguments or one JSON object.
func validCmd(cmd string) bool {
	if strings.HasPrefix(cmd, FaultctlPrefix) {
		return strings.TrimSpace(cmd[len(FaultctlPrefix):]) != ""
	}
	dec := json.NewDecoder(strings.NewReader(cmd))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

// validateVerdict checks verdict and spike_step.
func (sc Scenario) validateVerdict() []error {
	switch sc.Verdict {
	case VerdictCapacity, VerdictNone:
		if sc.SpikeStep != nil {
			return []error{fmt.Errorf("spike_step = %d: only a verdict = %q scenario has a spike step", *sc.SpikeStep, VerdictSpike)}
		}
	case VerdictSpike:
		switch {
		case sc.SpikeStep == nil:
			return []error{fmt.Errorf("spike_step is missing: verdict = %q needs the spike's 0-based step index", VerdictSpike)}
		case *sc.SpikeStep < 0 || *sc.SpikeStep > len(sc.Steps)-2:
			return []error{fmt.Errorf("spike_step = %d: want the 0-based index of a step other than the last of the %d (the spike needs a step after it)",
				*sc.SpikeStep, len(sc.Steps))}
		}
	default:
		return []error{fmt.Errorf("verdict = %q: want %q, %q or %q", sc.Verdict, VerdictCapacity, VerdictSpike, VerdictNone)}
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// validInstances reports whether n is a valid instance count: 1 or 3 (§B4).
func validInstances(n int) bool { return n == 1 || n == 3 }

// overrideTables returns the file's [policy], [instance] and [api] tables as TOML for Merge,
// or nil when it has none of them. Each must be a table whose values have their keys' types
// (checked here, so an error points at the scenario's line); the key names are checked with
// the scenario's other keys.
func overrideTables(md toml.MetaData, f scenarioFile) ([]byte, error) {
	tables := map[string]any{}
	for _, t := range []struct {
		name string
		prim toml.Primitive
		into any
	}{{tablePolicy, f.Policy, &policyKeys{}}, {tableInstance, f.Instance, &instanceKeys{}}, {tableAPI, f.API, &API{}}} {
		if !md.IsDefined(t.name) {
			continue
		}
		// The decoder turns a value that is not a table into an empty map, without an error, so
		// the value itself is checked first. A table written with dotted keys (policy.x = …) is
		// a table too, though md.Type names no type for it.
		var v any
		if err := md.PrimitiveDecode(t.prim, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
		if _, ok := v.(map[string]any); !ok {
			return nil, fmt.Errorf("%s: a TOML %s, want a table of %s overrides", t.name, tomlTypeName(md.Type(t.name)), t.name)
		}
		if err := md.PrimitiveDecode(t.prim, t.into); err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
		var m map[string]any
		if err := md.PrimitiveDecode(t.prim, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
		tables[t.name] = m
	}
	if len(tables) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(tables); err != nil {
		return nil, fmt.Errorf("encoding the overrides: %w", err)
	}
	return buf.Bytes(), nil
}

// tomlTypeName is the TOML name of a value's type as md.Type reports it ("Integer", "Array",
// "ArrayHash", …).
func tomlTypeName(typ string) string {
	switch typ {
	case "ArrayHash":
		return "array of tables"
	case "Bool":
		return "boolean"
	}
	return strings.ToLower(typ)
}

// checkOverrides checks a scenario's overrides: only [policy], [instance] and [api], each key
// one of that table's harness-config keys spelled exactly, each value of its key's type (the
// enums by their SPEC names). The values' ranges are checked when Merge loads the merged file.
func checkOverrides(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var raw struct {
		Policy   toml.Primitive `toml:"policy"`
		Instance toml.Primitive `toml:"instance"`
		API      toml.Primitive `toml:"api"`
	}
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return fmt.Errorf("overrides: parsing: %w", err)
	}
	var errs []error
	pk, ik, api := policyKeys{}, instanceKeys{}, API{}
	for _, t := range []struct {
		name string
		prim toml.Primitive
		into any
	}{{tablePolicy, raw.Policy, &pk}, {tableInstance, raw.Instance, &ik}, {tableAPI, raw.API, &api}} {
		if err := md.PrimitiveDecode(t.prim, t.into); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.name, err))
		}
	}
	if err := checkKeys(md); err != nil {
		errs = append(errs, fmt.Errorf("overrides: %w", err))
	}
	for _, k := range md.Keys() {
		if k[0] == tableStore {
			errs = append(errs, fmt.Errorf("overrides: %s cannot be overridden (only [%s], [%s] and [%s])",
				k, tablePolicy, tableInstance, tableAPI))
		}
	}
	return errors.Join(errs...)
}

// checkScenarioKeys fails on every key that is not a scenario key spelled exactly: the decoder
// leaves unknown keys undecoded, and it matches a key to a field ignoring case.
func checkScenarioKeys(md toml.MetaData) error {
	var bad []string
	for _, k := range md.Undecoded() {
		bad = append(bad, k.String())
	}
	for _, k := range md.Keys() {
		if !scenarioKey(k) {
			bad = append(bad, k.String())
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("unknown keys: %s", strings.Join(dedupe(bad), ", "))
}

// scenarioKey reports whether k, a key path as the decoder lists it (array tables without
// their index), is a scenario key.
func scenarioKey(k toml.Key) bool {
	tables := map[string][]string{
		"steps":      {"rate_per_s", "duration_s"},
		"users":      {"count", "zipf_s", "states"},
		"streams":    {"users", "rate_per_s"},
		"cohorts":    {"users", "start_s", "stop_s", "revisit_s"},
		"duplicates": {"fraction"},
		"retries":    {"max_extra", "jitter_max_ms"},
		"faults":     {"at_s", "cmd", "instance"},
	}
	overrides := map[string]map[string]bool{
		tablePolicy:   tagNames(policyKeys{}),
		tableInstance: tagNames(instanceKeys{}),
		tableAPI:      tagNames(API{}),
	}
	top := []string{"name", "seed", "run_kind", "instances", "forced_aof_rewrites_s", "stop_early", "verdict", "spike_step"}
	switch {
	case len(k) == 1:
		_, isTable := tables[k[0]]
		_, isOverride := overrides[k[0]]
		return slices.Contains(top, k[0]) || isTable || isOverride
	case len(k) == 2:
		if keys, ok := overrides[k[0]]; ok {
			return keys[k[1]]
		}
		return slices.Contains(tables[k[0]], k[1])
	case k[0] != "users" || k[1] != "states":
		return false
	case len(k) == 3:
		return slices.Contains([]string{"quiet", "active", "burst"}, k[2])
	case len(k) == 4:
		return slices.Contains([]string{"quiet", "active", "burst"}, k[2]) &&
			slices.Contains([]string{"weight", "mean_dwell_s"}, k[3])
	}
	return false
}
