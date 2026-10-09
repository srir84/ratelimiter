package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ratelimiter/limiter"
)

// writeScenario writes a scenario file into a fresh temporary directory and returns its path.
func writeScenario(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// fullScenario sets every §B4 scenario field.
const fullScenario = `
name = "full-1"
seed = 42
run_kind = "test"
instances = 3
forced_aof_rewrites_s = [180, 420]
stop_early = true
verdict = "spike"
spike_step = 1

[policy]
clock_source = "controlled"
failure_mode = "open"

[instance]
max_held_receipts = 64

[api]
refuse_after_acquire_bp = 300

[[steps]]
rate_per_s = 100
duration_s = 200
[[steps]]
rate_per_s = 500.5
duration_s = 300
[[steps]]
rate_per_s = 0
duration_s = 100

[users]
count = 1000
zipf_s = 1.2
[users.states.quiet]
weight = 0
mean_dwell_s = 30
[users.states.active]
weight = 1
mean_dwell_s = 20
[users.states.burst]
weight = 5
mean_dwell_s = 2.5

[[streams]]
users = [0, 0]
rate_per_s = 100
[[streams]]
users = [1, 100]
rate_per_s = 5

[[cohorts]]
users = [200, 299]
stop_s = 60
revisit_s = 150
[[cohorts]]
users = [300, 309]
start_s = 30

[duplicates]
fraction = 0.05

[retries]
max_extra = 1
jitter_max_ms = 20

[[faults]]
at_s = 10.5
cmd = '{"target":"store","mode":"drop_reply","match":"refund","count":5}'
instance = 2
[[faults]]
at_s = 20
cmd = "faultctl store pause 4.5s"
`

// scenarioParts are the pieces refusal cases edit: the identity keys, other top-level keys,
// and the tables.
type scenarioParts struct {
	name, seed, runKind, top, steps, users, tables string
}

func (p scenarioParts) String() string {
	return strings.Join([]string{p.name, p.seed, p.runKind, p.top, p.steps, p.users, p.tables}, "\n")
}

// minimalParts is a valid scenario with only the required keys.
func minimalParts() scenarioParts {
	return scenarioParts{
		name:    `name = "minimal"`,
		seed:    "seed = 1",
		runKind: `run_kind = "test"`,
		steps:   "[[steps]]\nrate_per_s = 10\nduration_s = 60",
		users:   "[users]\ncount = 10",
	}
}

func ptr[T any](v T) *T { return &v }

// TestScenario_ParseAndRefuse (plan H2 step 1): every §B4 field parses; the defaults apply
// (instances 3, max_extra 2, jitter_max_ms 50, zipf_s 1.1, verdict none); a [[faults]] entry
// without instance targets every instance; forced_aof_rewrites_s = [180, 420] parses; the
// [policy], [instance] and [api] overrides are kept as TOML that Merge lays over the harness
// config, whether written under table headers or with dotted keys, and one that is not a table
// is refused naming its TOML type; and each refusal rule gives an error naming its field.
func TestScenario_ParseAndRefuse(t *testing.T) {
	t.Run("every field", func(t *testing.T) {
		sc, err := LoadScenario(writeScenario(t, fullScenario))
		if err != nil {
			t.Fatal(err)
		}
		want := Scenario{
			Name: "full-1", Seed: 42, RunKind: RunTest, Instances: 3,
			Overrides: sc.Overrides, // checked through Merge below
			Steps:     []Step{{RatePerS: 100, DurationS: 200}, {RatePerS: 500.5, DurationS: 300}, {RatePerS: 0, DurationS: 100}},
			Users: Users{Count: 1000, ZipfS: 1.2, States: &States{
				Quiet:  State{Weight: 0, MeanDwellS: 30},
				Active: State{Weight: 1, MeanDwellS: 20},
				Burst:  State{Weight: 5, MeanDwellS: 2.5},
			}},
			Streams: []Stream{{Users: Range{0, 0}, RatePerS: 100}, {Users: Range{1, 100}, RatePerS: 5}},
			Cohorts: []Cohort{
				{Users: Range{200, 299}, StartS: 0, StopS: ptr(60.0), RevisitS: ptr(150.0)},
				{Users: Range{300, 309}, StartS: 30},
			},
			Duplicates: Duplicates{Fraction: 0.05},
			Retries:    Retries{MaxExtra: 1, JitterMaxMs: 20},
			Faults: []Fault{
				{AtS: 10.5, Cmd: `{"target":"store","mode":"drop_reply","match":"refund","count":5}`, Targets: []int{2}},
				{AtS: 20, Cmd: "faultctl store pause 4.5s", Targets: []int{1, 2, 3}},
			},
			ForcedAOFRewritesS: []float64{180, 420},
			StopEarly:          true,
			Verdict:            VerdictSpike,
			SpikeStep:          ptr(1),
		}
		if !reflect.DeepEqual(sc, want) {
			t.Errorf("LoadScenario =\n%+v\nwant\n%+v", sc, want)
		}
		if sc.Faults[0].Faultctl() || !sc.Faults[1].Faultctl() {
			t.Errorf("Faultctl() = %v, %v: want false for the JSON body, true for the faultctl command",
				sc.Faults[0].Faultctl(), sc.Faults[1].Faultctl())
		}
		if got := sc.RunNs(); got != 600_000_000_000 {
			t.Errorf("RunNs() = %d, want 600 s", got)
		}

		t.Setenv(LibPasswordEnv, "")
		merged, err := Merge([]byte(b2Policy+b2Instance+storeAndAPI), sc.Overrides)
		if err != nil {
			t.Fatalf("Merge(§B2 file, overrides): %v\noverrides:\n%s", err, sc.Overrides)
		}
		f := load(t, string(merged))
		wantFile := b2File()
		wantFile.Policy.ClockSource, wantFile.Policy.FailureMode = limiter.Controlled, limiter.Open
		wantFile.Instance.MaxHeldReceipts = 64
		wantFile.API.RefuseAfterAcquireBP = 300
		if !reflect.DeepEqual(f, wantFile) {
			t.Errorf("merged config =\n%+v\nwant the §B2 file with the 4 overrides\n%+v", f, wantFile)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		p := minimalParts()
		p.tables = "[[faults]]\nat_s = 1\ncmd = \"faultctl api kill 2 --restart\""
		sc, err := LoadScenario(writeScenario(t, p.String()))
		if err != nil {
			t.Fatal(err)
		}
		want := Scenario{
			Name: "minimal", Seed: 1, RunKind: RunTest, Instances: 3,
			Steps:   []Step{{RatePerS: 10, DurationS: 60}},
			Users:   Users{Count: 10, ZipfS: 1.1},
			Retries: Retries{MaxExtra: 2, JitterMaxMs: 50},
			Faults:  []Fault{{AtS: 1, Cmd: "faultctl api kill 2 --restart", Targets: []int{1, 2, 3}}},
			Verdict: VerdictNone,
		}
		if !reflect.DeepEqual(sc, want) {
			t.Errorf("LoadScenario =\n%+v\nwant the defaults\n%+v", sc, want)
		}

		p.top = "instances = 1"
		sc, err = LoadScenario(writeScenario(t, p.String()))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(sc.Faults[0].Targets, []int{1}) {
			t.Errorf("a fault without instance in a 1-instance scenario targets %v, want [1]", sc.Faults[0].Targets)
		}

		// rate_per_s = 0 is allowed (plan choice): a step that lasts but plans no arrivals.
		p = minimalParts()
		p.steps = "[[steps]]\nrate_per_s = 0\nduration_s = 2"
		if _, err := LoadScenario(writeScenario(t, p.String())); err != nil {
			t.Errorf("a step with rate_per_s = 0: %v, want it accepted", err)
		}
	})

	t.Run("dotted-key overrides", func(t *testing.T) {
		// A table written with dotted keys is a table: the overrides are the ones the same
		// keys give under [policy], [instance] and [api] headers.
		dotted := minimalParts()
		dotted.top = "policy.failure_mode = \"open\"\ninstance.max_held_receipts = 64\napi.refuse_after_acquire_bp = 100"
		sc, err := LoadScenario(writeScenario(t, dotted.String()))
		if err != nil {
			t.Fatalf("overrides written with dotted keys: %v, want them accepted", err)
		}
		headers := minimalParts()
		headers.tables = "[policy]\nfailure_mode = \"open\"\n[instance]\nmax_held_receipts = 64\n[api]\nrefuse_after_acquire_bp = 100"
		under, err := LoadScenario(writeScenario(t, headers.String()))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sc.Overrides, under.Overrides) {
			t.Errorf("dotted-key overrides kept as\n%s\nwant the same as under table headers\n%s", sc.Overrides, under.Overrides)
		}
		t.Setenv(LibPasswordEnv, "")
		merged, err := Merge([]byte(b2Policy+b2Instance+storeAndAPI), sc.Overrides)
		if err != nil {
			t.Fatalf("Merge(§B2 file, overrides): %v\noverrides:\n%s", err, sc.Overrides)
		}
		f := load(t, string(merged))
		wantFile := b2File()
		wantFile.Policy.FailureMode = limiter.Open
		wantFile.Instance.MaxHeldReceipts = 64
		wantFile.API.RefuseAfterAcquireBP = 100
		if !reflect.DeepEqual(f, wantFile) {
			t.Errorf("merged config =\n%+v\nwant the §B2 file with the 3 overrides\n%+v", f, wantFile)
		}
	})

	t.Run("overrides that are not tables", func(t *testing.T) {
		// Each is refused with an error naming the table and the value's TOML type.
		for _, c := range []struct{ name, line, typ string }{
			{"policy", "policy = 5", "integer"},
			{"policy", "policy = 1.5", "float"},
			{"instance", `instance = "x"`, "string"},
			{"api", "api = true", "boolean"},
			{"api", "api = [1, 2]", "array"},
			{"policy", "policy = 2026-10-08T00:00:00Z", "datetime"},
		} {
			p := minimalParts()
			p.top = c.line
			_, err := LoadScenario(writeScenario(t, p.String()))
			if want := c.name + ": a TOML " + c.typ + ","; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %v, want one containing %q", c.line, err, want)
			}
		}
		p := minimalParts()
		p.tables = "[[policy]]\nfailure_mode = \"open\""
		_, err := LoadScenario(writeScenario(t, p.String()))
		if want := "policy: a TOML array of tables,"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("[[policy]]: error %v, want one containing %q", err, want)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		for _, c := range []struct {
			what  string
			edit  func(p *scenarioParts)
			field string // the error names it
		}{
			// The refusal rules of plan H2 step 1.
			{"missing seed", func(p *scenarioParts) { p.seed = "" }, "seed"},
			{"missing name", func(p *scenarioParts) { p.name = "" }, "name"},
			{"missing run_kind", func(p *scenarioParts) { p.runKind = "" }, "run_kind"},
			{"run_kind prod", func(p *scenarioParts) { p.runKind = `run_kind = "prod"` }, "run_kind"},
			{"run_kind Bench", func(p *scenarioParts) { p.runKind = `run_kind = "Bench"` }, "run_kind"},
			{"instances 2", func(p *scenarioParts) { p.top = "instances = 2" }, "instances"},
			{"instances 0", func(p *scenarioParts) { p.top = "instances = 0" }, "instances"},
			{"instances 4", func(p *scenarioParts) { p.top = "instances = 4" }, "instances"},
			{"no steps", func(p *scenarioParts) { p.steps = "" }, "steps"},
			{"max_extra 3", func(p *scenarioParts) { p.tables = "[retries]\nmax_extra = 3" }, "retries.max_extra"},
			{"fault instance 4 of 3", func(p *scenarioParts) {
				p.tables = "[[faults]]\nat_s = 1\ncmd = \"faultctl store pause 1s\"\ninstance = 4"
			}, "faults[0].instance"},
			{"fault instance 0", func(p *scenarioParts) {
				p.tables = "[[faults]]\nat_s = 1\ncmd = \"faultctl store pause 1s\"\ninstance = 0"
			}, "faults[0].instance"},
			{"fault instance 2 of 1", func(p *scenarioParts) {
				p.top = "instances = 1"
				p.tables = "[[faults]]\nat_s = 1\ncmd = \"faultctl store pause 1s\"\n" +
					"[[faults]]\nat_s = 2\ncmd = \"faultctl store pause 1s\"\ninstance = 2"
			}, "faults[1].instance"},
			{"cmd faultctl alone", func(p *scenarioParts) { p.tables = "[[faults]]\nat_s = 1\ncmd = \"faultctl\"" }, "faults[0].cmd"},
			{"cmd faultctl and spaces", func(p *scenarioParts) { p.tables = "[[faults]]\nat_s = 1\ncmd = \"faultctl   \"" }, "faults[0].cmd"},
			{"cmd another command", func(p *scenarioParts) { p.tables = "[[faults]]\nat_s = 1\ncmd = \"kill -9 1234\"" }, "faults[0].cmd"},
			{"cmd JSON array", func(p *scenarioParts) { p.tables = "[[faults]]\nat_s = 1\ncmd = '[1, 2]'" }, "faults[0].cmd"},
			{"cmd JSON null", func(p *scenarioParts) { p.tables = "[[faults]]\nat_s = 1\ncmd = 'null'" }, "faults[0].cmd"},
			{"cmd broken JSON", func(p *scenarioParts) { p.tables = "[[faults]]\nat_s = 1\ncmd = '{\"target\": '" }, "faults[0].cmd"},
			{"cmd two JSON objects", func(p *scenarioParts) { p.tables = "[[faults]]\nat_s = 1\ncmd = '{\"a\":1} {\"b\":2}'" }, "faults[0].cmd"},
			// Every other field is validated too (AGENTS.md §1 Input; plan choices).
			{"negative seed", func(p *scenarioParts) { p.seed = "seed = -1" }, "seed"},
			{"name with a slash", func(p *scenarioParts) { p.name = `name = "a/b"` }, "name"},
			{"unknown key", func(p *scenarioParts) { p.top = "colour = 1" }, "colour"},
			{"misspelled key", func(p *scenarioParts) { p.seed = "Seed = 1" }, "Seed"},
			{"unknown table key", func(p *scenarioParts) { p.tables = "[retries]\nmax_extras = 1" }, "retries.max_extras"},
			{"store override", func(p *scenarioParts) { p.tables = "[store]\naddr = \"127.0.0.1:1\"" }, "store"},
			{"unknown policy key", func(p *scenarioParts) { p.tables = "[policy]\nbogus = 1" }, "policy.bogus"},
			{"bad clock_source", func(p *scenarioParts) { p.tables = "[policy]\nclock_source = \"wall\"" }, "clock_source"},
			{"policy not a table", func(p *scenarioParts) { p.top = "policy = 5" }, "policy"},
			{"negative rate", func(p *scenarioParts) { p.steps = "[[steps]]\nrate_per_s = -1\nduration_s = 1" }, "steps[0].rate_per_s"},
			{"nan rate", func(p *scenarioParts) { p.steps = "[[steps]]\nrate_per_s = nan\nduration_s = 1" }, "steps[0].rate_per_s"},
			{"zero duration", func(p *scenarioParts) { p.steps = "[[steps]]\nrate_per_s = 1\nduration_s = 0" }, "steps[0].duration_s"},
			{"missing duration", func(p *scenarioParts) { p.steps = "[[steps]]\nrate_per_s = 1" }, "steps[0].duration_s"},
			{"run over a day", func(p *scenarioParts) { p.steps = "[[steps]]\nrate_per_s = 0\nduration_s = 86401" }, "steps"},
			{"too many arrivals", func(p *scenarioParts) { p.steps = "[[steps]]\nrate_per_s = 1e6\nduration_s = 60" }, "planned arrivals"},
			{"missing users", func(p *scenarioParts) { p.users = "" }, "users.count"},
			{"no users", func(p *scenarioParts) { p.users = "[users]\ncount = 0" }, "users.count"},
			{"too many users", func(p *scenarioParts) { p.users = "[users]\ncount = 1000001" }, "users.count"},
			{"negative zipf_s", func(p *scenarioParts) { p.users = "[users]\ncount = 10\nzipf_s = -1" }, "users.zipf_s"},
			{"quiet weight 1", func(p *scenarioParts) {
				p.users = "[users]\ncount = 10\n" + states("1", "1", "5")
			}, "users.states.quiet.weight"},
			{"active weight 0", func(p *scenarioParts) {
				p.users = "[users]\ncount = 10\n" + states("0", "0", "5")
			}, "users.states.active.weight"},
			{"no burst state", func(p *scenarioParts) {
				p.users = "[users]\ncount = 10\n[users.states.quiet]\nweight = 0\nmean_dwell_s = 1\n" +
					"[users.states.active]\nweight = 1\nmean_dwell_s = 1"
			}, "users.states.burst"},
			{"unknown state", func(p *scenarioParts) {
				p.users = "[users]\ncount = 10\n" + states("0", "1", "5") + "\n[users.states.idle]\nweight = 0\nmean_dwell_s = 1"
			}, "users.states.idle"},
			{"stream past the users", func(p *scenarioParts) { p.tables = "[[streams]]\nusers = [5, 10]\nrate_per_s = 1" }, "streams[0].users"},
			{"stream with one index", func(p *scenarioParts) { p.tables = "[[streams]]\nusers = [5]\nrate_per_s = 1" }, "streams[0].users"},
			{"stream rate 0", func(p *scenarioParts) { p.tables = "[[streams]]\nusers = [5, 6]\nrate_per_s = 0" }, "streams[0].rate_per_s"},
			{"overlapping cohorts", func(p *scenarioParts) {
				p.tables = "[[cohorts]]\nusers = [0, 4]\n[[cohorts]]\nusers = [4, 9]"
			}, "cohorts[1].users"},
			{"cohort stops before it starts", func(p *scenarioParts) {
				p.tables = "[[cohorts]]\nusers = [0, 4]\nstart_s = 10\nstop_s = 5"
			}, "cohorts[0].stop_s"},
			{"cohort revisits without a stop", func(p *scenarioParts) {
				p.tables = "[[cohorts]]\nusers = [0, 4]\nrevisit_s = 5"
			}, "cohorts[0].revisit_s"},
			{"cohort revisits before its stop", func(p *scenarioParts) {
				p.tables = "[[cohorts]]\nusers = [0, 4]\nstop_s = 10\nrevisit_s = 10"
			}, "cohorts[0].revisit_s"},
			{"duplicates fraction 1", func(p *scenarioParts) { p.tables = "[duplicates]\nfraction = 1" }, "duplicates.fraction"},
			{"duplicates without fraction", func(p *scenarioParts) { p.tables = "[duplicates]" }, "duplicates.fraction"},
			{"negative max_extra", func(p *scenarioParts) { p.tables = "[retries]\nmax_extra = -1" }, "retries.max_extra"},
			{"negative jitter", func(p *scenarioParts) { p.tables = "[retries]\njitter_max_ms = -1" }, "retries.jitter_max_ms"},
			{"fault after the run", func(p *scenarioParts) { p.tables = "[[faults]]\nat_s = 60\ncmd = \"faultctl store pause 1s\"" }, "faults[0].at_s"},
			{"fault without cmd", func(p *scenarioParts) { p.tables = "[[faults]]\nat_s = 1" }, "faults[0].cmd"},
			{"rewrites out of order", func(p *scenarioParts) { p.top = "forced_aof_rewrites_s = [40, 20]" }, "forced_aof_rewrites_s[1]"},
			{"rewrite after the run", func(p *scenarioParts) { p.top = "forced_aof_rewrites_s = [60]" }, "forced_aof_rewrites_s[0]"},
			{"unknown verdict", func(p *scenarioParts) { p.top = `verdict = "maybe"` }, "verdict"},
			{"spike without spike_step", func(p *scenarioParts) { p.top = `verdict = "spike"` }, "spike_step"},
			{"spike_step on the last step", func(p *scenarioParts) { p.top = "verdict = \"spike\"\nspike_step = 0" }, "spike_step"},
			{"spike_step without a spike", func(p *scenarioParts) { p.top = "verdict = \"capacity\"\nspike_step = 0" }, "spike_step"},
			// The other side of each bound, and the keys a table needs.
			{"stream from index -1", func(p *scenarioParts) { p.tables = "[[streams]]\nusers = [-1, 2]\nrate_per_s = 1" }, "streams[0].users"},
			{"cohort range backwards", func(p *scenarioParts) { p.tables = "[[cohorts]]\nusers = [5, 3]" }, "cohorts[0].users"},
			{"too many stream arrivals", func(p *scenarioParts) {
				p.users = "[users]\ncount = 1000000"
				p.tables = "[[streams]]\nusers = [0, 999999]\nrate_per_s = 100"
			}, "planned arrivals"},
			{"spike_step -1", func(p *scenarioParts) {
				p.steps = "[[steps]]\nrate_per_s = 10\nduration_s = 30\n[[steps]]\nrate_per_s = 10\nduration_s = 30"
				p.top = "verdict = \"spike\"\nspike_step = -1"
			}, "spike_step"},
			{"fault before the run", func(p *scenarioParts) { p.tables = "[[faults]]\nat_s = -1\ncmd = \"faultctl store pause 1s\"" }, "faults[0].at_s"},
			{"rewrites at one offset", func(p *scenarioParts) { p.top = "forced_aof_rewrites_s = [20, 20]" }, "forced_aof_rewrites_s[1]"},
			{"cohort stops as it starts", func(p *scenarioParts) {
				p.tables = "[[cohorts]]\nusers = [0, 4]\nstart_s = 5\nstop_s = 5"
			}, "cohorts[0].stop_s"},
			{"cohort starts before the run", func(p *scenarioParts) { p.tables = "[[cohorts]]\nusers = [0, 4]\nstart_s = -1" }, "cohorts[0].start_s"},
			{"negative duplicates fraction", func(p *scenarioParts) { p.tables = "[duplicates]\nfraction = -0.1" }, "duplicates.fraction"},
			{"jitter over a minute", func(p *scenarioParts) { p.tables = "[retries]\njitter_max_ms = 60001" }, "retries.jitter_max_ms"},
			{"65-character name", func(p *scenarioParts) { p.name = `name = "` + strings.Repeat("n", 65) + `"` }, "name"},
			{"too many state moves", func(p *scenarioParts) {
				// 1,000 users × 60 s ÷ 0.0001 s = 6e8 moves.
				p.users = "[users]\ncount = 1000\n[users.states.quiet]\nweight = 0\nmean_dwell_s = 1\n" +
					"[users.states.active]\nweight = 1\nmean_dwell_s = 1\n[users.states.burst]\nweight = 5\nmean_dwell_s = 0.0001"
			}, "users.states"},
			{"quiet without weight", func(p *scenarioParts) {
				p.users = "[users]\ncount = 10\n[users.states.quiet]\nmean_dwell_s = 1\n" +
					"[users.states.active]\nweight = 1\nmean_dwell_s = 1\n[users.states.burst]\nweight = 5\nmean_dwell_s = 1"
			}, "users.states.quiet.weight"},
			{"fault without at_s", func(p *scenarioParts) { p.tables = "[[faults]]\ncmd = \"faultctl store pause 1s\"" }, "faults[0].at_s"},
			{"step without rate_per_s", func(p *scenarioParts) { p.steps = "[[steps]]\nduration_s = 60" }, "steps[0].rate_per_s"},
		} {
			p := minimalParts()
			c.edit(&p)
			sc, err := LoadScenario(writeScenario(t, p.String()))
			if err == nil {
				t.Errorf("%s: LoadScenario succeeded (%+v), want an error naming %s\nfile:\n%s", c.what, sc, c.field, p)
				continue
			}
			if !strings.Contains(err.Error(), c.field) {
				t.Errorf("%s: error %q does not name %s", c.what, err, c.field)
			}
		}
	})
}

// states is a [users.states] table with the given weights and a 1 s mean dwell each.
func states(quiet, active, burst string) string {
	return "[users.states.quiet]\nweight = " + quiet + "\nmean_dwell_s = 1\n" +
		"[users.states.active]\nweight = " + active + "\nmean_dwell_s = 1\n" +
		"[users.states.burst]\nweight = " + burst + "\nmean_dwell_s = 1"
}

// TestScenario_OverridesKey: OverridesKey tells whether the scenario sets a key of an
// override table, whatever its value (IMPLEMENTATION_PLAN H3 step 1: a benchmark run refuses
// a clock_source override, even "store").
func TestScenario_OverridesKey(t *testing.T) {
	head := "name = \"ok\"\nseed = 1\nrun_kind = \"test\"\n"
	tail := "[[steps]]\nrate_per_s = 1\nduration_s = 1\n[users]\ncount = 1\n"
	for _, c := range []struct {
		name, overrides string
		want            bool
	}{
		{"no overrides", "", false},
		{"clock_source store", "[policy]\nclock_source = \"store\"\n", true},
		{"clock_source controlled", "[policy]\nclock_source = \"controlled\"\n", true},
		{"dotted key", "policy.clock_source = \"store\"\n", true},
		{"another policy key", "[policy]\nfailure_mode = \"open\"\n", false},
		{"another table", "[api]\nrefuse_after_acquire_bp = 1\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			content := head + c.overrides + tail // the override tables, or a dotted key, before [[steps]]
			sc, err := LoadScenario(writeScenario(t, content))
			if err != nil {
				t.Fatal(err)
			}
			got, err := sc.OverridesKey("policy", "clock_source")
			if err != nil || got != c.want {
				t.Errorf("OverridesKey(policy, clock_source) = %v, %v; want %v, nil\nfile:\n%s", got, err, c.want, content)
			}
		})
	}
	bad := Scenario{Overrides: []byte("[policy\n")}
	if _, err := bad.OverridesKey("policy", "clock_source"); err == nil {
		t.Error("OverridesKey of overrides that are not TOML: no error")
	}
}
