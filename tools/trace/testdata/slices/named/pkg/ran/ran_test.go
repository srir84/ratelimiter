package ran

import (
	"fmt"
	"testing"
)

func TestPass(t *testing.T) {}

func TestRenamedNow(t *testing.T) {}

func TestNoPass(t *testing.T) {}

func TestBrace(t *testing.T) {
	for _, name := range []string{"A", "B"} {
		t.Run(name, func(t *testing.T) {})
	}
}

func TestGap(t *testing.T) {
	for _, name := range []string{"A", "C"} {
		t.Run(name, func(t *testing.T) {})
	}
}

func TestStar_One(t *testing.T) {}

func TestStar_Two(t *testing.T) {}

func TestSkipped(t *testing.T) {}

func TestOther(t *testing.T) {
	for _, name := range []string{"sub", "kept"} {
		t.Run(name, func(t *testing.T) {})
	}
}

func TestFlaky(t *testing.T) {}

func TestFlakyStream(t *testing.T) {}

func TestSkipThenPass(t *testing.T) {}

func TestUnrelatedSkip(t *testing.T) {}

func TestRanOnly(t *testing.T) {}

func TestThenSkip(t *testing.T) {}

func TestDoSkip(t *testing.T) {}

func TestWrapperSkip(t *testing.T) {}

func TestCaseSkip(t *testing.T) {
	for _, name := range []string{"a", "b"} {
		t.Run(name, func(t *testing.T) {})
	}
}

func TestGroup(t *testing.T) {
	for _, group := range []string{"A", "B"} {
		t.Run(group, func(t *testing.T) {
			for _, name := range []string{"x", "y"} {
				t.Run(name, func(t *testing.T) {})
			}
		})
	}
}

func ExampleNoOutput() {
	fmt.Println("never run: the example has no output comment")
}
