package harness

// fixture_repo.go — local bare-git-repo fixtures for smokes that need a
// clonable repository with zero network access. Mirrors the makeBareRepo
// pattern in donmai's runner tests (runner/integration_test.go): init a
// working repo, seed one commit, `git clone --bare` it, and return the
// bare path. Tests point `git clone` / worktree provisioning at the bare
// copy, so nothing ever mutates the seeded fixture in place.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// MakeBareFixtureRepo creates a bare git repository named <name>.git,
// seeded with a single commit on `main` that adds a README.md containing
// the fixture name (so callers can assert clone content), and returns
// the bare repo's absolute path. extraBranches names additional branches
// created at the seeded commit before the bare clone — callers
// exercising `<url>#<ref>` clone syntax check one of these out.
//
// Callers must have verified git is on PATH (t.Skip otherwise); any git
// failure inside this helper is t.Fatal — a broken fixture is a harness
// bug, not a system-under-test signal.
func MakeBareFixtureRepo(t *testing.T, name string, extraBranches ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Commit identity is pinned via env so the helper works on hosts
	// with no global git config (hosted CI).
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=donmai-smokes",
		"GIT_AUTHOR_EMAIL=smokes@donmai.invalid",
		"GIT_COMMITTER_NAME=donmai-smokes",
		"GIT_COMMITTER_EMAIL=smokes@donmai.invalid",
	)
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture repo %s: git %v: %v\n%s", name, args, err, out)
		}
	}

	work := t.TempDir()
	run(work, "init", "-b", "main")
	readme := fmt.Sprintf("# fixture %s\n", name)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte(readme), 0o600); err != nil {
		t.Fatalf("fixture repo %s: write README.md: %v", name, err)
	}
	run(work, "add", ".")
	run(work, "commit", "-m", "seed "+name)
	for _, b := range extraBranches {
		run(work, "branch", b)
	}

	bare := filepath.Join(t.TempDir(), name+".git")
	run(work, "clone", "--bare", work, bare)
	return bare
}
