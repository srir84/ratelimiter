//go:build !rltest

package main

import (
	"context"
	"errors"
	"time"

	"ratelimiter/harness/config"
)

// errNotTestBuild is why an untagged orchestrator cannot drive the controlled clock or run
// faultctl: the clock driver lives in harness/fixtures, which only rltest builds contain.
var errNotTestBuild = errors.New("this orchestrator was built without -tags rltest; run bin/test/orchestrator (make build)")

// buildRefusals is this build's part of preflight (IMPLEMENTATION_PLAN H3 steps 1 and 3): an
// untagged orchestrator refuses a controlled clock (merged clock_source = "controlled") and
// every faultctl cmd.
func buildRefusals(sc config.Scenario, controlled bool) []refusal {
	var refs refusals
	if controlled {
		refs.add(reasonClockUntagged, "clock_source = \"controlled\": %v", errNotTestBuild)
	}
	for i, f := range sc.Faults {
		if f.Faultctl() {
			refs.add(reasonFaultctlUntagged, "faults[%d].cmd %q runs faultctl: %v", i, f.Cmd, errNotTestBuild)
		}
	}
	return refs
}

// setControlledClock fails in an untagged build, which has no clock driver; preflight has
// already refused every run that would call it.
func setControlledClock(context.Context, string, string, time.Time) (int64, error) {
	return 0, errNotTestBuild
}
