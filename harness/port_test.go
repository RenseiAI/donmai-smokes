package harness

import "testing"

// TestPickFreePort_ReturnsValidPort verifies PickFreePort returns a
// non-zero ephemeral port. The returned port is in the kernel's
// ephemeral range, so we don't assert the exact range — just that we
// got something usable.
func TestPickFreePort_ReturnsValidPort(t *testing.T) {
	p, err := PickFreePort()
	if err != nil {
		t.Fatalf("PickFreePort: %v", err)
	}
	if p <= 0 || p > 65535 {
		t.Errorf("PickFreePort returned out-of-range port: %d", p)
	}
}

// TestPickFreePort_ReturnsDifferentPorts verifies that two calls in
// quick succession return different ports (the kernel allocator gives
// us fresh ports on each Listen). This is a smoke test of the helper
// being usable for parallel test setup.
func TestPickFreePort_ReturnsDifferentPorts(t *testing.T) {
	a, err := PickFreePort()
	if err != nil {
		t.Fatalf("first PickFreePort: %v", err)
	}
	b, err := PickFreePort()
	if err != nil {
		t.Fatalf("second PickFreePort: %v", err)
	}
	if a == b {
		t.Errorf("PickFreePort returned same port twice: %d", a)
	}
}
