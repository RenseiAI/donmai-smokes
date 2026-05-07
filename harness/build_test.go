package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildBinary_MissingSourceDir verifies a non-existent SourceDir
// produces a clear error before any go build is attempted.
func TestBuildBinary_MissingSourceDir(t *testing.T) {
	var buf bytes.Buffer
	_, err := BuildBinary(context.Background(), BuildOptions{
		SourceDir:  "/this/path/definitely/does/not/exist",
		EntryPoint: "./cmd/whatever",
		OutputPath: filepath.Join(t.TempDir(), "out"),
		LogSink:    &buf,
		Timeout:    5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error from missing source dir")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBuildBinary_SourceIsFile verifies that pointing SourceDir at a
// file (rather than a directory) produces a clear error.
func TestBuildBinary_SourceIsFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(tmp, []byte("nope"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	var buf bytes.Buffer
	_, err := BuildBinary(context.Background(), BuildOptions{
		SourceDir:  tmp,
		EntryPoint: "./cmd/whatever",
		OutputPath: filepath.Join(t.TempDir(), "out"),
		LogSink:    &buf,
		Timeout:    5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error for file path")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBuildBinary_MissingFields verifies that empty required fields
// produce clear errors.
func TestBuildBinary_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		opts BuildOptions
	}{
		{name: "empty SourceDir", opts: BuildOptions{EntryPoint: "./x", OutputPath: "/tmp/y"}},
		{name: "empty EntryPoint", opts: BuildOptions{SourceDir: ".", OutputPath: "/tmp/y"}},
		{name: "empty OutputPath", opts: BuildOptions{SourceDir: ".", EntryPoint: "./x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildBinary(context.Background(), tc.opts)
			if err == nil {
				t.Fatal("expected error for missing required field")
			}
		})
	}
}
