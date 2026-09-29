package interactive

import (
	"io"
	"log/slog"
)

// discardLogger silences PTY diagnostics in the child smoke process.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
