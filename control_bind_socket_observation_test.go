package smokes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These observations run only after the original immediate bind has failed.
// The netstat writer never retains or prints the global TCP table: it keeps one
// short input line at a time and emits only canonical exact-port loopback rows.
const (
	controlBindSnapshotLimit = 4096
	controlBindLineLimit     = 256
)

func controlBindTCPStateReceipt(port int) string {
	if runtime.GOOS != "darwin" {
		return "target-port tcp-state unavailable: non-Darwin platform"
	}
	const tool = "/usr/sbin/netstat"
	if info, err := os.Stat(tool); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "target-port tcp-state unavailable: netstat tool"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	filter := &controlBindNetstatFilter{port: port}
	cmd := exec.CommandContext(ctx, tool, "-an", "-p", "tcp") //nolint:gosec // fixed system tool, numeric TCP table filtered before logging
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	cmd.Stdout = filter
	cmd.Stderr = io.Discard
	started := time.Now()
	err := cmd.Run()
	return fmt.Sprintf("target-port tcp-state class=%s elapsedMs=%d matched=%d truncated=%t rows=%q",
		controlBindToolClass(ctx, err), time.Since(started).Milliseconds(), filter.matched,
		filter.truncated, filter.rows.String())
}

type controlBindNetstatFilter struct {
	port      int
	line      []byte
	dropping  bool
	rows      strings.Builder
	matched   int
	truncated bool
}

func (f *controlBindNetstatFilter) Write(chunk []byte) (int, error) {
	for _, b := range chunk {
		if b == '\n' {
			if !f.dropping {
				f.accept(string(f.line))
			}
			f.line = f.line[:0]
			f.dropping = false
			continue
		}
		if f.dropping {
			continue
		}
		if len(f.line) >= controlBindLineLimit {
			f.line = f.line[:0]
			f.dropping = true
			continue
		}
		f.line = append(f.line, b)
	}
	return len(chunk), nil
}

func (f *controlBindNetstatFilter) accept(line string) {
	row, ok := controlBindNetstatRow(line, f.port)
	if !ok {
		return
	}
	f.matched++
	if f.rows.Len()+len(row)+1 > controlBindSnapshotLimit {
		f.truncated = true
		return
	}
	f.rows.WriteString(row)
	f.rows.WriteByte('\n')
}

func controlBindNetstatRow(line string, selectedPort int) (string, bool) {
	if selectedPort < 1 || selectedPort > 65535 {
		return "", false
	}
	fields := strings.Fields(line)
	if len(fields) != 6 || (fields[0] != "tcp4" && fields[0] != "tcp6") ||
		!controlBindDecimal(fields[1]) || !controlBindDecimal(fields[2]) ||
		!controlBindTCPState(fields[5]) {
		return "", false
	}
	localHost, localPort, ok := controlBindNetstatEndpoint(fields[3], fields[0])
	if !ok {
		return "", false
	}
	peerHost, peerPort := "*", 0
	if fields[4] != "*.*" {
		peerHost, peerPort, ok = controlBindNetstatEndpoint(fields[4], fields[0])
		if !ok {
			return "", false
		}
	} else if fields[5] != "LISTEN" {
		return "", false
	}
	if localPort != selectedPort && peerPort != selectedPort {
		return "", false
	}
	peer := "*:*"
	if peerPort != 0 {
		peer = fmt.Sprintf("%s:%d", peerHost, peerPort)
	}
	return fmt.Sprintf("%s %s:%d -> %s %s", fields[0], localHost, localPort, peer, fields[5]), true
}

func controlBindNetstatEndpoint(raw, proto string) (string, int, bool) {
	if len(raw) > 64 {
		return "", 0, false
	}
	separator := strings.LastIndexByte(raw, '.')
	if separator < 0 {
		return "", 0, false
	}
	host, portText := raw[:separator], raw[separator+1:]
	if (proto == "tcp4" && host != "127.0.0.1") || (proto == "tcp6" && host != "::1") ||
		!controlBindDecimal(portText) || len(portText) > 5 {
		return "", 0, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, false
	}
	return host, port, true
}

func controlBindDecimal(value string) bool {
	if value == "" || len(value) > 20 {
		return false
	}
	for _, b := range value {
		if b < '0' || b > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func controlBindTCPState(state string) bool {
	switch state {
	case "LISTEN", "ESTABLISHED", "CLOSE_WAIT", "TIME_WAIT", "FIN_WAIT_1", "FIN_WAIT_2",
		"LAST_ACK", "CLOSING", "SYN_SENT", "SYN_RCVD", "CLOSED":
		return true
	default:
		return false
	}
}

// The existing lsof view asks who has the port. This separate query asks only
// whether this smoke's own PID still holds a loopback descriptor for it. An
// empty or unavailable snapshot does not prove that no owner existed at bind.
func controlBindOwnPIDSocketReceipt(port, pid int) string {
	if runtime.GOOS != "darwin" {
		return "own-test-pid lsof unavailable: non-Darwin platform"
	}
	const tool = "/usr/sbin/lsof"
	if info, err := os.Stat(tool); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "own-test-pid lsof unavailable: lsof tool"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, "-nP", "-a", "-p", strconv.Itoa(pid),
		"-iTCP:"+strconv.Itoa(port), "-FpnT") //nolint:gosec // fixed tool, exact current test PID and selected port
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	output := &controlPortCappedOutput{limit: controlBindSnapshotLimit}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return fmt.Sprintf("own-test-pid lsof pid=%d class=%s matched=%d truncated=%t",
		pid, controlBindToolClass(ctx, err), controlBindOwnLsofMatches(output.buf.String(), port, pid), output.truncated)
}

func controlBindOwnLsofMatches(raw string, selectedPort, selectedPID int) int {
	matched := 0
	currentPIDMatches := false
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "p") {
			currentPIDMatches = line == "p"+strconv.Itoa(selectedPID)
			continue
		}
		if !currentPIDMatches || !strings.HasPrefix(line, "n") || len(line) > 160 {
			continue
		}
		endpoints := strings.Split(strings.TrimPrefix(line, "n"), "->")
		if len(endpoints) < 1 || len(endpoints) > 2 {
			continue
		}
		found := false
		valid := true
		for _, endpoint := range endpoints {
			port, ok := controlBindLsofEndpoint(endpoint)
			if !ok {
				valid = false
				break
			}
			found = found || port == selectedPort
		}
		if valid && found {
			matched++
		}
	}
	return matched
}

func controlBindLsofEndpoint(raw string) (int, bool) {
	var portText string
	switch {
	case strings.HasPrefix(raw, "127.0.0.1:"):
		portText = strings.TrimPrefix(raw, "127.0.0.1:")
	case strings.HasPrefix(raw, "[::1]:"):
		portText = strings.TrimPrefix(raw, "[::1]:")
	case strings.HasPrefix(raw, "::1:"):
		portText = strings.TrimPrefix(raw, "::1:")
	default:
		return 0, false
	}
	if !controlBindDecimal(portText) || len(portText) > 5 {
		return 0, false
	}
	port, err := strconv.Atoi(portText)
	return port, err == nil && port > 0 && port <= 65535
}

func controlBindToolClass(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if err == nil {
		return "ok"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return "exit"
	}
	return "unavailable"
}

func TestControlBindSocketObservationFiltersToExactLoopbackPort(t *testing.T) {
	const secret = "secret-context-must-not-appear"
	filter := &controlBindNetstatFilter{port: 49958}
	input := strings.Join([]string{
		"Active Internet connections (including servers)",
		"tcp4 0 0 127.0.0.1.49958 127.0.0.1.54001 TIME_WAIT",
		"tcp4 0 0 127.0.0.1.54001 127.0.0.1.49958 ESTABLISHED",
		"tcp4 0 0 127.0.0.1.49959 127.0.0.1.54001 TIME_WAIT",
		"tcp4 0 0 192.0.2.1.49958 127.0.0.1.54001 LISTEN",
		"tcp4 0 0 127.0.0.1.49958 192.0.2.1.54001 ESTABLISHED",
		"tcp4 0 0 127.0.0.1.49958 127.0.0.1.54001 TIME_WAIT " + secret,
		"tcp4 0 0 127.0.0.1.49958 127.0.0.1.54001 " + secret,
		"tcp4 bad 0 127.0.0.1.49958 *.* LISTEN",
		"tcp4 0 0 127.0.0.1.49958 *.* LISTEN",
		"tcp6 0 0 ::1.49958 ::1.54001 CLOSE_WAIT",
		"tcp6 0 0 fe80::1.49958 ::1.54001 CLOSE_WAIT",
	}, "\n") + "\n"
	for _, chunk := range []string{input[:31], input[31:101], input[101:]} {
		if _, err := filter.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	got := filter.rows.String()
	if filter.matched != 4 || filter.truncated || strings.Contains(got, secret) ||
		strings.Contains(got, "192.0.2.1") || strings.Contains(got, "49959") ||
		!strings.Contains(got, "127.0.0.1:49958") || !strings.Contains(got, "::1:49958") {
		t.Fatalf("exact-port numeric filter = matched %d truncated %t rows %q", filter.matched, filter.truncated, got)
	}
	if count := controlBindOwnLsofMatches("p999\nn127.0.0.1:49958\np123\nn127.0.0.1:54001->127.0.0.1:49958\nn192.0.2.1:49958\nn127.0.0.1:49959\n", 49958, 123); count != 1 {
		t.Fatalf("own PID lsof exact-port count = %d, want 1", count)
	}
	capped := &controlBindNetstatFilter{port: 49958}
	row := "tcp4 0 0 127.0.0.1.49958 127.0.0.1.54001 TIME_WAIT\n"
	for range 100 {
		_, _ = capped.Write([]byte(row))
	}
	if !capped.truncated || capped.rows.Len() > controlBindSnapshotLimit {
		t.Fatal("matching rows were not capped at 4 KiB")
	}
	long := &controlBindNetstatFilter{port: 49958}
	_, _ = long.Write([]byte("tcp4 0 0 127.0.0.1.49958 127.0.0.1.54001 TIME_WAIT " + strings.Repeat(secret, 30) + "\n"))
	if long.matched != 0 || long.rows.Len() != 0 {
		t.Fatal("oversized raw line reached the receipt")
	}
}
