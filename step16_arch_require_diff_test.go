package smokes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

func TestArchRequireDiffEndToEnd(t *testing.T) {
	afh.SkipIfShort(t, "end-to-end strict arch assessment")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	binary, _ := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{
		SourceDir: inFlightSourceDir(), OutputPath: filepath.Join(t.TempDir(), "donmai"),
	})
	const view = `{"title":"Change","body":"","changedFiles":1,"files":[{"path":"src/auth/login.ts","additions":1,"deletions":0}]}`
	for _, tc := range []struct {
		name     string
		script   string
		policy   string
		wantExit int
		wantText string
	}{
		{name: "no GitHub CLI", policy: "none", wantExit: 2, wantText: "required PR diff unavailable"},
		{name: "authentication refused", script: "#!/bin/sh\necho authentication-required >&2\nexit 4\n", policy: "none", wantExit: 2, wantText: "authentication-required"},
		{name: "patch retrieval refused", script: "#!/bin/sh\nif [ \"$2\" = view ]; then printf '%s' '" + view + "'; else echo patch-download-failed >&2; exit 1; fi\n", policy: "none", wantExit: 2, wantText: "patch-download-failed"},
		{name: "real diff analyzed", script: strictArchFixture(view), policy: "none", wantText: `"native-diff-only"`},
		{name: "real diff gated", script: strictArchFixture(view), policy: "zero-deviations", wantExit: 1, wantText: `"gated": true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.script != "" {
				if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(tc.script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			r := runArchAssess(t, binary, dir, 15*time.Second,
				[]string{"HOME=" + dir, "PATH=" + dir, "DONMAI_ARCH_BIN="},
				"--require-diff", "--gate-policy", tc.policy)
			if r.timedOut || r.exitCode != tc.wantExit || !strings.Contains(r.combined, tc.wantText) {
				t.Fatalf("exit=%d want=%d timeout=%t output=%s", r.exitCode, tc.wantExit, r.timedOut, r.combined)
			}
			if tc.wantExit == 2 {
				if r.stdout != "" {
					t.Fatalf("failed assessment emitted a successful result: %s", r.stdout)
				}
				return
			}
			var report archAssessResult
			if err := afh.JSONUnmarshal(r.stdout, &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Observations) == 0 || report.Gated != (tc.wantExit == 1) {
				t.Fatalf("patch was not analyzed under requested policy: %s", r.stdout)
			}
		})
	}
}

func strictArchFixture(view string) string {
	return "#!/bin/sh\nif [ \"$2\" = view ]; then printf '%s' '" + view + "'; else\n" +
		"printf '%s\\n' 'diff --git a/src/auth/login.ts b/src/auth/login.ts' '@@ -0,0 +1 @@' '+const r: Result<User, Error> = ok(user)'\nfi\n"
}

const archPaginationGH = `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$ARCH_GH_CALLS"
if [ "$#" -eq 5 ] && [ "$1" = pr ] && [ "$2" = view ] && [ "$3" = "$ARCH_GH_REF" ] && [ "$4" = --json ]; then
  case "$5" in
    title,body,changedFiles,files|title,body,files) cat "$ARCH_GH_VIEW"; exit 0 ;;
  esac
fi
if [ "$#" -eq 3 ] && [ "$1" = api ] && [ "$2" = --paginate ] && [ "$3" = 'repos/acme/widgets/pulls/42/files?per_page=100' ]; then
  cat "$ARCH_GH_FILES"
  exit 0
fi
if [ "$#" -eq 3 ] && [ "$1" = pr ] && [ "$2" = diff ] && [ "$3" = "$ARCH_GH_REF" ]; then
  cat "$ARCH_GH_DIFF"
  exit 0
fi
printf 'UNEXPECTED_GH_CALL:%s\n' "$*" >&2
exit 91
`

func writeArchPaginationFixture(t *testing.T, apiCount, patchCount int, pageArrays bool) (string, []string, string) {
	t.Helper()
	const changedFiles = 108
	if apiCount < 100 || apiCount > changedFiles || patchCount < 0 || patchCount > changedFiles {
		t.Fatal("invalid pagination fixture dimensions")
	}
	dir := t.TempDir()
	viewFiles := make([]map[string]any, 0, 100)
	apiFiles := make([]map[string]any, 0, apiCount)
	var diff strings.Builder
	for i := range changedFiles {
		path := fmt.Sprintf("src/auth/file-%03d.go", i)
		if i < 100 {
			viewFiles = append(viewFiles, map[string]any{"path": path, "additions": 1, "deletions": 0})
		}
		if i < apiCount {
			apiFiles = append(apiFiles, map[string]any{"filename": path, "additions": 1, "deletions": 0})
		}
		if i < patchCount {
			added := fmt.Sprintf("const value%d = %d", i, i)
			if i == changedFiles-1 {
				added = "const result: Result<User, Error> = ok(user)"
			}
			fmt.Fprintf(&diff, "diff --git a/%s b/%s\n@@ -0,0 +1 @@\n+%s\n", path, path, added)
		}
	}
	view, err := json.Marshal(map[string]any{
		"title": "Review 108 auth files", "body": "", "changedFiles": changedFiles, "files": viewFiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	api, err := json.Marshal(apiFiles)
	if err != nil {
		t.Fatal(err)
	}
	if pageArrays && apiCount > 100 {
		first, err := json.Marshal(apiFiles[:100])
		if err != nil {
			t.Fatal(err)
		}
		second, err := json.Marshal(apiFiles[100:])
		if err != nil {
			t.Fatal(err)
		}
		api = append(append(first, '\n'), second...)
	}
	paths := map[string][]byte{
		"gh":         []byte(archPaginationGH),
		"view.json":  view,
		"files.json": api,
		"diff.txt":   []byte(diff.String()),
		"calls.log":  nil,
	}
	for name, data := range paths {
		mode := os.FileMode(0o600)
		if name == "gh" {
			mode = 0o700
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, mode); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	calls := filepath.Join(dir, "calls.log")
	env := []string{
		"HOME=" + dir,
		"DONMAI_ARCH_BIN=",
		"ARCH_GH_REF=" + fixturePRURL,
		"ARCH_GH_VIEW=" + filepath.Join(dir, "view.json"),
		"ARCH_GH_FILES=" + filepath.Join(dir, "files.json"),
		"ARCH_GH_DIFF=" + filepath.Join(dir, "diff.txt"),
		"ARCH_GH_CALLS=" + calls,
	}
	return dir, env, calls
}

func TestArchRequireDiffPaginatedPR(t *testing.T) {
	afh.SkipIfShort(t, "compiled large-PR strict arch assessment")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	binary, source := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{
		SourceDir: inFlightSourceDir(), OutputPath: filepath.Join(t.TempDir(), "donmai"),
	})
	t.Logf("compiled donmai from %s", source)
	for _, tc := range []struct {
		name       string
		apiCount   int
		patchCount int
		pageArrays bool
		wantExit   int
		wantText   string
	}{
		{name: "merged 108-file response", apiCount: 108, patchCount: 108},
		{name: "two page arrays", apiCount: 108, patchCount: 108, pageArrays: true},
		{name: "missing second page", apiCount: 100, patchCount: 108, wantExit: 2, wantText: "incomplete changed-file list: got 100, want 108"},
		{name: "missing final patch", apiCount: 108, patchCount: 107, wantExit: 2, wantText: "patch sections do not match the changed-file list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, env, callsPath := writeArchPaginationFixture(t, tc.apiCount, tc.patchCount, tc.pageArrays)
			r := runArchAssess(t, binary, dir, 30*time.Second, env,
				"--require-diff", "--gate-policy", "none")
			if r.timedOut || r.exitCode != tc.wantExit || strings.Contains(r.combined, "UNEXPECTED_GH_CALL") {
				t.Fatalf("exit=%d want=%d timeout=%t output=%s", r.exitCode, tc.wantExit, r.timedOut, r.combined)
			}
			calls, err := os.ReadFile(callsPath)
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := []string{
				"pr view " + fixturePRURL + " --json title,body,changedFiles,files",
				"api --paginate repos/acme/widgets/pulls/42/files?per_page=100",
			}
			if tc.apiCount == 108 {
				wantCalls = append(wantCalls, "pr diff "+fixturePRURL)
			}
			if got := strings.TrimSpace(string(calls)); got != strings.Join(wantCalls, "\n") {
				t.Fatalf("gh calls = %q, want %q; output=%s", got, strings.Join(wantCalls, "\n"), r.combined)
			}
			if tc.wantExit == 2 {
				if r.stdout != "" || !strings.Contains(r.combined, tc.wantText) {
					t.Fatalf("strict failure did not name the incomplete data; stdout=%q output=%s", r.stdout, r.combined)
				}
				return
			}
			var report struct {
				Mode         string `json:"mode"`
				Observations []struct {
					Kind    string `json:"kind"`
					Payload struct {
						Title string `json:"title"`
					} `json:"payload"`
				} `json:"observations"`
			}
			if err := json.Unmarshal([]byte(r.stdout), &report); err != nil {
				t.Fatalf("decode native report: %v\n%s", err, r.stdout)
			}
			if report.Mode != "native-diff-only" {
				t.Fatalf("mode=%q, want native-diff-only: %s", report.Mode, r.stdout)
			}
			foundFinalPatch := false
			for _, observation := range report.Observations {
				if observation.Kind == "convention" && observation.Payload.Title == "Result<T, E> error handling" {
					foundFinalPatch = true
				}
			}
			if !foundFinalPatch {
				t.Fatalf("final file's Result convention patch was not analyzed: %s", r.stdout)
			}
		})
	}
}
