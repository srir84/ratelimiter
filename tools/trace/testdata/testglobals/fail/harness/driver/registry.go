package driver

// Instances is a registry that its pointer-receiver method Add changes.
type Instances struct{ open map[string]int }

// Add records an open instance.
func (r *Instances) Add(name string) { r.open[name]++ }

// Count only reads the registry: a value receiver.
func (r Instances) Count() int { return len(r.open) }

// Live is written through Add by the tests that call it (a/method_test.go and
// a/dotdriver_test.go); its declaration holds no mutable value.
var Live = Instances{open: map[string]int{}}
