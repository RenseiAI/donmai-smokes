package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMakeBareFixtureRepo verifies the fixture helper yields a bare,
// clonable repo carrying the seeded commit on main plus any extra
// branches — the exact contract step17's sibling-context smoke leans on.
func TestMakeBareFixtureRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	bare := MakeBareFixtureRepo(t, "ctx-fixture", "extra-ref")

	if got := filepath.Base(bare); got != "ctx-fixture.git" {
		t.Errorf("bare repo basename = %q, want ctx-fixture.git", got)
	}
	// A bare repo has HEAD at its top level (no working tree).
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		t.Fatalf("bare repo missing HEAD: %v", err)
	}

	branches, err := exec.Command("git", "-C", bare, "branch", "--list").CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v\n%s", err, branches)
	}
	for _, want := range []string{"main", "extra-ref"} {
		if !strings.Contains(string(branches), want) {
			t.Errorf("branch %q missing from bare repo; got:\n%s", want, branches)
		}
	}

	// The bare repo must be shallow-clonable over the file transport with
	// the seeded content intact — the same shape the runner's sibling
	// provisioning issues (`git clone --depth 1 file://...`).
	dst := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "--depth", "1",
		"file://"+bare, dst).CombinedOutput(); err != nil {
		t.Fatalf("git clone --depth 1: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dst, "README.md"))
	if err != nil {
		t.Fatalf("read cloned README.md: %v", err)
	}
	if !strings.Contains(string(body), "ctx-fixture") {
		t.Errorf("cloned README.md missing seeded fixture name; got %q", body)
	}
}
