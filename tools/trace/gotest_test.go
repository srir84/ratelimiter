package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runGotestIn runs trace gotest in a testdata module and returns its exit code, stdout
// and the saved stream.
func runGotestIn(t *testing.T, module string, args ...string) (int, string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "nested", "stream.json") // gotest creates the directory
	var stdout, stderr strings.Builder
	e := &env{dir: testdataDir(t, module), stdout: &stdout, stderr: &stderr}
	code := runGotest(t.Context(), e, append([]string{"-o", out, "--"}, args...))
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("stream: %v\nstderr: %s", err, stderr.String())
	}
	return code, stdout.String(), string(data)
}

func TestGotest_ExitCode(t *testing.T) {
	code, stdout, stream := runGotestIn(t, "gotest/fail", "-count=1", "./...")
	if code == 0 {
		t.Fatalf("a failing module gave exit 0\n%s", stdout)
	}
	if !strings.Contains(stdout, "gotest: FAIL gotestfail TestPlantedFailure") || !strings.Contains(stdout, "0 passed, 1 failed, 0 skipped") {
		t.Errorf("summary does not list the failed test:\n%s", stdout)
	}
	if !strings.Contains(stream, `"Action":"fail"`) {
		t.Errorf("the saved stream has no fail event")
	}
	code, stdout, _ = runGotestIn(t, "gotest/pass", "-count=1", "./...")
	if code != 0 || !strings.Contains(stdout, "3 passed, 0 failed, 0 skipped") {
		t.Fatalf("a passing module: exit %d\n%s", code, stdout)
	}
}

func TestGotest_RepeatedRuns(t *testing.T) {
	// -cpu 1,4 runs each test twice: a failure in the first run outlives the pass in the
	// second (fail beats skip beats pass), so it is listed and counted.
	code, stdout, stream := runGotestIn(t, "gotest/flaky", "-count=1", "-cpu", "1,4", "./...")
	if code == 0 {
		t.Fatalf("a test failing in one of its runs gave exit 0\n%s", stdout)
	}
	for _, want := range []string{
		"gotest: FAIL gotestflaky TestCPU\n",
		"gotest: SKIP gotestflaky TestSkipOnce\n",
		"gotest: FAIL gotestflaky (package)\n",
		"gotest: 1 passed, 1 failed, 1 skipped;",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Count(stream, `"Action":"pass","Package":"gotestflaky","Test":"TestCPU"`) != 1 {
		t.Errorf("the stream does not hold TestCPU's passing second run:\n%s", stream)
	}
}

func TestGotest_Shuffle(t *testing.T) {
	_, _, stream := runGotestIn(t, "gotest/pass", "-count=1", "./...")
	if !strings.Contains(stream, "-test.shuffle ") {
		t.Fatalf("the saved stream has no -test.shuffle line:\n%s", stream)
	}
	_, _, stream = runGotestIn(t, "gotest/pass", "-count=1", "-shuffle=7", "./...")
	if !strings.Contains(stream, "-test.shuffle 7") {
		t.Fatalf("-shuffle=7 was not passed through:\n%s", stream)
	}
	if args := gotestArgs([]string{"-count=1", "-shuffle=7", "./..."}); slices.Contains(args, "-shuffle=on") {
		t.Errorf("gotestArgs added -shuffle=on beside -shuffle=7: %v", args)
	}
	if args := gotestArgs([]string{"-race", "./..."}); strings.Join(args, " ") != "test -json -shuffle=on -race ./..." {
		t.Errorf("gotestArgs = %v", args)
	}
}
