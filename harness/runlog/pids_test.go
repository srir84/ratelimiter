package runlog

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestPIDs_RoundTrip: WritePIDs writes pids.json with mode 0600 and no temporary file left
// behind, rewriting replaces the list, and ReadPIDs reads back what was written (plan H3 step
// 4: the orchestrator rewrites the file when hitgen starts).
func TestPIDs_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PIDsFile)
	first := PIDs{Procs: []Proc{
		{Role: RoleValkey, PID: 100},
		{Role: RoleHitAPI, Instance: 1, PID: 101},
		{Role: RoleHitAPI, Instance: 2, PID: 102},
		{Role: RoleHitAPI, Instance: 3, PID: 103},
		{Role: RoleSampler, PID: 104},
	}}
	second := PIDs{Procs: append(append([]Proc{}, first.Procs...), Proc{Role: RoleHitgen, PID: 105})}
	for _, want := range []PIDs{first, second} {
		if err := WritePIDs(path, want); err != nil {
			t.Fatalf("WritePIDs: %v", err)
		}
		got, err := ReadPIDs(path)
		if err != nil {
			t.Fatalf("ReadPIDs: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ReadPIDs = %+v, want %+v", got, want)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const wantJSON = `{"procs":[{"role":"valkey-server","pid":100},{"role":"hitapi","instance":1,"pid":101},` +
		`{"role":"hitapi","instance":2,"pid":102},{"role":"hitapi","instance":3,"pid":103},` +
		`{"role":"sampler","pid":104},{"role":"hitgen","pid":105}]}` + "\n"
	if string(data) != wantJSON {
		t.Errorf("pids.json =\n%s\nwant\n%s", data, wantJSON)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("pids.json mode %o, want 600", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want only pids.json: %v", len(entries), entries)
	}
}

// TestPIDs_Refusals: WritePIDs refuses an invalid list and writes nothing; ReadPIDs refuses an
// invalid or malformed file, each with an error naming the problem.
func TestPIDs_Refusals(t *testing.T) {
	invalid := []struct {
		name string
		p    PIDs
		want string
	}{
		{"unknown role", PIDs{Procs: []Proc{{Role: "postgres", PID: 1}}}, `role "postgres"`},
		{"pid 0", PIDs{Procs: []Proc{{Role: RoleValkey, PID: 0}}}, "pid 0"},
		{"hitapi without instance", PIDs{Procs: []Proc{{Role: RoleHitAPI, PID: 1}}}, "hitapi instance 0"},
		{"hitapi instance 4", PIDs{Procs: []Proc{{Role: RoleHitAPI, Instance: 4, PID: 1}}}, "hitapi instance 4"},
		{"instance on valkey", PIDs{Procs: []Proc{{Role: RoleValkey, Instance: 1, PID: 1}}}, "valkey-server has instance 1"},
		{"two entries for one instance", PIDs{Procs: []Proc{
			{Role: RoleHitAPI, Instance: 2, PID: 1}, {Role: RoleHitAPI, Instance: 2, PID: 2},
		}}, "a second hitapi entry"},
		{"two samplers", PIDs{Procs: []Proc{{Role: RoleSampler, PID: 1}, {Role: RoleSampler, PID: 2}}}, "a second sampler entry"},
		{"one pid twice", PIDs{Procs: []Proc{{Role: RoleValkey, PID: 7}, {Role: RoleHitgen, PID: 7}}}, "pid 7 named twice"},
	}
	for _, c := range invalid {
		t.Run("write "+c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), PIDsFile)
			err := WritePIDs(path, c.p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("WritePIDs: error %v, want one naming %q", err, c.want)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("WritePIDs wrote %s for an invalid list (stat: %v)", path, err)
			}
		})
	}
	malformed := []struct{ name, content, want string }{
		{"unknown key", `{"procs":[],"run":1}`, "unknown field"},
		{"unknown entry key", `{"procs":[{"role":"sampler","pid":1,"cpu":2}]}`, "unknown field"},
		{"pid not an integer", `{"procs":[{"role":"sampler","pid":1.5}]}`, "pid"},
		{"two objects", `{"procs":[]} {"procs":[]}`, "data after the JSON object"},
		{"not JSON", `procs`, "invalid character"},
		{"invalid entry", `{"procs":[{"role":"hitapi","pid":3}]}`, "hitapi instance 0"},
		{"too large", `{"procs":[]}` + strings.Repeat(" ", maxPIDsSize), "more than"},
	}
	for _, c := range malformed {
		t.Run("read "+c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), PIDsFile)
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			p, err := ReadPIDs(path)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("ReadPIDs = %+v, %v; want an error naming %q", p, err, c.want)
			}
		})
	}
	if _, err := ReadPIDs(filepath.Join(t.TempDir(), PIDsFile)); err == nil {
		t.Error("ReadPIDs of a missing file: no error")
	}
}
