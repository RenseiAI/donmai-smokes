package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// BuildOptions configures BuildBinary.
type BuildOptions struct {
	// SourceDir is the directory containing the Go module to build from.
	// `go build` runs with cmd.Dir set to this path. Required.
	SourceDir string

	// EntryPoint is the package path passed to `go build` (e.g.
	// "./cmd/af", "./cmd/rensei"). Required.
	EntryPoint string

	// OutputPath is the absolute path of the produced executable.
	// Required.
	OutputPath string

	// Env is the environment passed to `go build`. If nil, the parent
	// process environment is used. Callers typically append "GOWORK="
	// to the parent env so the build subprocess respects the
	// SourceDir's own go.mod/go.work rather than a workspace overlay
	// at a higher directory.
	Env []string

	// LogSink, when non-nil, receives a tee'd copy of go build's
	// combined stdout+stderr so the caller can render build progress
	// and compile errors in real time alongside other harness output.
	LogSink io.Writer

	// Timeout caps the build duration. Zero defaults to 2 minutes,
	// which is generous on Apple Silicon where rensei/af builds in
	// well under a minute warm.
	Timeout time.Duration
}

// BuildBinary compiles a Go binary via `go build -o <OutputPath> <EntryPoint>`
// from BuildOptions.SourceDir and returns the absolute path of the produced
// executable.
//
// Errors include the captured combined output so the caller can surface
// compile errors without re-running the build to inspect them.
func BuildBinary(ctx context.Context, opts BuildOptions) (string, error) {
	if opts.SourceDir == "" {
		return "", fmt.Errorf("BuildBinary: SourceDir is required")
	}
	if opts.EntryPoint == "" {
		return "", fmt.Errorf("BuildBinary: EntryPoint is required")
	}
	if opts.OutputPath == "" {
		return "", fmt.Errorf("BuildBinary: OutputPath is required")
	}

	absSource, err := filepath.Abs(opts.SourceDir)
	if err != nil {
		return "", fmt.Errorf("resolve source dir %q: %w", opts.SourceDir, err)
	}
	if info, err := os.Stat(absSource); err != nil {
		return "", fmt.Errorf("source dir %s does not exist: %w", absSource, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("source dir %s is not a directory", absSource)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	buildCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if opts.LogSink != nil {
		_, _ = fmt.Fprintf(opts.LogSink,
			"==> BuildBinary: go build -o %s %s (from %s)\n",
			opts.OutputPath, opts.EntryPoint, absSource)
	}

	cmd := exec.CommandContext(buildCtx, "go", "build", "-o", opts.OutputPath, opts.EntryPoint) //nolint:gosec
	cmd.Dir = absSource
	if opts.Env != nil {
		cmd.Env = opts.Env
	}

	var combined bytes.Buffer
	if opts.LogSink != nil {
		cmd.Stdout = io.MultiWriter(opts.LogSink, &combined)
		cmd.Stderr = io.MultiWriter(opts.LogSink, &combined)
	} else {
		cmd.Stdout = &combined
		cmd.Stderr = &combined
	}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build %s in %s: %w\n%s",
			opts.EntryPoint, absSource, err, combined.String())
	}

	absOut, err := filepath.Abs(opts.OutputPath)
	if err != nil {
		return "", fmt.Errorf("resolve output path %q: %w", opts.OutputPath, err)
	}
	if opts.LogSink != nil {
		_, _ = fmt.Fprintf(opts.LogSink, "==> BuildBinary: produced %s\n", absOut)
	}
	return absOut, nil
}
