package smokes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

const nativeLargePRGH = `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$ARCH_LARGE_CALLS"
if [ "$#" -eq 5 ] && [ "$1" = pr ] && [ "$2" = view ] && [ "$3" = 'https://github.com/example/project/pull/7' ] && [ "$4" = --json ] && [ "$5" = title,body,changedFiles,files ]; then
  cat "$ARCH_LARGE_VIEW"
  exit 0
fi
if [ "$#" -eq 3 ] && [ "$1" = api ] && [ "$2" = --paginate ] && [ "$3" = 'repos/example/project/pulls/7/files?per_page=100' ]; then
  cat "$ARCH_LARGE_FILES"
  exit 0
fi
if [ "$#" -eq 3 ] && [ "$1" = pr ] && [ "$2" = diff ] && [ "$3" = 'https://github.com/example/project/pull/7' ]; then
  printf '%s\n' 'HTTP 406: diff exceeded 20000 lines' >&2
  exit 1
fi
printf '%s\n' 'UNEXPECTED_GH_CALL' >&2
exit 91
`

// Only the external gh boundary is finite. The built CLI must fetch, validate,
// analyze and report all REST patches after the full-diff endpoint refuses them.
func TestNativeLargePRRESTPatchCLI(t *testing.T) {
	afh.SkipIfShort(t, "compiled strict large-PR REST patch assessment")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	binary, source := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{
		SourceDir: inFlightSourceDir(), OutputPath: filepath.Join(t.TempDir(), "donmai"),
	})
	t.Logf("compiled strict large-PR CLI SUT from %s", source)
	for _, tc := range []struct {
		name      string
		lastPatch string
		lastAdded int
		wantError string
	}{
		{name: "complete REST after full diff HTTP406", lastPatch: "@@ -0,0 +1 @@\n+const result: Result<User, Error> = ok(user)", lastAdded: 1},
		{name: "malformed REST header refused", lastPatch: "@@ not-a-hunk @@\n+const result: Result<User, Error> = ok(user)", lastAdded: 1, wantError: "malformed REST patch hunk header"},
		{name: "truncated REST hunk refused", lastPatch: "@@ -0,0 +1,2 @@\n+const result: Result<User, Error> = ok(user)", lastAdded: 2, wantError: "incomplete REST patch hunk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			viewFiles := make([]map[string]any, 0, 100)
			restFiles := make([]map[string]any, 0, 130)
			for i := range 130 {
				path := fmt.Sprintf("src/file-%03d.go", i)
				patch, added := "@@ -0,0 +1 @@\n+package example", 1
				if i == 129 {
					patch, added = tc.lastPatch, tc.lastAdded
				}
				restFiles = append(restFiles, map[string]any{"filename": path, "additions": added, "deletions": 0, "patch": patch})
				if i < 100 {
					viewFiles = append(viewFiles, map[string]any{"path": path, "additions": 1, "deletions": 0})
				}
			}
			view, err := json.Marshal(map[string]any{"title": "Change files", "body": "", "changedFiles": 130, "files": viewFiles})
			if err != nil {
				t.Fatal(err)
			}
			first, err := json.Marshal(restFiles[:100])
			if err != nil {
				t.Fatal(err)
			}
			second, err := json.Marshal(restFiles[100:])
			if err != nil {
				t.Fatal(err)
			}
			for name, raw := range map[string][]byte{"gh": []byte(nativeLargePRGH), "view.json": view, "files.json": append(append(first, '\n'), second...), "calls.log": nil} {
				mode := os.FileMode(0o600)
				if name == "gh" {
					mode = 0o700
				}
				if err := os.WriteFile(filepath.Join(dir, name), raw, mode); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "arch", "assess", "https://github.com/example/project/pull/7", "--require-diff", "--gate-policy", "none")
			cmd.Dir = dir
			cmd.Env = []string{"HOME=" + dir, "PATH=" + dir + ":/usr/bin:/bin", "DONMAI_ARCH_BIN=", "ARCH_LARGE_CALLS=" + filepath.Join(dir, "calls.log"), "ARCH_LARGE_VIEW=" + filepath.Join(dir, "view.json"), "ARCH_LARGE_FILES=" + filepath.Join(dir, "files.json")}
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			runErr := cmd.Run()
			if ctx.Err() != nil {
				t.Fatalf("actual strict CLI deadline: %v; stderr=%s", ctx.Err(), stderr.String())
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := "pr view https://github.com/example/project/pull/7 --json title,body,changedFiles,files\napi --paginate repos/example/project/pulls/7/files?per_page=100\npr diff https://github.com/example/project/pull/7\n"
			if string(calls) != wantCalls || strings.Contains(stderr.String(), "UNEXPECTED_GH_CALL") {
				t.Fatalf("actual gh request sequence changed: %q; stderr=%s", calls, stderr.String())
			}
			if tc.wantError != "" {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.wantError) {
					t.Fatalf("incomplete REST must fail closed without a result: err=%v stdout=%s stderr=%s", runErr, stdout.String(), stderr.String())
				}
				return
			}
			if runErr != nil {
				t.Fatalf("complete REST CLI failed: %v; stderr=%s", runErr, stderr.String())
			}
			var report struct {
				Mode   string `json:"mode"`
				Gated  *bool  `json:"gated"`
				Change struct {
					Repository string `json:"repository"`
					PRNumber   int    `json:"prNumber"`
				} `json:"change"`
				Observations []struct {
					Kind    string `json:"kind"`
					Payload struct {
						Title string `json:"title"`
					} `json:"payload"`
				} `json:"observations"`
			}
			if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
				t.Fatal(err)
			}
			if report.Mode != "native-diff-only" || report.Gated == nil || *report.Gated || report.Change.Repository != "github.com/example/project" || report.Change.PRNumber != 7 {
				t.Fatalf("strict native assessment identity/policy mismatch: %s", stdout.String())
			}
			found := false
			for _, observation := range report.Observations {
				if observation.Kind == "convention" && observation.Payload.Title == "Result<T, E> error handling" {
					found = true
				}
			}
			if !found {
				t.Fatalf("final REST-only convention did not reach native analysis: %s", stdout.String())
			}
		})
	}
	afh.RecordLive(t.Name(), afh.LiveExercised, "actual compiled arch assess strict130-file REST patches after full-diff HTTP406 plus malformed/truncated refusal")
}
