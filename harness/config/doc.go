// Package config loads the harness's shared config file (DESIGN §B2; HARNESS HAR-12): the
// limiter's policy and instance settings, the store address and the hitapi settings, read
// once at startup by every hitapi. Load decodes [policy] onto limiter.DefaultPolicy(), then
// [instance] onto limiter.DefaultInstance(policy), so absent keys keep their defaults
// (DESIGN §A5.2 "Defaults and zero values"); Merge overlays a scenario's overrides on the
// base file (§B4, §B5). Unknown keys are an error (IMPLEMENTATION_PLAN H1 step 3).
// LoadScenario reads a scenario file (§B4; IMPLEMENTATION_PLAN H2 step 1), the input of
// hitgen's schedule and of the orchestrator's run, keeping its overrides for Merge.
package config
