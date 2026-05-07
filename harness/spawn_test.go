package harness

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLiveDaemon_StopIdempotent verifies that a LiveDaemon's Stop is safe
// to call multiple times. The first call performs the actual stop
// sequence; subsequent calls return without re-invoking the underlying
// stopFn (which would Wait on an already-reaped process).
//
// Constructed manually rather than via SpawnDaemon because we don't need
// a real daemon process — sync.Once guarding stopFn invocations is what
// matters, and the assertion is purely about how many times stopFn ran.
func TestLiveDaemon_StopIdempotent(t *testing.T) {
	var calls atomic.Int32
	d := &LiveDaemon{
		URL: "http://127.0.0.1:9999",
		stopFn: func() {
			calls.Add(1)
		},
	}

	d.Stop()
	d.Stop()
	d.Stop()

	if got := calls.Load(); got != 1 {
		t.Errorf("stopFn invoked %d times; want 1", got)
	}
}

// TestLiveDaemon_StopConcurrentIdempotent verifies that even under
// concurrent Stop() calls, the underlying stopFn fires exactly once.
// This is the classic sync.Once semantics test — if any caller skips
// the Once, two stopFn invocations race the cmd.Wait inside.
func TestLiveDaemon_StopConcurrentIdempotent(t *testing.T) {
	var calls atomic.Int32
	d := &LiveDaemon{
		URL: "http://127.0.0.1:9999",
		stopFn: func() {
			// Sleep briefly so concurrent callers actually overlap; otherwise
			// Go's scheduler can serialize them and the test passes for the
			// wrong reason.
			time.Sleep(10 * time.Millisecond)
			calls.Add(1)
		},
	}

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			d.Stop()
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("stopFn invoked %d times under %d-way concurrent Stop; want 1",
			got, goroutines)
	}
}
