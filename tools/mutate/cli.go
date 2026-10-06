package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const usageText = `usage: mutate --curated [ID…]
       mutate --sweep <file>… [--budget 30m]

  --curated [ID…]  every tools/mutate/curated.go entry whose file exists (or only the IDs
                   given): its named tests first pass on the unmutated code, then the mutant
                   counts as killed only when one of them fails with an assertion (--- FAIL,
                   no panic:, build error or timeout; DESIGN §C9). Exit 1 when a baseline
                   fails or a mutant is not killed.
  --sweep <file>…  the §C9 operators on the files (paths relative to the module root): .lua
                   files, and .go files under internal/core, internal/store, internal/receipt,
                   internal/keys and internal/settings. Each mutant runs the L2 packages (.lua)
                   or its own package's tests (.go) until the budget is spent. Survivors and
                   untried mutants go to .out/mutants.txt; they never fail the run.
  --budget <d>     the sweep's time budget (default 30m)
  --timeout <d>    the most one test run may take (default 10m)

Only L1 and L2 tests run, with -count=1 -failfast. Test output goes to .out/mutate/, never
.out/*.json, and the replies the L2 tests record go to a temp file (RL_REPLIES_FILE), never
.out/replies-l2.jsonl.
`

// config is what a run works with; main fills it from the module root and the environment,
// and tests build their own around a fixture module.
type config struct {
	root        string        // module root, symlinks resolved
	outDir      string        // test output of the runs (.out/mutate)
	mutantsFile string        // the sweep's survivors and untried mutants (.out/mutants.txt)
	environ     []string      // environment of every child process
	goBin       string        // go
	python      string        // python3
	luacheck    string        // luacheck
	maxTimeout  time.Duration // the most one test run may take
	tmpDir      string        // parent of the temp directories ("": the system's)
	luaPackages []string      // the L2 packages a .lua mutant runs (-tags scripttest)
	goScope     []string      // the directories whose .go files the Go operators apply to
}

func defaultConfig(root string, environ []string) config {
	return config{
		root:        root,
		outDir:      filepath.Join(root, ".out", "mutate"),
		mutantsFile: filepath.Join(root, ".out", "mutants.txt"),
		environ:     environ,
		goBin:       "go",
		python:      "python3",
		luacheck:    "luacheck",
		maxTimeout:  10 * time.Minute,
		// The packages of make test-scripts (plan S4 step 12).
		luaPackages: []string{"./internal/scripts/...", "./internal/testutil/storestate/...", "./internal/testutil/scriptrun/..."},
		goScope:     []string{"internal/core", "internal/store", "internal/receipt", "internal/keys", "internal/settings"},
	}
}

// printer writes a run's report and keeps the first write error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}

// options is a parsed command line.
type options struct {
	curated bool
	ids     []string
	sweep   bool
	files   []string
	budget  time.Duration
	timeout time.Duration
}

var errHelp = errors.New("help")

func parseArgs(args []string) (options, error) {
	o := options{budget: 30 * time.Minute}
	budgetSet := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := strings.Cut(a, "=")
		switch {
		case a == "help" || a == "-h" || a == "-help" || a == "--help":
			return o, errHelp
		case a == "--curated" || a == "-curated":
			if o.curated || o.sweep {
				return o, errors.New("give --curated or --sweep, once")
			}
			o.curated = true
		case a == "--sweep" || a == "-sweep":
			if o.curated || o.sweep {
				return o, errors.New("give --curated or --sweep, once")
			}
			o.sweep = true
		case name == "--budget" || name == "-budget" || name == "--timeout" || name == "-timeout":
			if !hasValue {
				if i+1 >= len(args) {
					return o, fmt.Errorf("%s needs a duration", name)
				}
				i++
				value = args[i]
			}
			d, err := time.ParseDuration(value)
			if err != nil || d <= 0 {
				return o, fmt.Errorf("%s %q is not a positive duration", name, value)
			}
			if strings.HasSuffix(name, "budget") {
				o.budget, budgetSet = d, true
			} else {
				o.timeout = d
			}
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %s", a)
		case o.curated:
			o.ids = append(o.ids, a)
		case o.sweep:
			o.files = append(o.files, a)
		default:
			return o, fmt.Errorf("unexpected argument %q before --curated or --sweep", a)
		}
	}
	switch {
	case !o.curated && !o.sweep:
		return o, errors.New("give --curated or --sweep")
	case o.sweep && len(o.files) == 0:
		return o, errors.New("--sweep needs the files to mutate")
	case o.curated && budgetSet:
		return o, errors.New("--budget is for --sweep")
	}
	return o, nil
}

// run parses args, builds the config with mkConfig and runs the mode; it returns the exit
// code: 0 pass, 1 a baseline failed, a mutant was not killed or a run went wrong, 2 usage.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, mkConfig func() (config, error)) int {
	out, errOut := &printer{w: stdout}, &printer{w: stderr}
	opts, err := parseArgs(args)
	if errors.Is(err, errHelp) {
		out.printf("%s", usageText)
		return 0
	}
	if err != nil {
		errOut.printf("mutate: %v\n\n%s", err, usageText)
		return 2
	}
	cfg, err := mkConfig()
	if err != nil {
		errOut.printf("mutate: %v\n", err)
		return 2
	}
	if opts.timeout > 0 {
		cfg.maxTimeout = opts.timeout
	}
	var code int
	if opts.curated {
		code = curatedCmd(ctx, cfg, curatedEntries(), opts.ids, out, errOut)
	} else {
		code = sweepCmd(ctx, cfg, opts.files, opts.budget, out, errOut)
	}
	if (out.err != nil || errOut.err != nil) && code == 0 {
		return 1 // the report could not be written
	}
	return code
}
