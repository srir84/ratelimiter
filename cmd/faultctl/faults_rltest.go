//go:build rltest

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"ratelimiter/harness/config"
	"ratelimiter/harness/fixtures"
	"ratelimiter/harness/runlog"
	"ratelimiter/internal/receipt"
)

// The run directory's files and the store users faultctl uses (DESIGN §A2, §B1).
const (
	configFile = "config.toml"
	// orchestratorPIDFile is the running orchestrator's pid file, relative to the repository
	// root (IMPLEMENTATION_PLAN H3 step 4), which is a run directory's grandparent (runs/<id>).
	orchestratorPIDFile = ".data/orchestrator.pid"
	userOps             = "rl_ops"
	userFixture         = "rl_fixture"
	// outputSuffix ends a run process's output file, runs/<id>/<name>.stderr, as the
	// orchestrator names them (plan H3 choice): a restarted process appends to its own.
	outputSuffix = ".stderr"
	// maxConfigSize bounds a config file faultctl reads, in bytes.
	maxConfigSize = 1 << 20
	// storeWait bounds one store connection that sends a few quick commands: the dial, AUTH
	// and the commands of a tamper operation, a clock step, or the reads that rebuild the
	// store's command line (plan choice).
	storeWait = 10 * time.Second
	// prekillSlack is how long past P the pre-kill call may take: P + 2 s (plan H4 step 2).
	prekillSlack = 2 * time.Second
	// healthPath is an instance's /health, healthWait bounds one read of it (plan choice), and
	// maxHealthBody bounds its reply, in bytes.
	healthPath    = "/health"
	healthWait    = 5 * time.Second
	maxHealthBody = 64 << 10
)

// report is the command's line on stdout (plan H4 step 3): see the package comment. start_ns
// and end_ns are the command's own times for every command; a store fault whose outage was
// measured adds outage_start_ns and outage_ns.
type report struct {
	Cmd           string `json:"cmd"`
	StartNs       int64  `json:"start_ns"`
	EndNs         int64  `json:"end_ns"`
	OutageStartNs *int64 `json:"outage_start_ns,omitempty"`
	OutageNs      *int64 `json:"outage_ns,omitempty"`
	Error         string `json:"error,omitempty"`
}

// runFiles is what faultctl reads from the run directory: its path and its config.toml.
type runFiles struct {
	path string
	cfg  config.File // Store.Password stays "": config.Parse reads no environment
}

// execute runs inv's command on the run in inv.run and returns its line, with the command's
// start and end as now reads them; the error is the command's failure.
func execute(ctx context.Context, inv invocation, environ []string, client *http.Client, now func() time.Time) (report, error) {
	rep := report{Cmd: inv.cmd.Line}
	start := now()
	stamp := func(err error) (report, error) {
		rep.StartNs, rep.EndNs = start.UnixNano(), now().UnixNano()
		return rep, err
	}
	cfg, err := readConfig(filepath.Join(inv.run, configFile))
	if err != nil {
		return stamp(err)
	}
	rd := runFiles{path: inv.run, cfg: cfg}
	switch inv.cmd.Op {
	case fixtures.OpStoreDown, fixtures.OpStorePause:
		out, err := storeFault(ctx, inv, rd, environ)
		if out.EndNs != 0 { // the outage was measured: where it started, and how long it lasted
			outageStart, outage := out.StartNs, out.OutageNs
			rep.OutageStartNs, rep.OutageNs = &outageStart, &outage
		}
		return stamp(err)
	case fixtures.OpAPIKill:
		return stamp(apiKill(ctx, inv, rd, environ, client))
	case fixtures.OpTamper:
		return stamp(withFixtureConn(ctx, rd, inv.pw.fixture, func(c *fixtures.StoreConn) error {
			return fixtures.NewTamper(c, inv.cmd.Tamper.EpochID).Apply(inv.cmd.Tamper)
		}))
	case fixtures.OpClockSet, fixtures.OpClockAdvance, fixtures.OpClockBack:
		return stamp(withFixtureConn(ctx, rd, inv.pw.fixture, func(c *fixtures.StoreConn) error {
			clk := fixtures.NewClock(c, nil)
			if inv.cmd.Op != fixtures.OpClockSet {
				if err := clk.Load(); err != nil {
					return err // a step needs the value an earlier driver wrote
				}
			}
			return clk.Apply(inv.cmd.Op, inv.cmd.Ns)
		}))
	}
	return stamp(fmt.Errorf("unknown command %q", inv.cmd.Op))
}

// withFixtureConn runs fn on an rl_fixture connection to the run's store (DESIGN §A2), the
// whole exchange within storeWait.
func withFixtureConn(ctx context.Context, rd runFiles, password string, fn func(*fixtures.StoreConn) error) error {
	ctx, cancel := context.WithTimeout(ctx, storeWait)
	defer cancel()
	c, err := fixtures.DialStore(ctx, rd.cfg.Store.Addr, userFixture, password)
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c)
}

// storeFault runs store down or store pause on the run's store: the PID pids.json names goes
// into a PID file of faultctl's own (StoreTarget's form), and after store down the restarted
// store's PID goes from that file into pids.json.
func storeFault(ctx context.Context, inv invocation, rd runFiles, environ []string) (fixtures.Outage, error) {
	pid, err := procPID(rd.path, runlog.RoleValkey, 0)
	if err != nil {
		return fixtures.Outage{}, err
	}
	tmp, err := os.MkdirTemp("", "faultctl-")
	if err != nil {
		return fixtures.Outage{}, fmt.Errorf("the PID file's directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }() // a temporary file of this command alone
	pidFile := filepath.Join(tmp, runlog.RoleValkey+".pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return fixtures.Outage{}, fmt.Errorf("writing the PID file: %w", err)
	}
	t := fixtures.StoreTarget{Addr: rd.cfg.Store.Addr, OpsUser: userOps, OpsPassword: inv.pw.ops, PIDFile: pidFile}
	if inv.cmd.Op == fixtures.OpStorePause {
		return fixtures.StorePause(ctx, t, inv.cmd.Duration)
	}
	if t.Args, err = storeCommandLine(ctx, t, pid); err != nil {
		return fixtures.Outage{}, err
	}
	t.Env = plainEnv(environ)
	t.OutPath = filepath.Join(rd.path, runlog.RoleValkey+outputSuffix)
	out, err := fixtures.StoreDown(ctx, t, inv.cmd.Duration)
	newPID, rerr := readPIDFile(pidFile)
	switch {
	case rerr != nil:
		err = errors.Join(err, rerr)
	case newPID != pid:
		err = errors.Join(err, setPID(rd.path, runlog.RoleValkey, 0, newPID))
	}
	return out, err
}

// storeCommandLine rebuilds the exact command line that started the run's store, to restart
// it (plan H4 step 3, plan choice): the orchestrator's line, runlog.StoreArgs through
// runlog.FileLimitCommand, filled from what the store itself reports as rl_ops. INFO server
// gives executable and config_file (absolute paths), process_id, which must be pid, and
// tcp_port, which must be t.Addr's; CONFIG GET gives dir and aclfile (absolute paths) and
// auto-aof-rewrite-percentage, runlog.RewritePercentageOff unless the scenario forces rewrites.
func storeCommandLine(ctx context.Context, t fixtures.StoreTarget, pid int) ([]string, error) {
	_, portText, err := net.SplitHostPort(t.Addr)
	if err != nil {
		return nil, fmt.Errorf("the store address %q: %w", t.Addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("the store address %q: port %q", t.Addr, portText)
	}
	ctx, cancel := context.WithTimeout(ctx, storeWait)
	defer cancel()
	c, err := fixtures.DialStore(ctx, t.Addr, t.OpsUser, t.OpsPassword)
	if err != nil {
		return nil, fmt.Errorf("reading the store's command line: %w", err)
	}
	defer c.Close()
	info, err := c.Info("server")
	if err != nil {
		return nil, fmt.Errorf("reading the store's command line: %w", err)
	}
	if got := info["process_id"]; got != strconv.Itoa(pid) {
		return nil, fmt.Errorf("the store at %s is process %q, but pids.json names %d", t.Addr, got, pid)
	}
	if got := info["tcp_port"]; got != strconv.Itoa(port) {
		return nil, fmt.Errorf("the store at %s reports tcp_port %q", t.Addr, got)
	}
	set, err := configGet(c, "dir", "aclfile", "auto-aof-rewrite-percentage")
	if err != nil {
		return nil, fmt.Errorf("reading the store's command line: %w", err)
	}
	exe, conf, dir, acl := info["executable"], info["config_file"], set["dir"], set["aclfile"]
	for name, v := range map[string]string{"executable": exe, "config_file": conf, "dir": dir, "aclfile": acl} {
		if !filepath.IsAbs(v) {
			return nil, fmt.Errorf("the store at %s reports %s %q: want an absolute path", t.Addr, name, v)
		}
	}
	forced := set["auto-aof-rewrite-percentage"] != runlog.RewritePercentageOff
	return runlog.FileLimitCommand(exe, runlog.StoreArgs(conf, port, dir, acl, forced)...), nil
}

// configGet reads the store's current value of each named setting (CONFIG GET with several
// names: a flat array of name and value pairs).
func configGet(c *fixtures.StoreConn, names ...string) (map[string]string, error) {
	r, err := c.Do(append([]string{"CONFIG", "GET"}, names...)...)
	if err != nil {
		return nil, err
	}
	if r.Kind != '*' || len(r.Arr)%2 != 0 {
		return nil, fmt.Errorf("CONFIG GET: the store replied %s, want name and value pairs", r)
	}
	m := make(map[string]string, len(r.Arr)/2)
	for i := 0; i < len(r.Arr); i += 2 {
		k, v := r.Arr[i], r.Arr[i+1]
		if k.Kind != '$' || v.Kind != '$' || k.Nil || v.Nil {
			return nil, fmt.Errorf("CONFIG GET: the store replied %s, want bulk strings", r)
		}
		m[k.Str] = v.Str
	}
	for _, n := range names {
		if _, ok := m[n]; !ok {
			return nil, fmt.Errorf("CONFIG GET: the store did not report %s", n)
		}
	}
	return m, nil
}

// apiKill runs api kill N on the run's instance N (runInstance): its command line also gives
// its run log (--log) and its config (--config, for P); the restart, when asked, runs that line
// again through the file-limit shell with RL_LIB_PASSWORD, and the new PID goes into pids.json.
func apiKill(ctx context.Context, inv invocation, rd runFiles, environ []string, client *http.Client) error {
	n := inv.cmd.Instance
	in, err := runInstance(ctx, rd.path, n)
	if err != nil {
		return err
	}
	logPath, cfgPath := flagValue(in.argv, "log"), flagValue(in.argv, "config")
	if logPath == "" || cfgPath == "" {
		return fmt.Errorf("hitapi %d: process %d's command line lacks --log or --config", n, in.pid)
	}
	cfg, err := readConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("hitapi %d: %w", n, err)
	}
	t := fixtures.APITarget{
		BaseURL:        "http://" + in.listen,
		PID:            in.pid,
		LogPath:        logPath,
		PrekillTimeout: time.Duration(cfg.Instance.MaxReleaseDelayNs) + prekillSlack,
		Client:         client,
	}
	if client.Timeout != 0 && client.Timeout < t.PrekillTimeout {
		return fmt.Errorf("hitapi %d: the pre-kill call may take %v, longer than the HTTP client's %v", n, t.PrekillTimeout, client.Timeout)
	}
	if inv.cmd.Restart {
		t.Args = runlog.FileLimitCommand(in.argv[0], in.argv[1:]...)
		t.Env = append(plainEnv(environ), config.LibPasswordEnv+"="+inv.pw.lib)
		t.OutPath = filepath.Join(rd.path, fmt.Sprintf("%s-%d%s", runlog.RoleHitAPI, n, outputSuffix))
	}
	newPID, err := fixtures.KillAPI(ctx, t, inv.cmd.Restart)
	if newPID != 0 {
		err = errors.Join(err, setPID(rd.path, runlog.RoleHitAPI, n, newPID))
	}
	return err
}

// instance is one hitapi instance of the run as faultctl finds it (runInstance): its PID and
// its command line, with the address it serves on.
type instance struct {
	pid    int
	argv   []string
	listen string // --listen, host:port
}

// runInstance finds the run's instance n: its PID from pids.json, and its command line read
// from its process, which must run hitapi (argv[0] named so) and name --listen.
func runInstance(ctx context.Context, runPath string, n int) (instance, error) {
	pid, err := procPID(runPath, runlog.RoleHitAPI, n)
	if err != nil {
		return instance{}, err
	}
	argv, err := commandLine(ctx, pid)
	if err != nil {
		return instance{}, fmt.Errorf("hitapi %d: %w", n, err)
	}
	if filepath.Base(argv[0]) != runlog.RoleHitAPI {
		return instance{}, fmt.Errorf("hitapi %d: process %d runs %q, not hitapi", n, pid, argv[0])
	}
	listen := flagValue(argv, "listen")
	if listen == "" {
		return instance{}, fmt.Errorf("hitapi %d: process %d's command line lacks --listen", n, pid)
	}
	return instance{pid: pid, argv: argv, listen: listen}, nil
}

// runEpoch returns the ID of the epoch the run's instances serve (plan choice; rl_fixture
// cannot read the epoch record, DESIGN §A2): GET /health of each hitapi instance pids.json
// names, in instance order, each found through its --listen (runInstance), until one answers
// 200 with an epoch_id of 32 lowercase hex characters (DESIGN §A5.11; "" while an instance has
// joined no epoch).
func runEpoch(ctx context.Context, runPath string, client *http.Client) (string, error) {
	p, err := runlog.ReadPIDs(filepath.Join(runPath, runlog.PIDsFile))
	if err != nil {
		return "", err
	}
	var ns []int
	for _, pr := range p.Procs {
		if pr.Role == runlog.RoleHitAPI {
			ns = append(ns, pr.Instance)
		}
	}
	if len(ns) == 0 {
		return "", fmt.Errorf("%s names no hitapi instance", runlog.PIDsFile)
	}
	slices.Sort(ns)
	var errs []error
	for _, n := range ns {
		id, err := instanceEpoch(ctx, runPath, n, client)
		if err == nil {
			return id, nil
		}
		errs = append(errs, err)
	}
	return "", errors.Join(errs...)
}

// instanceEpoch reads the epoch_id instance n serves on GET /health, within healthWait.
func instanceEpoch(ctx context.Context, runPath string, n int, client *http.Client) (string, error) {
	in, err := runInstance(ctx, runPath, n)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, healthWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+in.listen+healthPath, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("hitapi %d: GET %s: %w", n, healthPath, err)
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("hitapi %d: GET %s: %w", n, healthPath, err)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxHealthBody+1))
	_ = res.Body.Close() // read or failed: the read's error is the one that matters
	switch {
	case err != nil:
		return "", fmt.Errorf("hitapi %d: GET %s: reading the reply: %w", n, healthPath, err)
	case len(body) > maxHealthBody:
		return "", fmt.Errorf("hitapi %d: GET %s: a reply longer than %d bytes", n, healthPath, maxHealthBody)
	case res.StatusCode != http.StatusOK:
		return "", fmt.Errorf("hitapi %d: GET %s: status %d", n, healthPath, res.StatusCode)
	}
	var h struct {
		EpochID *string `json:"epoch_id"`
	}
	if err := json.Unmarshal(body, &h); err != nil || h.EpochID == nil {
		return "", fmt.Errorf("hitapi %d: GET %s: no epoch_id in the reply", n, healthPath)
	}
	if _, err := receipt.ParseEpochID(*h.EpochID); err != nil {
		return "", fmt.Errorf("hitapi %d: GET %s: epoch_id %q: want 32 lowercase hex characters (has the instance joined an epoch?)", n, healthPath, *h.EpochID)
	}
	return *h.EpochID, nil
}

// refuseUnderOrchestrator refuses cmd, any faultctl command, on a run whose orchestrator is
// live (plan choice): .data/orchestrator.pid in the repository root, the run directory's
// grandparent, names a live process. The orchestrator holds that file for its whole run, from
// before it makes the run directory until after it writes context.json (cmd/orchestrator
// main.go), so nothing else is checked: a standalone stack, a run directory with config.toml
// and pids.json and no orchestrator, is accepted, with or without a context.json. Every command
// changes the store or a process, and a change made beside the orchestrator is in none of the
// run's evidence: context.json records the scenario's [[faults]] alone, with their actual
// start and end (DESIGN §B5 "Faults"), so a store outage, the store's state damaged or a
// process killed would go unseen. A store or instance faultctl restarts is also detached from
// it: the orchestrator would fail the run and never stop the restarted process, which would
// keep the run's port. Those faults are armed as a scenario's [[faults]] instead, which the
// orchestrator runs in-process and records (DESIGN §B4; plan H4 step 7).
func refuseUnderOrchestrator(run, cmd string) error {
	const instead = "faultctl changes the store or a process, which none of the run's records would show, and a process it restarts would outlive the orchestrator and fail the run: " +
		"arm the command as a [[faults]] entry of the scenario, which the orchestrator runs in-process and records in context.json (faultctl is for stacks without an orchestrator)"
	abs, err := filepath.Abs(run)
	if err != nil {
		return fmt.Errorf("%s: --run %s: %w", cmd, run, err)
	}
	pidPath := filepath.Join(filepath.Dir(filepath.Dir(abs)), filepath.FromSlash(orchestratorPIDFile))
	pid, live, err := livePIDFile(pidPath)
	if err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if live {
		return fmt.Errorf("%s: %s names process %d, a live orchestrator: %s", cmd, pidPath, pid, instead)
	}
	return nil
}

// livePIDFile reads the pid file at path and reports the PID it names and whether that process
// is alive, as the orchestrator reads its own (cmd/orchestrator pidfile.go): alive when the
// file holds one decimal PID from 1 to math.MaxInt32, surrounding white space allowed, and
// kill(pid, 0) succeeds or fails with EPERM. A missing file, any other content or a dead PID
// names no live orchestrator; only a read failure is an error.
func livePIDFile(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading the orchestrator's pid file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 1 || pid > math.MaxInt32 {
		return 0, false, nil
	}
	err = syscall.Kill(pid, 0)
	return pid, err == nil || errors.Is(err, syscall.EPERM), nil
}

// commandLine reads process pid's command line (gopsutil): a hitapi started through the
// file-limit shell shows its own, which exec kept.
func commandLine(ctx context.Context, pid int) ([]string, error) {
	if pid < 2 || pid > math.MaxInt32 {
		return nil, fmt.Errorf("pid %d: out of range", pid)
	}
	p, err := process.NewProcessWithContext(ctx, int32(pid))
	if err != nil {
		return nil, fmt.Errorf("process %d: %w", pid, err)
	}
	argv, err := p.CmdlineSliceWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("process %d's command line: %w", pid, err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("process %d has no command line", pid)
	}
	return argv, nil
}

// flagValue returns the value of flag name in a Go program's command line argv, given as
// --name value, -name value, --name=value or -name=value; "" when absent.
func flagValue(argv []string, name string) string {
	for i := 1; i < len(argv); i++ {
		if argv[i] == "--" {
			break
		}
		for _, f := range []string{"--" + name, "-" + name} {
			if argv[i] == f && i+1 < len(argv) {
				return argv[i+1]
			}
			if v, ok := strings.CutPrefix(argv[i], f+"="); ok {
				return v
			}
		}
	}
	return ""
}

// readConfig reads and checks a harness config file (config.Parse: no environment is read, so
// no password), at most maxConfigSize bytes. Only a regular file is read, so the read cannot
// wait without bound (AGENTS.md §2): a FIFO or a device is refused unopened, and the open
// (O_NONBLOCK: a FIFO swapped in meanwhile does not wait for a writer) is checked again.
func readConfig(path string) (config.File, error) {
	if fi, err := os.Stat(path); err != nil {
		return config.File{}, fmt.Errorf("reading the config: %w", err)
	} else if !fi.Mode().IsRegular() {
		return config.File{}, fmt.Errorf("reading the config: %s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return config.File{}, fmt.Errorf("reading the config: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only: the read's error is the one that matters
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return config.File{}, fmt.Errorf("reading the config: %s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return config.File{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > maxConfigSize {
		return config.File{}, fmt.Errorf("reading %s: more than %d bytes", path, maxConfigSize)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return config.File{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// procPID returns the PID pids.json names for role (and instance, hitapi only).
func procPID(runPath, role string, instance int) (int, error) {
	p, err := runlog.ReadPIDs(filepath.Join(runPath, runlog.PIDsFile))
	if err != nil {
		return 0, err
	}
	for _, pr := range p.Procs {
		if pr.Role == role && pr.Instance == instance {
			return pr.PID, nil
		}
	}
	return 0, fmt.Errorf("%s names no %s", runlog.PIDsFile, procName(role, instance))
}

// setPID writes pid as role's (and instance's) PID into the run's pids.json, which the
// sampler re-reads on every tick (plan H4 step 3): a restarted process is found by its new PID.
func setPID(runPath, role string, instance, pid int) error {
	path := filepath.Join(runPath, runlog.PIDsFile)
	p, err := runlog.ReadPIDs(path)
	if err != nil {
		return err
	}
	for i := range p.Procs {
		if p.Procs[i].Role == role && p.Procs[i].Instance == instance {
			p.Procs[i].PID = pid
			return runlog.WritePIDs(path, p)
		}
	}
	return fmt.Errorf("%s names no %s", runlog.PIDsFile, procName(role, instance))
}

// procName names a run process for a message.
func procName(role string, instance int) string {
	if instance == 0 {
		return role
	}
	return fmt.Sprintf("%s %d", role, instance)
}

// readPIDFile reads the PID in faultctl's own PID file (decimal text).
func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("reading the PID file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("the PID file %s holds %q, not a PID", path, data)
	}
	return pid, nil
}

// plainEnv is environ less its RL_ variables (DESIGN §A2): the environment of a process
// faultctl restarts, before the one password it may need is added.
func plainEnv(environ []string) []string {
	env := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "RL_") {
			env = append(env, kv)
		}
	}
	return env
}
