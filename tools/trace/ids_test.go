package main

import (
	"strings"
	"testing"
)

func TestIDs_RealDocs(t *testing.T) {
	root := repoRoot(t)
	spec, err := sectionIDs(t.Context(), root, "SPEC.md", "## 10.")
	if err != nil {
		t.Fatal(err)
	}
	harness, err := sectionIDs(t.Context(), root, "HARNESS.md", "## 8.")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec) != 63 || len(harness) != 20 {
		t.Fatalf("SPEC §10 defines %d IDs and HARNESS §8 %d, want 63 and 20", len(spec), len(harness))
	}
	defs, err := definedIDs(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, d := range defs {
		ids[d.id] = true
	}
	if len(ids) != 83 {
		t.Fatalf("%d distinct IDs, want 83", len(ids))
	}
	if !ids["T-KEY-1"] {
		t.Error("T-KEY-1 (the [D] row form) is missing")
	}
	if ids["T-RCP-2"] {
		t.Error("T-RCP-2 is not a defined ID")
	}
	// `ids --list` prints exactly the 83 IDs.
	code, stdout, stderr := runIn(t, root, "ids", "--list")
	if lines := strings.Split(strings.TrimSpace(stdout), "\n"); code != 0 || len(lines) != 83 || lines[0] != "T-COR-1" {
		t.Fatalf("ids --list: exit %d, %d lines\nstderr: %s", code, len(lines), stderr)
	}
}

func TestIDs_Mapping(t *testing.T) {
	for _, c := range []struct{ id, goName, pyName string }{
		{"T-HFLT-1", "TestT_HFLT_1", "test_t_hflt_1"},
		{"T-COR-10", "TestT_COR_10", "test_t_cor_10"},
		{"T-KEY-1", "TestT_KEY_1", "test_t_key_1"},
		{"T-RPT-2", "TestT_RPT_2", "test_t_rpt_2"},
	} {
		if got := idToGo(c.id); got != c.goName {
			t.Errorf("idToGo(%s) = %s, want %s", c.id, got, c.goName)
		}
		if got := idToPy(c.id); got != c.pyName {
			t.Errorf("idToPy(%s) = %s, want %s", c.id, got, c.pyName)
		}
		if got, ok := goToID(c.goName); !ok || got != c.id {
			t.Errorf("goToID(%s) = %s, %v, want %s", c.goName, got, ok, c.id)
		}
		if got, ok := pyToID(c.pyName); !ok || got != c.id {
			t.Errorf("pyToID(%s) = %s, %v, want %s", c.pyName, got, ok, c.id)
		}
	}
	for _, bad := range []string{"TestT_COR_1x", "TestT_cor_1", "TestT_COR", "TestT_COR_1_2", "TestCOR_1"} {
		if id, ok := goToID(bad); ok {
			t.Errorf("goToID(%s) = %s, want no ID", bad, id)
		}
	}
	for _, bad := range []string{"test_t_COR_1", "test_t_cor_1x", "test_t_cor", "test_cor_1"} {
		if id, ok := pyToID(bad); ok {
			t.Errorf("pyToID(%s) = %s, want no ID", bad, id)
		}
	}
}

func TestIDs_DuplicateOrphanMalformed(t *testing.T) {
	for _, c := range []struct{ fixture, want string }{
		{"ids/duplicate", "T-COR-1 has 2 test functions: a/a_test.go:5 TestT_COR_1, b/b_test.go:5 TestT_COR_1"},
		{"ids/orphan", "a/a_test.go:5: TestT_COR_99 maps to T-COR-99, which SPEC.md §10 and HARNESS.md §8 do not define"},
		{"ids/malformed", "a/a_test.go:5: TestT_COR_1x is not a test-ID name"},
		{"ids/twice", "SPEC.md:10: T-COR-2 is defined twice"}, // one SPEC row listed twice
	} {
		t.Run(c.fixture, func(t *testing.T) {
			root := fixture(t, "ids/docs", c.fixture)
			code, _, stderr := runIn(t, root, "ids")
			problems := strings.Split(strings.TrimSpace(stderr), "\n")
			if code != 1 || len(problems) != 2 || !strings.HasPrefix(problems[0], "ids: "+c.want) {
				t.Fatalf("exit %d, want 1 with the problem %q\nstderr:\n%s", code, c.want, stderr)
			}
		})
	}
	// The fixture docs define the rows under "## 10." and "## 8." only.
	defs, err := definedIDs(t.Context(), testdataDir(t, "ids/docs"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range defs {
		got = append(got, d.id)
	}
	if strings.Join(got, " ") != "T-COR-1 T-COR-2 T-KEY-1 T-HTTP-1" {
		t.Fatalf("defined IDs = %v", got)
	}
}

func TestIDs_SkipsTestdata(t *testing.T) {
	root := fixture(t, "ids/docs", "ids/skips")
	code, stdout, stderr := runIn(t, root, "ids", "--complete")
	// No orphan from a/testdata/ or refmodel/testdata/; --complete reports only the IDs
	// with no function.
	problems := strings.Split(strings.TrimSpace(stderr), "\n")
	if code != 1 || len(problems) != 3 || countContaining(problems, "T-KEY-1 has no test function") != 1 ||
		countContaining(problems, "T-HTTP-1 has no test function") != 1 || strings.Contains(stderr, "99") {
		t.Fatalf("ids --complete: exit %d\nstdout: %s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "4 IDs defined, 2 test-ID functions") {
		t.Fatalf("summary: %s", stdout)
	}
	// Without --complete, completeness is not checked (make check runs this form).
	if code, _, stderr := runIn(t, root, "ids"); code != 0 {
		t.Fatalf("ids: exit %d\n%s", code, stderr)
	}
}
