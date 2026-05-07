package harness

import (
	"fmt"
	"net"
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
