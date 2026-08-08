package harness

import (
	"fmt"
	"net"
	"time"
)

// PickFreePort asks the kernel for a free TCP port on the loopback
// interface. The returned port is closed before it is returned, so there
// is a small TOCTOU window — but if the caller's binding step is the
// next thing it does (the typical pattern), the practical race is zero.
//
// Using the kernel's port allocator is the standard idiom for tests that
// need a real listener and can't rely on a fixed port. Useful when
// spawning multiple daemon processes in parallel test runs.
func PickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen for free port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// PickClosedPort returns a loopback port with nothing listening on it,
// confirmed by an actual dial rather than assumed from PickFreePort's TOCTOU
// window. Callers that need a guaranteed connection-refused (hard-fail UX
// smokes, doctor paths) use this instead of dialing PickFreePort's result and
// hoping.
//
// It retries: a port that lost the race is a reason to pick another one, not
// a reason to abandon the assertion. The hard-fail smokes previously skipped
// on a lost race — a rare, invisible hole in exactly the coverage they exist
// to provide, and one that no reader of a green run would ever suspect.
// Exhausting every attempt returns an error, which callers surface as a
// failure.
func PickClosedPort(attempts int) (int, error) {
	if attempts <= 0 {
		attempts = 8
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		port, err := PickFreePort()
		if err != nil {
			lastErr = err
			continue
		}
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if dialErr != nil {
			// Nothing accepted the connection: the port is genuinely closed.
			return port, nil
		}
		_ = conn.Close()
		lastErr = fmt.Errorf("port %d was taken between allocation and probe", port)
	}
	return 0, fmt.Errorf("no closed loopback port after %d attempts: %w", attempts, lastErr)
}
