package runlog

import (
	"slices"
	"testing"
)

// TestStoreCommandLine (IMPLEMENTATION_PLAN H3 step 3, H4 step 3; DESIGN §A2, §B1): a run's
// store starts as /bin/sh -c 'ulimit -S -n 8192; exec "$0" "$@"' valkey-server <conf> --port
// <port> --dir <dir> --aclfile <acl>, with --auto-aof-rewrite-percentage 0 unless the scenario
// forces rewrites; the orchestrator and faultctl build it from these two functions alone.
func TestStoreCommandLine(t *testing.T) {
	const (
		bin  = "/opt/homebrew/bin/valkey-server"
		conf = "/repo/deploy/valkey-durable.conf"
		dir  = "/repo/runs/20261009-120000-s-1/store"
		acl  = "/repo/runs/20261009-120000-s-1/store/acl.conf"
	)
	head := []string{"/bin/sh", "-c", `ulimit -S -n 8192; exec "$0" "$@"`, bin, conf, "--port", "6390", "--dir", dir, "--aclfile", acl}
	if got, want := FileLimitCommand(bin, StoreArgs(conf, 6390, dir, acl, false)...),
		append(slices.Clone(head), "--auto-aof-rewrite-percentage", "0"); !slices.Equal(got, want) {
		t.Errorf("the store's command line without forced rewrites =\n  %q\nwant\n  %q", got, want)
	}
	if got := FileLimitCommand(bin, StoreArgs(conf, 6390, dir, acl, true)...); !slices.Equal(got, head) {
		t.Errorf("the store's command line with forced rewrites =\n  %q\nwant\n  %q", got, head)
	}
	if got, want := FileLimitCommand("/bin/hitapi"), []string{"/bin/sh", "-c", `ulimit -S -n 8192; exec "$0" "$@"`, "/bin/hitapi"}; !slices.Equal(got, want) {
		t.Errorf("FileLimitCommand without arguments = %q, want %q", got, want)
	}
}
