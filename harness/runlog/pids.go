package runlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// PIDsFile is the name of a run directory's process list (IMPLEMENTATION_PLAN H3 steps 4 and
// 7): the orchestrator writes runs/<id>/pids.json as it starts the run's processes and
// rewrites it when hitgen starts; the sampler reads it on every tick to find the processes it
// samples (DESIGN §B7).
const PIDsFile = "pids.json"

// The roles of a run's processes in pids.json (DESIGN §B1, §B7).
const (
	RoleValkey  = "valkey-server" // the limiter store
	RoleHitAPI  = "hitapi"        // one entry per instance
	RoleSampler = "sampler"
	RoleHitgen  = "hitgen"
)

// MaxInstances is the most hitapi instances a run starts (DESIGN §B4: 1 or 3).
const MaxInstances = 3

// maxPIDsSize is the largest pids.json ReadPIDs accepts, in bytes; six entries need far less.
const maxPIDsSize = 64 << 10

// PIDs is the content of pids.json, {"procs":[…]}: the run's processes, one entry each, in
// any order.
type PIDs struct {
	Procs []Proc `json:"procs"`
}

// Proc is one process of a run: its role, its instance (hitapi only, 1…MaxInstances; 0, and
// left out of the file, for every other role) and its PID.
type Proc struct {
	Role     string `json:"role"`
	Instance int    `json:"instance,omitempty"`
	PID      int    `json:"pid"`
}

// Validate checks p: every role one of the four; every PID above 0 and named once; each hitapi
// entry with an instance in 1…MaxInstances, one entry per instance; at most one entry for each
// other role, without an instance.
func (p PIDs) Validate() error {
	var errs []error
	roles := map[Proc]bool{} // role and instance, PID left 0
	pids := map[int]bool{}
	for i, pr := range p.Procs {
		switch pr.Role {
		case RoleHitAPI:
			if pr.Instance < 1 || pr.Instance > MaxInstances {
				errs = append(errs, fmt.Errorf("procs[%d]: hitapi instance %d: want 1 to %d", i, pr.Instance, MaxInstances))
			}
		case RoleValkey, RoleSampler, RoleHitgen:
			if pr.Instance != 0 {
				errs = append(errs, fmt.Errorf("procs[%d]: %s has instance %d: only hitapi entries have one", i, pr.Role, pr.Instance))
			}
		default:
			errs = append(errs, fmt.Errorf("procs[%d]: role %q: want %s, %s, %s or %s", i, pr.Role, RoleValkey, RoleHitAPI, RoleSampler, RoleHitgen))
		}
		if pr.PID < 1 {
			errs = append(errs, fmt.Errorf("procs[%d]: pid %d: want 1 or more", i, pr.PID))
		}
		key := Proc{Role: pr.Role, Instance: pr.Instance}
		if roles[key] {
			errs = append(errs, fmt.Errorf("procs[%d]: a second %s entry (instance %d)", i, pr.Role, pr.Instance))
		}
		roles[key] = true
		if pr.PID >= 1 && pids[pr.PID] {
			errs = append(errs, fmt.Errorf("procs[%d]: pid %d named twice", i, pr.PID))
		}
		pids[pr.PID] = true
	}
	return errors.Join(errs...)
}

// WritePIDs checks p and writes it to path (mode 0600) through a temporary file in the same
// directory and one rename, so a reader finds the old list or the new one, never part of one.
func WritePIDs(path string, p PIDs) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if p.Procs == nil {
		p.Procs = []Proc{} // "procs":[] rather than null
	}
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pids-*.json")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name()) // the half-written temporary file; the error is reported
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name()) // the rename failed: nothing else refers to the temporary file
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// ReadPIDs reads the pids.json at path and checks it: a regular file (readRegular) of at most
// maxPIDsSize bytes holding one JSON object with the procs key only, each entry with only
// role, instance and pid, then Validate.
func ReadPIDs(path string) (PIDs, error) {
	data, err := readRegular(path, maxPIDsSize+1)
	if err != nil {
		return PIDs{}, fmt.Errorf("reading the process list: %w", err)
	}
	if len(data) > maxPIDsSize {
		return PIDs{}, fmt.Errorf("reading %s: more than %d bytes", path, maxPIDsSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p PIDs
	if err := dec.Decode(&p); err != nil {
		return PIDs{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return PIDs{}, fmt.Errorf("reading %s: data after the JSON object", path)
	}
	if err := p.Validate(); err != nil {
		return PIDs{}, fmt.Errorf("reading %s: %w", path, err)
	}
	return p, nil
}

// readRegular reads at most limit bytes of the file at path. Only a regular file is read, so
// the read cannot wait without bound (AGENTS.md §2; cmd/rladmin reads its policy file so): a
// FIFO, a device or any other file is refused unopened, and the open (O_NONBLOCK: a FIFO
// swapped in meanwhile does not wait for a writer) is checked again before reading.
func readRegular(path string, limit int64) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %v)", path, fi.Mode())
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // opened read-only: closing cannot change what was read
	if fi, err = f.Stat(); err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %v)", path, fi.Mode())
	}
	return io.ReadAll(io.LimitReader(f, limit))
}
