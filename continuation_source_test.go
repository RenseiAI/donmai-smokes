package smokes

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// The continuation API belongs to the selected in-flight source, while the
// root module deliberately consumes the immutable released Donmai module.
//
//go:embed testdata/continuation/*_test.go
var continuationChildSources embed.FS

//go:embed testdata/continuation_result/control_test.go
var continuationResultControl string

const continuationParityTest = "TestContinuationCheckpointPTYParity"

func TestContinuationCheckpointPTYParitySource(t *testing.T) {
	afh.SkipIfShort(t, "source-built real PTY continuation parity smoke")
	sourceDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	moduleDir := writeContinuationChildModule(t, sourceDir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := runContinuationGo(ctx, moduleDir, "mod", "tidy"); err != nil {
		t.Fatalf("prepare continuation consumer against %s: %v\n%s", sourceDir, err, out)
	}
	out, err := runContinuationGo(ctx, moduleDir, "test", "-race", "-count=1", "-json", "-run", "^"+continuationParityTest+"$", ".")
	if err != nil {
		t.Fatalf("run continuation parity against %s: %v\n%s", sourceDir, err, out)
	}
	if err := continuationChildPassed(out, continuationParityTest); err != nil {
		t.Fatalf("continuation child did not exercise parity against %s: %v\n%s", sourceDir, err, out)
	}
	t.Logf("child go test -race: %s RUN and PASS", continuationParityTest)
	afh.RecordLive(t.Name(), afh.LiveExercised, "real PTY continuation parity against "+sourceDir)
}

func writeContinuationChildModule(t *testing.T, sourceDir string) string {
	t.Helper()
	moduleBytes, err := os.ReadFile(filepath.Join(sourceDir, "go.mod"))
	if err != nil {
		t.Fatalf("read donmai go.mod: %v", err)
	}
	version := ""
	for _, line := range strings.Split(string(moduleBytes), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			version = fields[1]
			break
		}
	}
	if version == "" {
		t.Fatal("donmai go.mod lacks a go directive")
	}
	moduleDir := t.TempDir()
	goMod := fmt.Sprintf("module donmai-smokes-continuation-consumer\n\ngo %s\n\nrequire github.com/RenseiAI/donmai v0.0.0-00010101000000-000000000000\n\nreplace github.com/RenseiAI/donmai => %s\n", version, sourceDir)
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write continuation consumer go.mod: %v", err)
	}
	entries, err := continuationChildSources.ReadDir("testdata/continuation")
	if err != nil {
		t.Fatalf("read embedded continuation sources: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("unexpected continuation source directory %s", entry.Name())
		}
		contents, err := continuationChildSources.ReadFile("testdata/continuation/" + entry.Name())
		if err != nil {
			t.Fatalf("read continuation source %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(moduleDir, entry.Name()), contents, 0o600); err != nil {
			t.Fatalf("write continuation source %s: %v", entry.Name(), err)
		}
	}
	return moduleDir
}

func runContinuationGo(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // fixed test command and validated source module.
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	return cmd.CombinedOutput()
}

func continuationChildPassed(output []byte, name string) error {
	type event struct {
		Action string
		Test   string
	}
	seenRun, seenPass, packagePass := false, false, false
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var e event
		if err := decoder.Decode(&e); err != nil {
			return fmt.Errorf("decode child go test event: %w", err)
		}
		if e.Action == "skip" && (e.Test == name || strings.HasPrefix(e.Test, name+"/")) {
			return fmt.Errorf("child test %s skipped", e.Test)
		}
		if e.Test == name {
			switch e.Action {
			case "run":
				seenRun = true
			case "pass":
				seenPass = true
			}
		}
		if e.Test == "" && e.Action == "pass" {
			packagePass = true
		}
	}
	if !seenRun || !seenPass || !packagePass {
		return fmt.Errorf("child test %s did not run and pass (run=%t pass=%t package-pass=%t)", name, seenRun, seenPass, packagePass)
	}
	return nil
}

func TestContinuationChildResultRefusesNonExecution(t *testing.T) {
	moduleDir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":          "module continuation-child-result-control\n\ngo 1.26\n",
		"control_test.go": continuationResultControl,
	} {
		if err := os.WriteFile(filepath.Join(moduleDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write child result control %s: %v", name, err)
		}
	}
	for _, tc := range []struct{ name, pattern, target, want string }{
		{name: "zero matched tests", pattern: "^NoSuchTest$", target: "NoSuchTest", want: "did not run and pass"},
		{name: "skipped named test", pattern: "^TestSkip$", target: "TestSkip", want: "child test TestSkip skipped"},
		{name: "skipped subtest", pattern: "^TestSubtestSkip$", target: "TestSubtestSkip", want: "child test TestSubtestSkip/inner skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			out, err := runContinuationGo(ctx, moduleDir, "test", "-race", "-count=1", "-json", "-run", tc.pattern, ".")
			if err != nil {
				t.Fatalf("run child result control: %v\n%s", err, out)
			}
			if err := continuationChildPassed(out, tc.target); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("child %s refusal = %v, want %q:\n%s", tc.name, err, tc.want, out)
			}
			t.Logf("child %s refused as expected: %s", tc.name, tc.want)
		})
	}
}
