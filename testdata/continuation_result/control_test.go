package control

import "testing"

func TestSkip(t *testing.T) {
	t.Skip("deliberate child skip")
}

func TestSubtestSkip(t *testing.T) {
	t.Run("inner", func(t *testing.T) {
		t.Skip("deliberate child subtest skip")
	})
}
