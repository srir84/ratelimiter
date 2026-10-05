package gotestfail

import "testing"

func TestPlantedFailure(t *testing.T) {
	t.Fatal("planted failure: trace gotest must exit non-zero")
}
