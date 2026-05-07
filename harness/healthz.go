package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// WaitForDaemonHealthz polls GET <baseURL>/healthz at 100ms intervals
// until ctx fires or the endpoint returns 200.
//
// Network-level errors (refused, unreachable) are tolerated until the
// timeout — they're the expected shape during the daemon's HTTP server
// bind phase. Any 200 response from /healthz returns nil; non-200
// responses keep polling until the deadline.
//
// Callers typically pass a 30-second timeout via ctx — generous compared
// to the ~1s warm / ~5s cold daemon startup observed in practice.
func WaitForDaemonHealthz(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 1 * time.Second}
	url := strings.TrimRight(baseURL, "/") + "/healthz"

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build healthz request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return errIfTimeout(ctx, ctx.Err())
		case <-ticker.C:
		}
	}
}

// errIfTimeout returns nil when err is nil, ctx.Err() when the deadline
// fired, or err otherwise. Helps step bodies report timeout cleanly.
func errIfTimeout(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ctx.Err()
	}
	return err
}
