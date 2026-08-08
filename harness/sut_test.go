package harness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeCheckout materialises the minimum a directory needs to satisfy
// describeNonDonmaiCheckout: a go.mod declaring the donmai module path and a
// cmd/donmai package directory.
func writeFakeCheckout(t *testing.T, dir, modulePath string, withCmd bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "module " + modulePath + "\n\ngo 1.25.12\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if withCmd {
		if err := os.MkdirAll(filepath.Join(dir, "cmd", "donmai"), 0o755); err != nil {
			t.Fatalf("mkdir cmd/donmai: %v", err)
		}
	}
}

// samePath compares two paths through EvalSymlinks: on macOS t.TempDir hands
// back /var/... while os.Getwd reports the resolved /private/var/....
func samePath(t *testing.T, got, want string) bool {
	t.Helper()
	g, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("eval %s: %v", got, err)
	}
	w, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("eval %s: %v", want, err)
	}
	return g == w
}

// TestLocateDonmaiSource_WalkUpFindsSiblingFromWorktree is the regression test
// for the bug this file was written for: from <root>/donmai-smokes.wt/<name>,
// the sibling checkout is at ../../donmai, and the old fixed "../donmai"
// missed it. The tree below reproduces that exact layout.
func TestLocateDonmaiSource_WalkUpFindsSiblingFromWorktree(t *testing.T) {
	t.Setenv(DonmaiSourceDirEnv, "")

	root := t.TempDir()
	checkout := filepath.Join(root, "donmai")
	writeFakeCheckout(t, checkout, donmaiModulePath, true)

	for _, tc := range []struct {
		name string
		cwd  string
	}{
		{"primary checkout", filepath.Join(root, "donmai-smokes")},
		{"linked worktree", filepath.Join(root, "donmai-smokes.wt", "some-lane")},
		{"nested worktree", filepath.Join(root, "donmai-smokes.wt", "a", "b")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(tc.cwd, 0o755); err != nil {
				t.Fatalf("mkdir cwd: %v", err)
			}
			t.Chdir(tc.cwd)

			got, err := LocateDonmaiSource()
			if err != nil {
				t.Fatalf("LocateDonmaiSource from %s: %v", tc.cwd, err)
			}
			if !samePath(t, got, checkout) {
				t.Errorf("resolved %q, want %q", got, checkout)
			}
		})
	}
}

// TestLocateDonmaiSource_NotFoundIsAnError pins the half of the fix that the
// ledger cannot cover: when there is no checkout anywhere up the tree, the
// answer is an error naming what was tried — never a nil error with some
// plausible-looking path the caller would then fail to build.
func TestLocateDonmaiSource_NotFoundIsAnError(t *testing.T) {
	t.Setenv(DonmaiSourceDirEnv, "")

	// A deep-enough empty tree that the bounded walk cannot reach a real
	// donmai checkout on the developer's machine.
	deep := filepath.Join(t.TempDir(), "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(deep)

	got, err := LocateDonmaiSource()
	if err == nil {
		t.Fatalf("LocateDonmaiSource returned %q with no error; want a *SUTNotFoundError", got)
	}

	var notFound *SUTNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("error is %T, want *SUTNotFoundError: %v", err, err)
	}
	if len(notFound.Candidates) == 0 {
		t.Error("SUTNotFoundError lists no candidates; the message must name where it looked")
	}
	// The message has to be actionable without a second run.
	for _, want := range []string{"gh repo clone", DonmaiSourceDirEnv, SkipLiveDaemonEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message omits %q; got:\n%s", want, err)
		}
	}
}

// TestLocateDonmaiSource_RejectsLookalikeDirectory proves resolution is by
// IDENTITY, not by name. A bare directory called "donmai" — a leftover, an
// empty clone target — must not be accepted, because accepting it would put
// the failure at build time where the old skip heuristic used to swallow it.
func TestLocateDonmaiSource_RejectsLookalikeDirectory(t *testing.T) {
	t.Setenv(DonmaiSourceDirEnv, "")

	root := t.TempDir()
	// Same name, wrong contents.
	if err := os.MkdirAll(filepath.Join(root, "donmai"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cwd := filepath.Join(root, "donmai-smokes")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(cwd)

	if got, err := LocateDonmaiSource(); err == nil {
		t.Fatalf("accepted a directory named donmai with no go.mod: %q", got)
	}
}

// TestLocateDonmaiSource_EnvOverrideIsAuthoritative pins that a set-but-wrong
// override fails instead of falling back. Falling back would build a
// different commit than the operator named, and report ok for it.
func TestLocateDonmaiSource_EnvOverrideIsAuthoritative(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "donmai")
	writeFakeCheckout(t, real, donmaiModulePath, true)
	cwd := filepath.Join(root, "donmai-smokes")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(cwd)

	// Sanity: without the override this tree resolves. If this failed, the
	// negative case below would pass for the wrong reason.
	if _, err := LocateDonmaiSource(); err != nil {
		t.Fatalf("precondition: tree must resolve without the override: %v", err)
	}

	t.Setenv(DonmaiSourceDirEnv, filepath.Join(root, "nowhere-at-all"))
	got, err := LocateDonmaiSource()
	if err == nil {
		t.Fatalf("bad %s fell back to %q; the override must be authoritative", DonmaiSourceDirEnv, got)
	}
	var notFound *SUTNotFoundError
	if !errors.As(err, &notFound) || !notFound.FromEnv {
		t.Errorf("error should be a *SUTNotFoundError with FromEnv set; got %T %v", err, err)
	}
}

func TestDescribeNonDonmaiCheckout(t *testing.T) {
	base := t.TempDir()

	good := filepath.Join(base, "good")
	writeFakeCheckout(t, good, donmaiModulePath, true)

	noCmd := filepath.Join(base, "no-cmd")
	writeFakeCheckout(t, noCmd, donmaiModulePath, false)

	wrongModule := filepath.Join(base, "wrong-module")
	writeFakeCheckout(t, wrongModule, "github.com/RenseiAI/donmai-smokes", true)

	noGoMod := filepath.Join(base, "no-go-mod")
	if err := os.MkdirAll(noGoMod, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	aFile := filepath.Join(base, "a-file")
	if err := os.WriteFile(aFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, tc := range []struct {
		name       string
		dir        string
		wantReason string // "" means "must be accepted"
	}{
		{"valid checkout", good, ""},
		{"missing", filepath.Join(base, "absent"), "does not exist"},
		{"a file, not a directory", aFile, "not a directory"},
		{"no go.mod", noGoMod, "no readable go.mod"},
		{"wrong module path", wrongModule, "want " + donmaiModulePath},
		{"no cmd/donmai", noCmd, "no cmd/donmai package"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := describeNonDonmaiCheckout(tc.dir)
			switch {
			case tc.wantReason == "" && got != "":
				t.Errorf("rejected a valid checkout: %s", got)
			case tc.wantReason != "" && !strings.Contains(got, tc.wantReason):
				t.Errorf("reason = %q, want it to contain %q", got, tc.wantReason)
			}
		})
	}
}

func TestModulePathOf(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "module github.com/RenseiAI/donmai\n\ngo 1.25.12\n", "github.com/RenseiAI/donmai"},
		{"leading blank lines", "\n\n  module   example.com/x  \n", "example.com/x"},
		{"trailing comment", "module example.com/x // pinned\n", "example.com/x"},
		{"quoted", `module "example.com/x"` + "\n", "example.com/x"},
		{"no module directive", "go 1.25.12\nrequire x v1.0.0\n", ""},
		{"module-prefixed word is not a directive", "modulepath example.com/x\n", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modulePathOf(tc.in); got != tc.want {
				t.Errorf("modulePathOf(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
