package harness

import "sync"

// LogTail is an io.Writer that retains the last N bytes of the data
// written through it.
//
// Used to tee a spawned process's combined stdout+stderr into a bounded
// buffer so failed assertions can attach the trailing daemon log to the
// failure message — much faster than asking the developer to dig through
// the operator's real log directory.
//
// Safe for concurrent writes; reads via String are also locked. Mid-rune
// truncation is acceptable for log-tail use.
type LogTail struct {
	mu   sync.Mutex
	cap  int
	data []byte
	wPos int // next write index when data is full
	full bool
}

// NewLogTail allocates a ring buffer with the supplied byte cap.
//
// A non-positive cap defaults to 8 KiB. Callers spawning a long-running
// daemon typically pass 64 KiB so the trailing logs are large enough to
// surface a startup failure cause without the buffer growing without
// bound.
func NewLogTail(cap int) *LogTail {
	if cap <= 0 {
		cap = 8 * 1024
	}
	return &LogTail{cap: cap, data: make([]byte, 0, cap)}
}

// Write implements io.Writer. Bytes beyond cap displace the oldest bytes
// in FIFO order. Mid-rune truncation is acceptable for log tail use.
func (b *LogTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(p)
	if n == 0 {
		return 0, nil
	}

	// Fast path: still room.
	if !b.full && len(b.data)+n <= b.cap {
		b.data = append(b.data, p...)
		if len(b.data) == b.cap {
			b.full = true
		}
		return n, nil
	}

	// Slow path: ring write. Allocate the underlying array eagerly to
	// cap if not yet there, then advance wPos with wrap-around.
	if cap(b.data) < b.cap {
		grown := make([]byte, b.cap)
		copy(grown, b.data)
		b.wPos = len(b.data)
		b.data = grown
		b.full = b.wPos >= b.cap
		if b.full {
			b.wPos = 0
		}
	}
	// p might be larger than cap; in that case only the trailing cap
	// bytes can survive the write.
	if n >= b.cap {
		copy(b.data, p[n-b.cap:])
		b.wPos = 0
		b.full = true
		return n, nil
	}
	// Two-segment write across the wrap.
	first := copy(b.data[b.wPos:], p)
	b.wPos += first
	if b.wPos == b.cap {
		b.wPos = 0
		b.full = true
	}
	if first < n {
		second := copy(b.data, p[first:])
		b.wPos = second
	}
	return n, nil
}

// String returns the current log tail in chronological order.
func (b *LogTail) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.full {
		return string(b.data)
	}
	out := make([]byte, 0, b.cap)
	out = append(out, b.data[b.wPos:]...)
	out = append(out, b.data[:b.wPos]...)
	return string(out)
}
