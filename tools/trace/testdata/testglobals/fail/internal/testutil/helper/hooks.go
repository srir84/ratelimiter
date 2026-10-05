package helper

// measurements is a hook name, but only internal/testutil/conformance's may be written.
var measurements = map[string]int{}

// Record writes a package-level variable.
func Record(key string) {
	measurements[key] = 1 // want
}
