package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"ratelimiter/harness/config"
)

// The run's outcome in context.json (plan choice).
const (
	outcomeOK      = "ok"
	outcomeFailed  = "failed"
	outcomeRefused = "refused"
)

// The run kinds' labels in context.json (DESIGN §B5 "Two run kinds"; plan H3 step 5).
const (
	labelTest  = "test-build scenario, not a benchmark"
	labelBench = "benchmark"
)

const (
	// maxConformanceSize bounds the .data/conformance.json a run records, in bytes.
	maxConformanceSize = 1 << 20
	// maxInstanceConfigSize bounds an --instance-config file the run copies, in bytes.
	maxInstanceConfigSize = 1 << 20
)

// The macOS programs that read the hardware and the OS (plan H3 step 5), run by absolute path:
// a PATH without /usr/sbin, as cron and launchd give, would not find sysctl.
const (
	sysctlProgram = "/usr/sbin/sysctl"
	swVersProgram = "/usr/bin/sw_vers"
)

// postgresSettingKeys are the PostgreSQL settings context.json records (DESIGN §B10; plan H3
// step 5).
func postgresSettingKeys() []string {
	return []string{"max_connections", "statement_timeout", "fsync", "synchronous_commit"}
}

// runContext is context.json (DESIGN §B8; HARNESS PRF-5; IMPLEMENTATION_PLAN H3 step 5; plan
// choice for the field names). Times are wall-clock ns. It never holds a password: settings
// come from config.toml, whose format has no field for one, and no environment is recorded.
type runContext struct {
	RunID             string             `json:"run_id"`
	EpochID           string             `json:"epoch_id"`
	RunKind           string             `json:"run_kind"`
	RunLabel          string             `json:"run_label"`
	Baseline          bool               `json:"baseline"`
	Outcome           string             `json:"outcome"`
	Error             string             `json:"error,omitempty"`
	StartNs           int64              `json:"start_ns"`
	EndNs             int64              `json:"end_ns"`
	Scenario          scenarioInfo       `json:"scenario"`
	Seed              uint64             `json:"seed"`
	Instances         int                `json:"instances"`
	Hardware          *hardwareInfo      `json:"hardware"`
	OS                map[string]string  `json:"os"`
	Versions          versionsInfo       `json:"versions"`
	StoreSettings     map[string]string  `json:"store_settings"`
	PostgresSettings  map[string]string  `json:"postgres_settings"`
	Conformance       json.RawMessage    `json:"conformance"`
	Settings          map[string]any     `json:"settings"`
	InstanceConfigs   []instanceConfInfo `json:"instance_configs"`
	ControlledClockNs *int64             `json:"controlled_clock_ns"`
	ScheduleSHA256    *string            `json:"schedule_sha256"`
	ThresholdsSHA256  *string            `json:"thresholds_sha256"`
	Faults            []faultRecord      `json:"faults"`
	Rewrites          []rewriteRecord    `json:"rewrites"`
	Children          []childRecord      `json:"children"`
	ReadErrors        map[string]string  `json:"read_errors,omitempty"`
}

// scenarioInfo names the scenario file a run came from.
type scenarioInfo struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// hardwareInfo is `sysctl hw.model hw.ncpu hw.memsize`.
type hardwareInfo struct {
	Model        string `json:"model"`
	NCPU         int64  `json:"ncpu"`
	MemsizeBytes int64  `json:"memsize_bytes"`
}

// versionsInfo are the programs' versions: the Go toolchain that built the orchestrator (make
// build builds every binary with it), and the first lines of `valkey-server --version` and
// `postgres --version`.
type versionsInfo struct {
	Go         string `json:"go"`
	Valkey     string `json:"valkey"`
	ValkeyPath string `json:"valkey_path"`
	Postgres   string `json:"postgres"`
}

// instanceConfInfo is an --instance-config file (plan H3 step 6): where it came from, its copy
// in the run directory, its SHA-256 and its settings.
type instanceConfInfo struct {
	Instance int            `json:"instance"`
	Source   string         `json:"source"`
	File     string         `json:"file"`
	SHA256   string         `json:"sha256"`
	Settings map[string]any `json:"settings"`
}

// newRunContext is context.json's start: what preflight and the run directory fixed.
func newRunContext(in inputs, rd runDir) runContext {
	rc := runContext{
		RunID: rd.id, RunKind: in.scenario.RunKind, RunLabel: labelBench, Baseline: in.baseline,
		Scenario:  scenarioInfo{Name: in.scenario.Name, Path: in.scenarioPath},
		Seed:      in.scenario.Seed,
		Instances: in.scenario.Instances,
		Versions:  versionsInfo{Go: runtime.Version()},
	}
	if in.scenario.RunKind == config.RunTest {
		rc.RunLabel = labelTest
	}
	if rd.thresholdsSHA256 != "" {
		sum := rd.thresholdsSHA256
		rc.ThresholdsSHA256 = &sum
	}
	return rc
}

// readErr records that a read for context.json failed; the run goes on without it, and a
// benchmark run then fails at its end (missingPRF5).
func (r *runner) readErr(what string, err error) {
	if r.rec.ReadErrors == nil {
		r.rec.ReadErrors = map[string]string{}
	}
	r.rec.ReadErrors[what] = err.Error()
	r.logger.Warn("context.json: a read failed", "what", what, "error", err)
}

// gatherHost fills context.json's host part (plan H3 step 5): the hardware, the OS, the
// versions, the scenario file's hash, the run's settings and .data/conformance.json. A read
// that fails is recorded in read_errors, as is a conformance result without its two fsync
// measurements (fsyncGaps), which context.json still records.
func (r *runner) gatherHost(ctx context.Context) {
	if sum, err := fileSHA256(r.in.scenarioPath); err != nil {
		r.readErr("scenario_sha256", err)
	} else {
		r.rec.Scenario.SHA256 = sum
	}
	if m, err := settingsMap(r.in.config); err != nil {
		r.readErr("settings", err)
	} else {
		r.rec.Settings = m
	}
	if hw, err := r.hardware(ctx); err != nil {
		r.readErr("hardware", err)
	} else {
		r.rec.Hardware = hw
	}
	if m, err := r.osInfo(ctx); err != nil {
		r.readErr("os", err)
	} else {
		r.rec.OS = m
	}
	for _, v := range []struct {
		what string
		dst  *string
		prog string
	}{
		{"valkey_version", &r.rec.Versions.Valkey, r.rec.Versions.ValkeyPath},
		{"postgres_version", &r.rec.Versions.Postgres, "postgres"},
	} {
		if line, err := r.firstLine(ctx, v.prog, "--version"); err != nil {
			r.readErr(v.what, err)
		} else {
			*v.dst = line
		}
	}
	path := r.s.path(conformanceFile)
	if raw, err := readConformance(path); err != nil {
		r.readErr("conformance", err)
	} else {
		r.rec.Conformance = raw
		if gaps := fsyncGaps(raw); len(gaps) > 0 {
			r.readErr("conformance", fmt.Errorf("%s: no finite number > 0 at %s", path, strings.Join(gaps, ", ")))
		}
	}
}

// hardware runs `sysctl hw.model hw.ncpu hw.memsize`.
func (r *runner) hardware(ctx context.Context) (*hardwareInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	out, err := runCommand(ctx, "", r.envs.plain, nil, sysctlProgram, "hw.model", "hw.ncpu", "hw.memsize")
	if err != nil {
		return nil, err
	}
	m := colonFields(string(out))
	hw := &hardwareInfo{Model: m["hw.model"]}
	if hw.NCPU, err = strconv.ParseInt(m["hw.ncpu"], 10, 64); err != nil {
		return nil, fmt.Errorf("sysctl hw.ncpu: %w", err)
	}
	if hw.MemsizeBytes, err = strconv.ParseInt(m["hw.memsize"], 10, 64); err != nil {
		return nil, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	if hw.Model == "" {
		return nil, errors.New("sysctl printed no hw.model")
	}
	return hw, nil
}

// osInfo runs sw_vers: ProductName, ProductVersion and BuildVersion.
func (r *runner) osInfo(ctx context.Context) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	out, err := runCommand(ctx, "", r.envs.plain, nil, swVersProgram)
	if err != nil {
		return nil, err
	}
	m := colonFields(string(out))
	if m["ProductVersion"] == "" {
		return nil, fmt.Errorf("sw_vers printed no ProductVersion: %.200q", out)
	}
	return m, nil
}

// missingPRF5 names the context.json items of plan H3 step 5 (HARNESS PRF-5) the run has not
// recorded, as context.json names them: the hardware, the OS, the versions, the store and
// PostgreSQL settings, the conformance result and its two fsync measurements (fsyncGaps), the
// merged settings, the schedule hash, and in a limiter run the frozen thresholds' hash. A
// benchmark run that lacks one fails at its end (finishRun); a test run goes on, with the
// failed read in read_errors.
func (r *runner) missingPRF5() []string {
	var missing []string
	for _, item := range []struct {
		name    string
		missing bool
	}{
		{"hardware", r.rec.Hardware == nil},
		{"os", len(r.rec.OS) == 0},
		{"versions.go", r.rec.Versions.Go == ""},
		{"versions.valkey", r.rec.Versions.Valkey == ""},
		{"versions.postgres", r.rec.Versions.Postgres == ""},
		{"store_settings", len(r.rec.StoreSettings) == 0},
		{"postgres_settings", len(r.rec.PostgresSettings) == 0},
		{"conformance", len(r.rec.Conformance) == 0},
		{"settings", len(r.rec.Settings) == 0},
		{"schedule_sha256", r.rec.ScheduleSHA256 == nil},
		{"thresholds_sha256", r.in.settings.API.LimiterEnabled && r.rec.ThresholdsSHA256 == nil},
	} {
		if item.missing {
			missing = append(missing, item.name)
		}
	}
	if len(r.rec.Conformance) > 0 {
		for _, gap := range fsyncGaps(r.rec.Conformance) {
			missing = append(missing, "conformance."+gap)
		}
	}
	return missing
}

// fsyncGaps returns the fsync measurements a conformance result must carry into context.json
// (DESIGN §B8: the F_FULLFSYNC floor and the durable single-client script p50) that raw lacks,
// as S1's .data/conformance.json names them (IMPLEMENTATION_PLAN S1 step 6; µs):
// fsync.f_fullfsync_floor_p50_us and fsync.durable_script_p50_us must each be a finite number
// > 0. Absent, null (which decodes as no change, leaving 0), not a number, or a number no
// float64 holds (1e999, which the decoder refuses) is a gap.
func fsyncGaps(raw json.RawMessage) []string {
	var doc struct {
		Fsync map[string]json.RawMessage `json:"fsync"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		doc.Fsync = nil // fsync is not an object: neither measurement is there
	}
	var gaps []string
	for _, key := range []string{"f_fullfsync_floor_p50_us", "durable_script_p50_us"} {
		var us float64
		if json.Unmarshal(doc.Fsync[key], &us) != nil || us <= 0 {
			gaps = append(gaps, "fsync."+key)
		}
	}
	return gaps
}

// firstLine runs prog with args and returns the first line it printed.
func (r *runner) firstLine(ctx context.Context, prog string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	out, err := runCommand(ctx, "", r.envs.plain, nil, prog, args...)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if line == "" {
		return "", fmt.Errorf("%s printed nothing", prog)
	}
	return line, nil
}

// colonFields reads "name: value" lines (sysctl, sw_vers) into a map.
func colonFields(text string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// settingsMap decodes a config file into the map context.json records: its TOML tables and
// keys as they are, without a password (the format has none).
func settingsMap(data []byte) (map[string]any, error) {
	m := map[string]any{}
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding the config: %w", err)
	}
	return m, nil
}

// readConformance reads .data/conformance.json (the latest §C2 result: the F_FULLFSYNC floor
// and the durable single-client script p50 among its fields) as one JSON object, compacted.
func readConformance(path string) (json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConformanceSize+1))
	_ = f.Close() // read-only: the bytes read are what counts
	if err != nil {
		return nil, err
	}
	if len(data) > maxConformanceSize {
		return nil, fmt.Errorf("%s: more than %d bytes", path, maxConformanceSize)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("%s: not a JSON object", path)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return buf.Bytes(), nil
}

// copyInstanceConfig copies the --instance-config file src for instance n into the run
// directory as instance-N.toml (mode 0600; plan choice), so the run keeps the file the
// instance read, and returns its record and settings. Preflight loaded it once already.
func copyInstanceConfig(src, runPath string, n int) (instanceConfInfo, config.File, error) {
	f, err := os.Open(src)
	if err != nil {
		return instanceConfInfo{}, config.File{}, fmt.Errorf("--instance-config %d: %w", n, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxInstanceConfigSize+1))
	_ = f.Close() // read-only: the bytes read are what counts
	if err != nil {
		return instanceConfInfo{}, config.File{}, fmt.Errorf("--instance-config %d: %w", n, err)
	}
	if len(data) > maxInstanceConfigSize {
		return instanceConfInfo{}, config.File{}, fmt.Errorf("--instance-config %d: %s has more than %d bytes", n, src, maxInstanceConfigSize)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return instanceConfInfo{}, config.File{}, fmt.Errorf("--instance-config %d: %w", n, err)
	}
	m, err := settingsMap(data)
	if err != nil {
		return instanceConfInfo{}, config.File{}, fmt.Errorf("--instance-config %d: %w", n, err)
	}
	name := fmt.Sprintf("instance-%d.toml", n)
	if err := writeNewFile(filepath.Join(runPath, name), data); err != nil {
		return instanceConfInfo{}, config.File{}, err
	}
	sum := sha256.Sum256(data)
	return instanceConfInfo{Instance: n, Source: src, File: name, SHA256: hex.EncodeToString(sum[:]), Settings: m}, cfg, nil
}

// fileSHA256 returns the hex SHA-256 of the file at path, read as a stream.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }() // read-only: the bytes hashed are what counts
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeContext writes context.json (DESIGN §B8) in the run directory, replacing an earlier
// version through a temporary file, so a reader never sees a partial one.
func (r *runner) writeContext() error {
	data, err := json.MarshalIndent(r.rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding context.json: %w", err)
	}
	return replaceFile(filepath.Join(r.runPath, contextFile), append(data, '\n'))
}

// replaceFile writes data to path (mode 0600) through a temporary file in its directory.
func replaceFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeNewFile(tmp, data); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // the rename error is reported
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	return nil
}

// writeDBRows counts the hits table's rows (r.countRows, dbclient.CountRows in a run) once
// every instance has stopped and writes db_rows.json, {"db_rows":N} (DESIGN §B8; plan choice
// for the shape).
func (r *runner) writeDBRows(ctx context.Context) error {
	if r.countRows == nil {
		return errors.New("counting the hits table's rows: no database client")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	n, err := r.countRows(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		DBRows int64 `json:"db_rows"`
	}{n})
	if err != nil {
		return err
	}
	return writeNewFile(filepath.Join(r.runPath, dbRowsFile), append(data, '\n'))
}

// appendRunIndex appends the run's line, {"scenario":…,"baseline":…,"run_id":…}, to
// runs/index.jsonl (plan H3 step 3; H6 reads it), in one write.
func appendRunIndex(path, scenario string, baseline bool, runID string) error {
	data, err := json.Marshal(struct {
		Scenario string `json:"scenario"`
		Baseline bool   `json:"baseline"`
		RunID    string `json:"run_id"`
	}{scenario, baseline, runID})
	if err != nil {
		return fmt.Errorf("appending to the run index: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("appending to the run index: %w", err)
	}
	_, werr := f.Write(append(data, '\n'))
	if err := errors.Join(werr, f.Close()); err != nil {
		return fmt.Errorf("appending to the run index: %w", err)
	}
	return nil
}
