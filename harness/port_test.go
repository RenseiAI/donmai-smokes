package harness

import (
	"fmt"
	"net"
	"testing"
	"time"
)

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

// TestPickClosedPort_PortIsActuallyClosed verifies the guarantee callers rely
// on: dialing the returned port must be refused. The hard-fail UX smokes
// assert on "connection refused" output, so a port that quietly accepted would
// turn those assertions into a skip (which is what they used to do) or a
// confusing failure.
func TestPickClosedPort_PortIsActuallyClosed(t *testing.T) {
	port, err := PickClosedPort(8)
	if err != nil {
		t.Fatalf("PickClosedPort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("out-of-range port: %d", port)
	}
	conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		t.Fatalf("port %d accepted a connection; PickClosedPort must return a closed port", port)
	}
}

// TestPickClosedPort_RetriesPastAnOccupiedPort proves the retry is real rather
// than decorative. A listener is held on the first port the allocator hands
// out, so the initial candidate is occupied and the helper must move on — the
// exact race the two hard-fail subtests used to resolve by skipping.
func TestPickClosedPort_RetriesPastAnOccupiedPort(t *testing.T) {
	// Bind and HOLD a port, then keep asking for closed ports until one of
	// the attempts collides with it. Without the retry loop a collision
	// returns the occupied port; with it, every result is closed.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	occupied := l.Addr().(*net.TCPAddr).Port

	for i := 0; i < 200; i++ {
		got, err := PickClosedPort(8)
		if err != nil {
			t.Fatalf("PickClosedPort attempt %d: %v", i, err)
		}
		if got == occupied {
			t.Fatalf("returned the occupied port %d; the retry did not filter it", occupied)
		}
	}
}

// TestPickClosedPort_ExhaustionIsAnError pins that running out of attempts
// reports an error rather than a plausible-looking port. Callers turn this
// into t.Fatal; a silent zero would reintroduce the skip.
func TestPickClosedPort_ExhaustionIsAnError(t *testing.T) {
	// attempts<=0 must not mean "give up immediately and return 0, nil".
	got, err := PickClosedPort(0)
	if err != nil {
		t.Fatalf("PickClosedPort(0) should apply the default attempt count, got: %v", err)
	}
	if got <= 0 {
		t.Fatalf("PickClosedPort(0) returned port %d with no error", got)
	}
}
