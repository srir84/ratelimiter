package main

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/srir84/ratelimiter/harness/config"
)

// header is the identity every test scenario starts with.
const header = `
name = "sched"
run_kind = "test"
`

// scenario writes a scenario file with the given seed and body and loads it.
func scenario(t *testing.T, seed uint64, body string) config.Scenario {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.toml")
	text := fmt.Sprintf("%sseed = %d\n%s", header, seed, body)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, err := config.LoadScenario(path)
	if err != nil {
		t.Fatalf("LoadScenario: %v\n%s", err, text)
	}
	return sc
}

// build builds sc's schedule and fails the test when it is empty.
func build(t *testing.T, sc config.Scenario) ([]Entry, int) {
	t.Helper()
	entries, skipped := BuildSchedule(sc)
	if len(entries) == 0 {
		t.Fatalf("BuildSchedule planned no entries (%d skipped)", skipped)
	}
	return entries, skipped
}

// scheduleHash writes entries with writeSchedule into a fresh directory and returns the
// file's SHA-256 and its bytes.
func scheduleHash(t *testing.T, entries []Entry) ([sha256.Size]byte, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schedule.jsonl")
	if err := writeSchedule(path, entries); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data), data
}

const sec = int64(1_000_000_000)

// everySource is a scenario that uses every schedule source: steps (one at rate 0), users
// with states, cohorts that stop, revisit and start late, two streams and duplicates.
const everySource = `
[[steps]]
rate_per_s = 200
duration_s = 10
[[steps]]
rate_per_s = 400
duration_s = 10
[[steps]]
rate_per_s = 0
duration_s = 5
[[steps]]
rate_per_s = 300
duration_s = 5

[users]
count = 200
[users.states.quiet]
weight = 0
mean_dwell_s = 3
[users.states.active]
weight = 1
mean_dwell_s = 4
[users.states.burst]
weight = 5
mean_dwell_s = 1

[[cohorts]]
users = [0, 49]
stop_s = 10
revisit_s = 20
[[cohorts]]
users = [50, 59]
start_s = 5

[[streams]]
users = [60, 61]
rate_per_s = 20
[[streams]]
users = [3, 3]
rate_per_s = 10

[duplicates]
fraction = 0.05
`

// TestSchedule_Deterministic (plan H2): the same scenario and seed give a byte-identical
// schedule.jsonl (equal SHA-256), from a fresh load and build; seed + 1 gives another hash.
func TestSchedule_Deterministic(t *testing.T) {
	first, firstSkipped := build(t, scenario(t, 20261008, everySource))
	again, againSkipped := build(t, scenario(t, 20261008, everySource))
	other, _ := build(t, scenario(t, 20261009, everySource))
	h1, data := scheduleHash(t, first)
	h2, _ := scheduleHash(t, again)
	h3, _ := scheduleHash(t, other)
	if h1 != h2 {
		t.Errorf("two builds of one scenario and seed: SHA-256 %x and %x, want equal", h1, h2)
	}
	if firstSkipped != againSkipped {
		t.Errorf("two builds of one scenario and seed skipped %d and %d arrivals, want equal", firstSkipped, againSkipped)
	}
	if h1 == h3 {
		t.Errorf("seed 20261008 and seed 20261009 give the same schedule (SHA-256 %x), want different", h1)
	}
	if n := bytes.Count(data, []byte{'\n'}); n != len(first) {
		t.Errorf("schedule.jsonl has %d lines, want one per entry (%d)", n, len(first))
	}
	kinds := map[string]int{}
	for _, e := range first {
		kinds[e.Kind]++
	}
	if kinds[kindInitial] == 0 || kinds[kindDuplicate] == 0 {
		t.Errorf("entry kinds %v, want both initial and duplicate entries in this scenario", kinds)
	}
}

// TestSchedule_PoissonRate (plan H2): 100/s for 60 s plans 6,000 ± 310 arrivals (4σ), and the
// gaps between them have a coefficient of variation in [0.9, 1.1] (an exponential's is 1).
func TestSchedule_PoissonRate(t *testing.T) {
	entries, skipped := build(t, scenario(t, 7, `
[[steps]]
rate_per_s = 100
duration_s = 60
[users]
count = 10
`))
	if n := len(entries); n < 6000-310 || n > 6000+310 {
		t.Errorf("100/s for 60 s planned %d arrivals, want 6,000 ± 310", n)
	}
	if skipped != 0 {
		t.Errorf("%d arrivals skipped with every user always active, want 0", skipped)
	}
	var sum, sumSq float64
	prev := int64(0) // the step starts at 0
	for _, e := range entries {
		if e.PlannedNs < prev || e.PlannedNs >= 60*sec {
			t.Fatalf("entry %d planned at %d ns after %d: want nondecreasing offsets inside [0, 60 s)", e.I, e.PlannedNs, prev)
		}
		gap := float64(e.PlannedNs - prev)
		sum += gap
		sumSq += gap * gap
		prev = e.PlannedNs
	}
	n := float64(len(entries))
	mean := sum / n
	cv := math.Sqrt(sumSq/n-mean*mean) / mean
	if cv < 0.9 || cv > 1.1 {
		t.Errorf("gap coefficient of variation %.3f (mean gap %.0f ns), want in [0.9, 1.1]", cv, mean)
	}
}

// TestSchedule_ZipfSkew (plan H2): with 100 always-active users and 50,000 arrivals,
// u000000's share is within 10 % of its Zipf weight share 1 / Σ_{r=1}^{100} r^-1.1 = 0.23375
// (computed by hand); the top decile gets more arrivals than the bottom decile. With
// zipf_s = 0.6 the share is 1 / Σ_{r=1}^{100} r^-0.6 = 0.07219 (by hand), within 10 %.
func TestSchedule_ZipfSkew(t *testing.T) {
	entries, _ := build(t, scenario(t, 11, `
[[steps]]
rate_per_s = 1000
duration_s = 50
[users]
count = 100
`))
	if n := len(entries); n < 49_000 || n > 51_000 {
		t.Fatalf("1,000/s for 50 s planned %d arrivals, want about 50,000", n)
	}
	h := 0.0
	for r := 1; r <= 100; r++ {
		h += math.Pow(float64(r), -1.1)
	}
	want := 1 / h
	if math.Abs(want-0.23375) > 0.00001 {
		t.Fatalf("the test's own Zipf share %.5f, want the hand-computed 0.23375", want)
	}
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.User]++
	}
	share := float64(counts["u000000"]) / float64(len(entries))
	if math.Abs(share/want-1) > 0.10 {
		t.Errorf("u000000 got %.4f of the arrivals, want within 10 %% of its Zipf share %.4f", share, want)
	}
	top, bottom := 0, 0
	for u := range 10 {
		top += counts[fmt.Sprintf("u%06d", u)]
		bottom += counts[fmt.Sprintf("u%06d", 90+u)]
	}
	if top <= bottom {
		t.Errorf("the top decile got %d arrivals and the bottom decile %d, want more for the top", top, bottom)
	}

	// Another exponent: the weights follow zipf_s.
	h = 0
	for r := 1; r <= 100; r++ {
		h += math.Pow(float64(r), -0.6)
	}
	want = 1 / h
	if math.Abs(want-0.07219) > 0.00001 {
		t.Fatalf("the test's own Zipf share for zipf_s = 0.6 %.5f, want the hand-computed 0.07219", want)
	}
	entries, _ = build(t, scenario(t, 11, `
[[steps]]
rate_per_s = 1000
duration_s = 50
[users]
count = 100
zipf_s = 0.6
`))
	heaviest := 0
	for _, e := range entries {
		if e.User == "u000000" {
			heaviest++
		}
	}
	if share := float64(heaviest) / float64(len(entries)); math.Abs(share/want-1) > 0.10 {
		t.Errorf("zipf_s = 0.6: u000000 got %.4f of the %d arrivals, want within 10 %% of its Zipf share %.4f", share, len(entries), want)
	}
}

// TestSchedule_RoutingAndBusyUser (plan H2, TRF-4): every entry goes to instance i mod 3 + 1,
// and a user with a 100/s stream reaches all 3 instances, each with 33 % ± 3 % of its entries.
// With instances = 1, every entry goes to instance 1.
func TestSchedule_RoutingAndBusyUser(t *testing.T) {
	entries, _ := build(t, scenario(t, 3, `
instances = 3
[[steps]]
rate_per_s = 50
duration_s = 60
[users]
count = 101
[[streams]]
users = [0, 0]
rate_per_s = 100
[[streams]]
users = [1, 100]
rate_per_s = 5
`))
	perInstance := map[int]int{}
	busy := 0
	for i, e := range entries {
		if e.I != i || e.Instance != i%3+1 {
			t.Fatalf("entry %d: I %d, instance %d, want I %d and instance %d", i, e.I, e.Instance, i, i%3+1)
		}
		if e.User == "u000000" {
			perInstance[e.Instance]++
			busy++
		}
	}
	if busy < 6000-310 {
		t.Fatalf("the busy user has %d entries, want about 6,000 (100/s for 60 s)", busy)
	}
	for n := 1; n <= 3; n++ {
		if share := float64(perInstance[n]) / float64(busy); share < 0.30 || share > 0.36 {
			t.Errorf("instance %d got %.3f of the busy user's %d entries, want 33 %% ± 3 %%", n, share, busy)
		}
	}

	one, _ := build(t, scenario(t, 3, `
instances = 1
[[steps]]
rate_per_s = 50
duration_s = 10
[users]
count = 10
`))
	for _, e := range one {
		if e.Instance != 1 {
			t.Fatalf("instances = 1: entry %d goes to instance %d, want 1", e.I, e.Instance)
		}
	}
}

// TestSchedule_CohortsAndStreams (plan H2): no arrival for a cohort user while its cohort is
// stopped, [stop_s, revisit_s), and none before a late cohort's start_s, a stream on a cohort
// user included; a stream user's count matches rate × duration within 4σ, counted in a
// rate-0 step, where only streams plan arrivals.
func TestSchedule_CohortsAndStreams(t *testing.T) {
	entries, _ := build(t, scenario(t, 5, `
[[steps]]
rate_per_s = 200
duration_s = 30
[[steps]]
rate_per_s = 0
duration_s = 30
[users]
count = 50
[[cohorts]]
users = [0, 19]
start_s = 0
stop_s = 10
revisit_s = 20
[[cohorts]]
users = [20, 29]
start_s = 15
[[streams]]
users = [5, 5]
rate_per_s = 20
[[streams]]
users = [40, 40]
rate_per_s = 50
`))
	before, after := 0, 0 // cohort-0 arrivals before its stop and after its revisit
	stream40, stream5 := 0, 0
	for _, e := range entries {
		var u int
		if _, err := fmt.Sscanf(e.User, "u%06d", &u); err != nil {
			t.Fatalf("entry %d: user %q: %v", e.I, e.User, err)
		}
		switch {
		case u <= 19 && e.PlannedNs >= 10*sec && e.PlannedNs < 20*sec:
			t.Errorf("entry %d: %s planned at %d ns, inside its cohort's stop [10 s, 20 s)", e.I, e.User, e.PlannedNs)
		case u <= 19 && e.PlannedNs < 10*sec:
			before++
		case u <= 19:
			after++
		case u <= 29 && e.PlannedNs < 15*sec:
			t.Errorf("entry %d: %s planned at %d ns, before its cohort's start (15 s)", e.I, e.User, e.PlannedNs)
		}
		if e.PlannedNs >= 30*sec {
			switch u {
			case 40:
				stream40++
			case 5:
				stream5++
			default:
				t.Errorf("entry %d: %s planned at %d ns, in the rate-0 step, and not a stream user", e.I, e.User, e.PlannedNs)
			}
		}
	}
	if before == 0 || after == 0 {
		t.Errorf("cohort 0 got %d arrivals before its stop and %d after its revisit, want some in both", before, after)
	}
	// 50/s × 30 s = 1,500 ± 4√1,500 (155); 20/s × 30 s = 600 ± 4√600 (98).
	if stream40 < 1500-155 || stream40 > 1500+155 {
		t.Errorf("stream user u000040 has %d entries in the 30 s rate-0 step, want 1,500 ± 155", stream40)
	}
	if stream5 < 600-98 || stream5 > 600+98 {
		t.Errorf("stream user u000005 (cohort 0, revisited) has %d entries in the rate-0 step, want 600 ± 98", stream5)
	}
}

// TestSchedule_NoEligibleSkips (plan H2): with every user in one cohort stopped over
// [stop_s, revisit_s), no entry is planned there, and skipped_no_eligible is above 0 and equal
// to the arrivals drawn there: the same scenario without the cohort plans exactly those
// arrivals in the window (the steps' arrival times never depend on who is eligible) and the
// same ones outside it. 300/s over the 10 s window draws 3,000 ± 219 (4σ).
func TestSchedule_NoEligibleSkips(t *testing.T) {
	const steps = `
[[steps]]
rate_per_s = 300
duration_s = 30
[users]
count = 30
`
	stopped, skipped := build(t, scenario(t, 13, steps+`
[[cohorts]]
users = [0, 29]
stop_s = 10
revisit_s = 20
`))
	open, openSkipped := build(t, scenario(t, 13, steps))
	inWindow := func(e Entry) bool { return e.PlannedNs >= 10*sec && e.PlannedNs < 20*sec }
	for _, e := range stopped {
		if inWindow(e) {
			t.Errorf("entry %d planned at %d ns, inside the window where no user is eligible", e.I, e.PlannedNs)
		}
	}
	var drawn int
	var openOutside, stoppedOutside []int64
	for _, e := range open {
		if inWindow(e) {
			drawn++
		} else {
			openOutside = append(openOutside, e.PlannedNs)
		}
	}
	for _, e := range stopped {
		stoppedOutside = append(stoppedOutside, e.PlannedNs)
	}
	if skipped <= 0 || skipped != drawn {
		t.Errorf("skipped_no_eligible = %d, want above 0 and equal to the %d arrivals drawn in the window", skipped, drawn)
	}
	if skipped < 3000-219 || skipped > 3000+219 {
		t.Errorf("skipped_no_eligible = %d, want 3,000 ± 219 (300/s over 10 s)", skipped)
	}
	if openSkipped != 0 {
		t.Errorf("without the cohort %d arrivals were skipped, want 0", openSkipped)
	}
	if !reflect.DeepEqual(openOutside, stoppedOutside) {
		t.Errorf("outside the window the schedules differ: %d and %d arrivals, want the same planned times",
			len(openOutside), len(stoppedOutside))
	}
}

// TestSchedule_Duplicates (plan H2, TRF-6): fraction = 0.1 makes 10 % ± 1 % of the entries
// duplicates, each pointing to an earlier initial entry with the same user and request ID,
// chosen uniformly: every user is always eligible here, so about half of entry i's earlier
// initial entries lie below i/2, and 0.45 to 0.55 of the duplicates point there. Only
// aggregate arrivals become duplicates, and only of entries whose user is eligible at the
// duplicate's planned time (§B4 cohorts and states); with none, the arrival stays initial.
func TestSchedule_Duplicates(t *testing.T) {
	entries, _ := build(t, scenario(t, 17, `
[[steps]]
rate_per_s = 500
duration_s = 60
[users]
count = 100
[duplicates]
fraction = 0.1
`))
	dups := 0
	for _, e := range entries {
		switch e.Kind {
		case kindInitial:
			if e.DuplicateOf != -1 {
				t.Errorf("initial entry %d has duplicate_of %d, want none", e.I, e.DuplicateOf)
			}
		case kindDuplicate:
			dups++
			if e.DuplicateOf < 0 || e.DuplicateOf >= e.I {
				t.Errorf("duplicate entry %d points to entry %d, want an earlier one", e.I, e.DuplicateOf)
				continue
			}
			of := entries[e.DuplicateOf]
			if of.Kind != kindInitial || of.User != e.User || of.RequestID != e.RequestID {
				t.Errorf("duplicate entry %d (%s, %s) points to entry %d (%s, %s, %s), want an initial entry with the same IDs",
					e.I, e.User, e.RequestID, of.I, of.Kind, of.User, of.RequestID)
			}
		default:
			t.Errorf("entry %d has kind %q, want initial or duplicate", e.I, e.Kind)
		}
	}
	if share := float64(dups) / float64(len(entries)); share < 0.09 || share > 0.11 {
		t.Errorf("%d of %d entries are duplicates (%.4f), want 10 %% ± 1 %%", dups, len(entries), share)
	}
	below := 0
	for _, e := range entries {
		if e.Kind == kindDuplicate && 2*e.DuplicateOf < e.I {
			below++
		}
	}
	if share := float64(below) / float64(dups); share < 0.45 || share > 0.55 {
		t.Errorf("%d of %d duplicates (%.3f) point below i/2, want 0.45 to 0.55 (a uniform choice)", below, dups, share)
	}

	t.Run("only aggregate arrivals", func(t *testing.T) {
		// The second step plans no aggregate arrivals, only the streams' (5 users at 20/s).
		entries, _ := build(t, scenario(t, 83, `
[[steps]]
rate_per_s = 200
duration_s = 10
[[steps]]
rate_per_s = 0
duration_s = 10
[users]
count = 20
[[streams]]
users = [0, 4]
rate_per_s = 20
[duplicates]
fraction = 0.5
`))
		early, late := 0, 0 // duplicates in the first step, entries in the second
		for _, e := range entries {
			switch {
			case e.PlannedNs >= 10*sec:
				late++
				if e.Kind != kindInitial {
					t.Errorf("entry %d planned at %d ns, in the rate-0 step, is a %s, want only initial stream entries", e.I, e.PlannedNs, e.Kind)
				}
			case e.Kind == kindDuplicate:
				early++
			}
		}
		if early == 0 || late == 0 {
			t.Errorf("%d duplicates in the first step and %d entries in the rate-0 step, want some of both", early, late)
		}
	})

	t.Run("stream entries", func(t *testing.T) {
		// A duplicate repeats any earlier initial entry, a [[streams]] one too. With zipf_s = 40
		// the steps draw u000001 with a chance of about 2^-40, so its entries are its stream's:
		// 100/s against u000000's 80/s of initial step entries (100/s × (1 − 0.2)). Uniform
		// choices give u000001 5/9 of the duplicates: 1,200 ± 139 duplicates (4σ), and its share
		// 5/9 ± 0.06 (about 4σ, σ = √(5/9 × 4/9 / 1,200) ≈ 0.0143).
		entries, _ := build(t, scenario(t, 89, `
[[steps]]
rate_per_s = 100
duration_s = 60
[users]
count = 2
zipf_s = 40
[[streams]]
users = [1, 1]
rate_per_s = 100
[duplicates]
fraction = 0.2
`))
		dups, ofStream := 0, 0
		for _, e := range entries {
			if e.Kind != kindDuplicate {
				continue
			}
			dups++
			if e.User == "u000001" {
				ofStream++
			}
		}
		if dups < 1200-139 || dups > 1200+139 {
			t.Fatalf("%d duplicates, want 1,200 ± 139 (4σ)", dups)
		}
		if share := float64(ofStream) / float64(dups); math.Abs(share-5.0/9) > 0.06 {
			t.Errorf("%d of %d duplicates (%.3f) repeat u000001's stream entries, want 5/9 ± 0.06", ofStream, dups, share)
		}
	})

	t.Run("cohort windows", func(t *testing.T) {
		entries, _ := build(t, scenario(t, 41, `
[[steps]]
rate_per_s = 300
duration_s = 30
[users]
count = 30
[[cohorts]]
users = [0, 19]
stop_s = 10
revisit_s = 20
[duplicates]
fraction = 0.1
`))
		inStop := 0 // duplicates planned while cohort 0 is stopped
		for _, e := range entries {
			stopped := e.PlannedNs >= 10*sec && e.PlannedNs < 20*sec
			if u := userIndex(t, e.User); u <= 19 && stopped {
				t.Errorf("%s entry %d for %s planned at %d ns, while its cohort is stopped [10 s, 20 s)", e.Kind, e.I, e.User, e.PlannedNs)
			}
			if stopped && e.Kind == kindDuplicate {
				inStop++
			}
		}
		if inStop == 0 {
			t.Error("no duplicate planned in [10 s, 20 s), want some, for users 20 to 29")
		}
	})

	t.Run("no eligible earlier entry", func(t *testing.T) {
		// From 10 s only cohort 1's users are eligible, and none of them has an entry before
		// 10 s: the first entry from 10 s on stays initial though 9 in 10 arrivals are drawn as
		// duplicates, and every later duplicate repeats an entry planned from 10 s on.
		entries, _ := build(t, scenario(t, 43, `
[[steps]]
rate_per_s = 300
duration_s = 20
[users]
count = 20
[[cohorts]]
users = [0, 9]
stop_s = 10
[[cohorts]]
users = [10, 19]
start_s = 10
[duplicates]
fraction = 0.9
`))
		firstLate, lateDups := -1, 0
		for _, e := range entries {
			if e.PlannedNs < 10*sec {
				continue
			}
			if firstLate < 0 {
				firstLate = e.I
				if e.Kind != kindInitial {
					t.Errorf("entry %d, the first from 10 s on, is a %s of entry %d, want an initial entry: no eligible user has an earlier one",
						e.I, e.Kind, e.DuplicateOf)
				}
			}
			if e.Kind == kindDuplicate {
				lateDups++
				if of := entries[e.DuplicateOf]; of.PlannedNs < 10*sec {
					t.Errorf("entry %d planned at %d ns repeats entry %d (%s) planned at %d ns, whose user is outside its cohort window",
						e.I, e.PlannedNs, of.I, of.User, of.PlannedNs)
				}
			}
		}
		if firstLate < 0 || lateDups == 0 {
			t.Errorf("first entry from 10 s on %d, %d duplicates after it, want an entry and duplicates", firstLate, lateDups)
		}
	})

	t.Run("user states", func(t *testing.T) {
		sc := scenario(t, 47, `
[[steps]]
rate_per_s = 400
duration_s = 30
[users]
count = 40
[users.states.quiet]
weight = 0
mean_dwell_s = 2
[users.states.active]
weight = 1
mean_dwell_s = 1
[users.states.burst]
weight = 4
mean_dwell_s = 0.5
[duplicates]
fraction = 0.2
`)
		entries, _ := build(t, sc)
		// Replay the users' states with a second generator on the same stream.
		m := newMarkov(sc.Seed, *sc.Users.States, sc.Users.Count)
		quietEntries, dups := 0, 0
		for _, e := range entries {
			for m.next() <= e.PlannedNs {
				m.move()
			}
			if m.state[userIndex(t, e.User)] == quiet {
				quietEntries++
			}
			if e.Kind == kindDuplicate {
				dups++
			}
		}
		if quietEntries != 0 || dups == 0 {
			t.Errorf("%d of %d entries (%d duplicates) went to a user that was quiet at its planned time, want 0, and some duplicates",
				quietEntries, len(entries), dups)
		}
	})
}

// userIndex is the index of the user whose ID is id (u%06d).
func userIndex(t *testing.T, id string) int {
	t.Helper()
	var u int
	if _, err := fmt.Sscanf(id, "u%06d", &u); err != nil {
		t.Fatalf("user %q: %v", id, err)
	}
	return u
}

// TestSchedule_IDsValid (plan H2, HTTP-2, HTTP-3): in the written schedule.jsonl every user
// and request ID matches ^[A-Za-z0-9_-]{1,64}$, request IDs are unique among the entries that
// are not duplicates, and each line holds exactly the plan's fields: i, planned_ns, step,
// user, request_id, instance, kind and duplicate_of (null for an initial entry).
func TestSchedule_IDsValid(t *testing.T) {
	entries, _ := build(t, scenario(t, 19, everySource))
	_, data := scheduleHash(t, entries)
	idRe := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	wantKeys := []string{"duplicate_of", "i", "instance", "kind", "planned_ns", "request_id", "step", "user"}
	seen := map[string]int{}
	lines := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var line map[string]any
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		if err := dec.Decode(&line); err != nil {
			t.Fatalf("line %d: %v", lines+1, err)
		}
		keys := make([]string, 0, len(line))
		for k := range line {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Fatalf("line %d has keys %v, want %v", lines+1, keys, wantKeys)
		}
		i, err := line["i"].(json.Number).Int64()
		if err != nil || i != int64(lines) {
			t.Fatalf("line %d: i = %v, want %d", lines+1, line["i"], lines)
		}
		user, _ := line["user"].(string)
		reqID, _ := line["request_id"].(string)
		if !idRe.MatchString(user) || !idRe.MatchString(reqID) {
			t.Errorf("line %d: user %q, request_id %q, want both to match %s", lines+1, user, reqID, idRe)
		}
		switch line["kind"] {
		case kindInitial:
			if line["duplicate_of"] != nil {
				t.Errorf("line %d: an initial entry with duplicate_of %v, want null", lines+1, line["duplicate_of"])
			}
			if want := fmt.Sprintf("r%09d", i); reqID != want {
				t.Errorf("line %d: request_id %q, want %q (from i)", lines+1, reqID, want)
			}
			if prev, dup := seen[reqID]; dup {
				t.Errorf("line %d: request_id %q also on line %d, want unique among initial entries", lines+1, reqID, prev+1)
			}
			seen[reqID] = lines
		case kindDuplicate:
			if _, ok := line["duplicate_of"].(json.Number); !ok {
				t.Errorf("line %d: a duplicate with duplicate_of %v, want an entry number", lines+1, line["duplicate_of"])
			}
		default:
			t.Errorf("line %d: kind %v, want initial or duplicate", lines+1, line["kind"])
		}
		lines++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != len(entries) || len(seen) == 0 {
		t.Errorf("schedule.jsonl has %d lines (%d initial), want %d", lines, len(seen), len(entries))
	}
}

// TestSchedule_UserStates checks the Markov states and the user draw (plan H2 step 2): every
// user starts active, each move goes to one of the two other states with equal chance after an
// exponential dwell with the state's mean (an exponential's coefficient of variation is 1),
// the first one active's; a user that is quiet or outside its cohort window is never drawn; and
// every aggregate arrival's user was active or bursting at its planned time. The state factor
// of the draw weights is TestSchedule_DrawWeights'.
func TestSchedule_UserStates(t *testing.T) {
	states := config.States{
		Quiet:  config.State{Weight: 0, MeanDwellS: 2},
		Active: config.State{Weight: 1, MeanDwellS: 1},
		Burst:  config.State{Weight: 4, MeanDwellS: 0.5},
	}
	t.Run("moves", func(t *testing.T) {
		const users = 50
		m := newMarkov(23, states, users)
		for u := range users {
			if m.state[u] != active {
				t.Fatalf("user %d starts %d, want active", u, m.state[u])
			}
		}
		entered := make([]int64, users) // when each user entered its state
		dwell := map[userState][]float64{}
		pairs := map[[2]userState]int{}
		last := int64(0)
		for m.next() <= 2_000*sec {
			at, u := m.due[0].ns, m.due[0].user
			from := m.state[u]
			if at < last {
				t.Fatalf("a move at %d ns after one at %d ns, want them in time order", at, last)
			}
			last = at
			if mu, to := m.move(); mu != u || to == from {
				t.Fatalf("move of user %d from %d: user %d to %d, want the same user and another state", u, from, mu, to)
			} else {
				pairs[[2]userState{from, to}]++
			}
			dwell[from] = append(dwell[from], float64(at-entered[u])/float64(sec))
			entered[u] = at
		}
		for s, mean := range map[userState]float64{quiet: 2, active: 1, burst: 0.5} {
			sum := 0.0
			for _, d := range dwell[s] {
				sum += d
			}
			n := float64(len(dwell[s]))
			// The mean of n exponential dwells has a standard deviation of mean/√n.
			if got := sum / n; n < 5000 || math.Abs(got-mean) > 5*mean/math.Sqrt(n) {
				t.Errorf("state %d: %v dwells averaging %.4f s, want thousands averaging %.1f s ± 5σ", s, n, got, mean)
			}
		}
		for from := quiet; from <= burst; from++ {
			a, b := pairs[[2]userState{from, (from + 1) % 3}], pairs[[2]userState{from, (from + 2) % 3}]
			// Each move picks one of two states: a ~ Binomial(a+b, 1/2), σ = √(a+b)/2.
			if d := math.Abs(float64(a-b)) / 2; d > 5*math.Sqrt(float64(a+b))/2 {
				t.Errorf("from state %d: %d and %d moves to the two other states, want them equal within 5σ", from, a, b)
			}
		}
		for s := quiet; s <= burst; s++ {
			var sum, sumSq float64
			for _, d := range dwell[s] {
				sum += d
				sumSq += d * d
			}
			n := float64(len(dwell[s]))
			mean := sum / n
			if cv := math.Sqrt(sumSq/n-mean*mean) / mean; !(cv >= 0.95 && cv <= 1.05) {
				t.Errorf("state %d: %v dwells with a coefficient of variation of %.3f, want 0.95 to 1.05 (exponential)", s, n, cv)
			}
		}
		// Every user's first dwell is active's, mean 1 s: over 10,000 users their mean is
		// 1 s ± 5σ, σ = 1 s/√10,000.
		first := newMarkov(41, states, 10_000)
		sum := 0.0
		for _, mv := range first.due {
			sum += float64(mv.ns) / float64(sec)
		}
		if mean := sum / 10_000; math.Abs(mean-1) > 5*1/math.Sqrt(10_000) {
			t.Errorf("10,000 users' first dwells average %.4f s, want active's mean 1 s ± 5σ (0.05 s)", mean)
		}
	})

	t.Run("draws", func(t *testing.T) {
		sc := scenario(t, 29, `
[[steps]]
rate_per_s = 1
duration_s = 1
[users]
count = 20
zipf_s = 0.5
`)
		p := newPicker(sc)
		for _, u := range []int{0, 3, 4, 19} {
			p.setInWindow(u, false)
		}
		p.state[7] = quiet
		p.update(7)
		allowed := map[int]bool{}
		var want int64
		for u := range 20 {
			if u != 0 && u != 3 && u != 4 && u != 19 && u != 7 {
				allowed[u] = true
				want += p.wActive[u]
			}
		}
		if p.total != want {
			t.Fatalf("total weight %d, want %d: the eligible users' active weights", p.total, want)
		}
		rng := rand.New(rand.NewPCG(31, 1))
		got := map[int]int{}
		for range 20_000 {
			u := p.find(rng.Int64N(p.total))
			if !allowed[u] {
				t.Fatalf("drew user %d, which is quiet or outside its window", u)
			}
			got[u]++
		}
		if len(got) != len(allowed) {
			t.Errorf("drew %d distinct users, want all %d eligible ones", len(got), len(allowed))
		}
		// find is exact: target t gives the u with prefix(u) ≤ t < prefix(u+1).
		var prefix int64
		for u := range 20 {
			w := p.weight[u]
			if w > 0 {
				for _, target := range []int64{prefix, prefix + w - 1} {
					if got := p.find(target); got != u {
						t.Errorf("find(%d) = %d, want %d (its weight covers [%d, %d))", target, got, u, prefix, prefix+w)
					}
				}
			}
			prefix += w
		}
	})

	t.Run("arrivals go to active or bursting users", func(t *testing.T) {
		sc := scenario(t, 37, `
[[steps]]
rate_per_s = 400
duration_s = 30
[users]
count = 40
[users.states.quiet]
weight = 0
mean_dwell_s = 2
[users.states.active]
weight = 1
mean_dwell_s = 1
[users.states.burst]
weight = 4
mean_dwell_s = 0.5
`)
		entries, _ := build(t, sc)
		// Replay the users' states with a second generator on the same stream.
		m := newMarkov(sc.Seed, *sc.Users.States, sc.Users.Count)
		quietDraws := 0
		for _, e := range entries {
			for m.next() <= e.PlannedNs {
				m.move()
			}
			var u int
			if _, err := fmt.Sscanf(e.User, "u%06d", &u); err != nil {
				t.Fatal(err)
			}
			if m.state[u] == quiet {
				quietDraws++
			}
		}
		if quietDraws != 0 {
			t.Errorf("%d of %d arrivals went to a user that was quiet at its planned time, want 0", quietDraws, len(entries))
		}
	})
}

// TestSchedule_DrawWeights checks the state factor of the user draw weights (plan H2 step 2)
// on a picker alone, building no schedule: with active weight 1 and burst weight 4, each user's
// burst weight is 4 times its active one, every user starts with its active weight, the heavier
// weights add up to at most totalWeight (so the tree's total cannot overflow), and the Fenwick
// tree finds exactly, before and after a user whose path reaches the tree's last node bursts.
func TestSchedule_DrawWeights(t *testing.T) {
	sc := scenario(t, 29, `
[[steps]]
rate_per_s = 1
duration_s = 1
[users]
count = 20
zipf_s = 0.5
[users.states.quiet]
weight = 0
mean_dwell_s = 2
[users.states.active]
weight = 1
mean_dwell_s = 1
[users.states.burst]
weight = 4
mean_dwell_s = 0.5
`)
	p := newPicker(sc)
	heavier := new(big.Int)
	for u := range 20 {
		if p.weight[u] != p.wActive[u] {
			t.Errorf("user %d starts with weight %d, want its active weight %d", u, p.weight[u], p.wActive[u])
		}
		if r := float64(p.wBurst[u]) / float64(p.wActive[u]); math.Abs(r-4) > 1e-9 {
			t.Errorf("user %d: burst weight %d / active weight %d = %v, want 4", u, p.wBurst[u], p.wActive[u], r)
		}
		heavier.Add(heavier, big.NewInt(max(p.wActive[u], p.wBurst[u])))
	}
	if heavier.Cmp(big.NewInt(totalWeight)) > 0 {
		t.Errorf("the users' heavier weights add up to %v, want at most totalWeight (%d)", heavier, int64(totalWeight))
	}
	findsExact(t, p, "at the start")
	// User 17's path through the Fenwick tree, indexes 18 and 20, reaches its last node.
	p.state[17] = burst
	p.update(17)
	if p.weight[17] != p.wBurst[17] {
		t.Errorf("user 17, bursting, has weight %d in the tree, want its burst weight %d", p.weight[17], p.wBurst[17])
	}
	findsExact(t, p, "once user 17 bursts")
}

// findsExact checks the picker's Fenwick tree against its users' weights: the total is their
// sum, and find maps the first and the last target of each user's share of [0, total) to it.
func findsExact(t *testing.T, p *picker, when string) {
	t.Helper()
	var prefix int64
	for u, w := range p.weight {
		if w > 0 {
			for _, target := range []int64{prefix, prefix + w - 1} {
				if got := p.find(target); got != u {
					t.Errorf("%s: find(%d) = %d, want %d (its weight covers [%d, %d))", when, target, got, u, prefix, prefix+w)
				}
			}
		}
		prefix += w
	}
	if p.total != prefix {
		t.Errorf("%s: total weight %d, want the users' sum %d", when, p.total, prefix)
	}
}

// TestSchedule_Steps checks a run of several steps (plan H2 step 2: each step is a Poisson
// process at its rate that restarts at the step's start; step 3: the step field). 100/s for
// 20 s, 300/s for 20 s, 0/s for 10 s and 200/s for 20 s plan 2,000 ± 179, 6,000 ± 310, no
// and 4,000 ± 253 arrivals (4σ, σ = √(rate × duration)); every entry lies inside its step's
// [start, end), from the steps' literal bounds, and carries that step's index.
func TestSchedule_Steps(t *testing.T) {
	entries, _ := build(t, scenario(t, 43, `
[[steps]]
rate_per_s = 100
duration_s = 20
[[steps]]
rate_per_s = 300
duration_s = 20
[[steps]]
rate_per_s = 0
duration_s = 10
[[steps]]
rate_per_s = 200
duration_s = 20
[users]
count = 10
`))
	bounds := []int64{0, 20 * sec, 40 * sec, 50 * sec, 70 * sec} // step k is [bounds[k], bounds[k+1])
	want := []struct{ n, dev int }{{2000, 179}, {6000, 310}, {0, 0}, {4000, 253}}
	counts := make([]int, len(want))
	for _, e := range entries {
		k := -1
		for j := range want {
			if e.PlannedNs >= bounds[j] && e.PlannedNs < bounds[j+1] {
				k = j
			}
		}
		if k < 0 {
			t.Fatalf("entry %d planned at %d ns, outside the run [0, 70 s)", e.I, e.PlannedNs)
		}
		if e.Step != k {
			t.Fatalf("entry %d planned at %d ns: step %d, want %d ([%d, %d) ns)", e.I, e.PlannedNs, e.Step, k, bounds[k], bounds[k+1])
		}
		counts[k]++
	}
	for k, w := range want {
		if counts[k] < w.n-w.dev || counts[k] > w.n+w.dev {
			t.Errorf("step %d planned %d arrivals, want %d ± %d", k, counts[k], w.n, w.dev)
		}
	}
}

// TestSchedule_MergeOrder (plan H2 step 2: merge all sources by planned offset, ties by source
// order): with steps, cohorts, states, two [[streams]] rows and duplicates, no entry is planned
// before the one ahead of it. On a tie the steps' arrival comes first, then the [[streams]]
// rows in order, a row's users in order: plannedOrder on the builder's own arrivals, given one
// time.
func TestSchedule_MergeOrder(t *testing.T) {
	sc := scenario(t, 53, everySource)
	entries, _ := build(t, sc)
	streamed := 0
	for i, e := range entries {
		if i > 0 && e.PlannedNs < entries[i-1].PlannedNs {
			t.Fatalf("entry %d planned at %d ns, before entry %d at %d ns: want nondecreasing planned_ns",
				i, e.PlannedNs, i-1, entries[i-1].PlannedNs)
		}
		if e.PlannedNs >= 20*sec && e.PlannedNs < 25*sec { // the rate-0 step: streams only
			streamed++
		}
	}
	if streamed == 0 {
		t.Error("no stream entry in the rate-0 step, want the streams merged in")
	}

	b := newBuilder(sc)
	arrivals, _ := b.aggregate()
	arrivals = b.streams(arrivals)
	firstOf := func(what string, match func(a arrival) bool) arrival {
		t.Helper()
		for _, a := range arrivals {
			if match(a) {
				return a
			}
		}
		t.Fatalf("no arrival from %s", what)
		return arrival{}
	}
	step := firstOf("the steps", func(a arrival) bool { return a.aggregate })
	row0 := firstOf("streams[0], user 60", func(a arrival) bool { return !a.aggregate && a.user == 60 })
	row0next := firstOf("streams[0], user 61", func(a arrival) bool { return !a.aggregate && a.user == 61 })
	row1 := firstOf("streams[1]", func(a arrival) bool { return !a.aggregate && a.user == 3 })
	row0.ns, row0next.ns, row1.ns = step.ns, step.ns, step.ns
	tied := []arrival{step, row0, row0next, row1} // in source order
	for i, x := range tied {
		for j, y := range tied {
			if got, want := cmp.Compare(plannedOrder(x, y), 0), cmp.Compare(i, j); got != want {
				t.Errorf("plannedOrder of tied arrivals %d and %d (in source order) has sign %d, want %d", i, j, got, want)
			}
		}
	}
	later := step
	later.ns++
	if plannedOrder(row1, later) >= 0 || plannedOrder(later, row1) <= 0 {
		t.Error("plannedOrder puts a later step arrival before an earlier stream arrival, want planned time first")
	}
}

// TestSchedule_SourceStreams (plan H2 step 2: one rand.New(rand.NewPCG(seed, streamID)) per
// source, with fixed stream IDs; TRF-1: the seed drives every random choice): the stream IDs
// are distinct and below the retry jitter streams (1<<32); two [[streams]] rows at one rate on
// different users draw different times; a step and a stream at one rate on one user plan no
// two arrivals at the same time; and every source changes with the seed: the steps' times, a
// stream's times, the Markov moves and which entries are duplicates.
func TestSchedule_SourceStreams(t *testing.T) {
	ids := []uint64{streamAggregate, streamMarkov, streamDuplicates}
	for r := range uint64(1000) {
		ids = append(ids, streamRowBase+r)
	}
	seen := map[uint64]bool{}
	for _, id := range ids {
		if seen[id] || id >= 1<<32 {
			t.Errorf("stream ID %d is used twice or not below 1<<32, want distinct IDs below the jitter streams", id)
		}
		seen[id] = true
	}
	times := func(entries []Entry, user string) []int64 {
		var ts []int64
		for _, e := range entries {
			if user == "" || e.User == user {
				ts = append(ts, e.PlannedNs)
			}
		}
		return ts
	}

	rows, _ := build(t, scenario(t, 59, `
[[steps]]
rate_per_s = 0
duration_s = 10
[users]
count = 2
[[streams]]
users = [0, 0]
rate_per_s = 50
[[streams]]
users = [1, 1]
rate_per_s = 50
`))
	if a, b := times(rows, "u000000"), times(rows, "u000001"); len(a) == 0 || reflect.DeepEqual(a, b) {
		t.Errorf("two [[streams]] rows at 50/s drew %d and %d times, the same ones, want each row its own generator", len(a), len(b))
	}

	mixed, _ := build(t, scenario(t, 61, `
[[steps]]
rate_per_s = 50
duration_s = 10
[users]
count = 1
[[streams]]
users = [0, 0]
rate_per_s = 50
`))
	for i := 1; i < len(mixed); i++ {
		if mixed[i].PlannedNs == mixed[i-1].PlannedNs {
			t.Fatalf("entries %d and %d are both planned at %d ns: a stream row replays the steps' generator", i-1, i, mixed[i].PlannedNs)
		}
	}

	const stepsOnly = "[[steps]]\nrate_per_s = 50\nduration_s = 10\n[users]\ncount = 5\n"
	const streamsOnly = "[[steps]]\nrate_per_s = 0\nduration_s = 10\n[users]\ncount = 1\n[[streams]]\nusers = [0, 0]\nrate_per_s = 50\n"
	for _, c := range []struct{ what, body string }{{"the steps", stepsOnly}, {"a stream", streamsOnly}} {
		a, _ := build(t, scenario(t, 67, c.body))
		b, _ := build(t, scenario(t, 68, c.body))
		if reflect.DeepEqual(times(a, ""), times(b, "")) {
			t.Errorf("%s: seeds 67 and 68 plan the same %d times, want them to depend on the seed", c.what, len(a))
		}
	}

	states := config.States{
		Quiet:  config.State{Weight: 0, MeanDwellS: 2},
		Active: config.State{Weight: 1, MeanDwellS: 1},
		Burst:  config.State{Weight: 4, MeanDwellS: 0.5},
	}
	firstMoves := func(seed uint64) []int64 {
		m := newMarkov(seed, states, 50)
		at := make([]int64, 50)
		for _, mv := range m.due {
			at[mv.user] = mv.ns
		}
		return at
	}
	if reflect.DeepEqual(firstMoves(71), firstMoves(72)) {
		t.Error("seeds 71 and 72 give the users the same first moves, want the Markov moves to depend on the seed")
	}

	dupAt := func(seed uint64) []bool {
		entries, _ := build(t, scenario(t, seed, stepsOnly+"[duplicates]\nfraction = 0.3\n"))
		kinds := make([]bool, len(entries))
		for i, e := range entries {
			kinds[i] = e.Kind == kindDuplicate
		}
		return kinds
	}
	a, b := dupAt(73), dupAt(74)
	n := min(len(a), len(b))
	if reflect.DeepEqual(a[:n], b[:n]) {
		t.Errorf("seeds 73 and 74 make the same ones of the first %d entries duplicates, want the choice to depend on the seed", n)
	}
}

// TestSchedule_FileRoundTrip (plan H2 step 3): every schedule.jsonl line holds its entry's
// eight values, under the plan's names, with duplicate_of null for an initial entry (-1 in
// Entry); writing a shorter schedule to the same path replaces the file, leaving its lines only.
func TestSchedule_FileRoundTrip(t *testing.T) {
	entries, _ := build(t, scenario(t, 79, everySource))
	path := filepath.Join(t.TempDir(), "schedule.jsonl")
	type fileLine struct {
		I           int    `json:"i"`
		PlannedNs   int64  `json:"planned_ns"`
		Step        int    `json:"step"`
		User        string `json:"user"`
		RequestID   string `json:"request_id"`
		Instance    int    `json:"instance"`
		Kind        string `json:"kind"`
		DuplicateOf *int   `json:"duplicate_of"`
	}
	roundTrip := func(want []Entry) {
		t.Helper()
		if err := writeSchedule(path, want); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(string(data), "\n")
		if last := lines[len(lines)-1]; last != "" {
			t.Fatalf("schedule.jsonl ends with %q, want a newline", last)
		}
		lines = lines[:len(lines)-1]
		if len(lines) != len(want) {
			t.Fatalf("schedule.jsonl has %d lines, want %d, one per entry", len(lines), len(want))
		}
		for i, text := range lines {
			dec := json.NewDecoder(strings.NewReader(text))
			dec.DisallowUnknownFields()
			var l fileLine
			if err := dec.Decode(&l); err != nil {
				t.Fatalf("line %d: %v", i+1, err)
			}
			got := Entry{I: l.I, PlannedNs: l.PlannedNs, Step: l.Step, User: l.User, RequestID: l.RequestID,
				Instance: l.Instance, Kind: l.Kind, DuplicateOf: -1}
			if l.DuplicateOf != nil {
				got.DuplicateOf = *l.DuplicateOf
			}
			if got != want[i] || (l.DuplicateOf != nil) != (want[i].DuplicateOf >= 0) {
				t.Fatalf("line %d is %s, want entry %+v (duplicate_of null when -1)", i+1, strings.TrimSpace(text), want[i])
			}
		}
	}
	roundTrip(entries)
	roundTrip(entries[:100])
}
