//go:build rltest

package spectest

import "testing"

func TestT_COR_1(t *testing.T) {}

func TestT_COR_2(t *testing.T) {}

func TestT_COR_3(t *testing.T) {}

func TestT_COR_6(t *testing.T) {}

func TestT_COR_7(t *testing.T) {}

func TestT_COR_8(t *testing.T) {}

func TestT_COR_9(t *testing.T) {}

func TestT_COR_10(t *testing.T) {
	for _, name := range []string{"a", "b"} {
		t.Run(name, func(t *testing.T) {})
	}
}
