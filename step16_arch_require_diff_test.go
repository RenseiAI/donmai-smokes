package smokes

import (
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
	const view = `{"title":"Change","body":"","files":[{"path":"src/auth/login.ts","additions":1,"deletions":0}]}`
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
