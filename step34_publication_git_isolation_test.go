package smokes

// This smoke builds a temporary consumer of the selected Donmai source and
// drives its exported worktree manager through real, private Git repositories.
// The root smokes module keeps its frozen Donmai dependency unchanged.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

func TestPublicationGitIsolationSourceConsumer(t *testing.T) {
	afh.SkipIfShort(t, "real-Git publication source-consumer smoke")
	afh.SkipIfToolMissing(t, "git", "publication inspection requires real local Git")
	source := afh.RequireDonmaiSource(t)
	if _, err := os.Stat(filepath.Join(source, "runtime", "worktree", "publication.go")); err != nil {
		afh.DeclineLive(t, "selected Donmai checkout predates the interactive publication inspector: %v", err)
	}

	moduleBytes, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil {
		t.Fatalf("read selected Donmai module: %v", err)
	}
	goVersion := ""
	for _, line := range strings.Split(string(moduleBytes), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			goVersion = fields[1]
			break
		}
	}
	if goVersion == "" {
		t.Fatal("selected Donmai module has no go directive")
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "publication-git-isolation", "consumer_test.go"))
	if err != nil {
		t.Fatalf("read publication source-consumer fixture: %v", err)
	}
	moduleDir := t.TempDir()
	mod := fmt.Sprintf("module donmai-publication-smoke-consumer\n\ngo %s\n\nrequire github.com/RenseiAI/donmai v0.0.0-00010101000000-000000000000\n\nreplace github.com/RenseiAI/donmai => %s\n", goVersion, source)
	for name, content := range map[string][]byte{
		"go.mod":           []byte(mod),
		"consumer_test.go": fixture,
	} {
		if err := os.WriteFile(filepath.Join(moduleDir, name), content, 0o600); err != nil {
			t.Fatalf("write private consumer %s: %v", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-race", "-count=1", "-v", "-mod=mod", ".") //nolint:gosec // fixed tool and private module
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("publication consumer against selected Donmai source: %v\n%s", err, output)
	}
	for _, name := range []string{
		"TestPublicationRejectsAmbientRewrite",
		"TestPublicationDoesNotExecuteCleanFilter",
		"TestPublicationRetainsStagedUncommittedChanges",
		"TestPublicationRejectsGitExecutionCanaries",
		"TestPublicationAdmittedHTTPS",
		"TestPublicationStandardSSHBoundary",
	} {
		if !strings.Contains(string(output), "--- PASS: "+name) {
			t.Fatalf("private consumer did not execute %s:\n%s", name, output)
		}
	}
	afh.RecordLive(t.Name(), afh.LiveExercised, "selected Donmai worktree manager inspected private Git remotes")
	t.Logf("publication source-consumer result against %s:\n%s", source, output)
}
