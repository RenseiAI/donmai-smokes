package smokes

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// step58SessionScratchCapability is the identity probe for the per-session
// scratch directory surface: the exported spawner option that derives a
// directory for an undeclared session. A checkout without it predates the
// surface and declines; it never fails on age.
const step58SessionScratchCapability = "SessionTmpDerivedPrefix"

// TestSessionScratchPublicConsumer builds a separate module against the
// selected source and drives only the public daemon spawner API with real
// worker processes. It asserts that a seat's TMPDIR is its own session
// directory (derived for an undeclared session, the declared value when the
// embedder's pre-spawn hook supplies one), that the directory is gone after
// the session ends or is killed, and that a restarted daemon's startup sweep
// reclaims the directory of a session whose daemon died mid-session.
func TestSessionScratchPublicConsumer(t *testing.T) {
	afh.SkipIfShort(t, "compiled public session-scratch consumer")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	afh.SkipIfToolMissing(t, "go", "external session-scratch consumer requires the Go toolchain")
	spawner, err := os.ReadFile(filepath.Join(source, "daemon", "worker_spawner.go"))
	if err != nil {
		t.Fatalf("read the selected source's spawner: %v", err)
	}
	if !strings.Contains(string(spawner), step58SessionScratchCapability) {
		afh.DeclineLive(t, "donmai checkout at %q predates the per-session scratch default (%s)", source, step58SessionScratchCapability)
	}
	moduleBytes, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	version := ""
	for _, line := range strings.Split(string(moduleBytes), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" {
			version = fields[1]
			break
		}
	}
	if version == "" {
		t.Fatal("selected source module has no Go directive")
	}
	moduleDir := t.TempDir()
	mod := fmt.Sprintf("module session-scratch-consumer\n\ngo %s\n\nrequire github.com/RenseiAI/donmai v0.0.0\nreplace github.com/RenseiAI/donmai => %s\n", version, strconv.Quote(source))
	for name, body := range map[string]string{"go.mod": mod, "consumer_test.go": step58SessionScratchConsumer} {
		if err := os.WriteFile(filepath.Join(moduleDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-race", "-count=1", "-v", "-mod=mod", ".")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("public session-scratch consumer from %s failed: %v\n%s", source, err, output)
	}
	for _, name := range []string{
		"TestPublicScratchDerivedAndRemoved/completes",
		"TestPublicScratchDerivedAndRemoved/killed",
		"TestPublicScratchHookDeclaredBinds",
		"TestPublicScratchSweptAfterDaemonDies",
	} {
		if !strings.Contains(string(output), "--- PASS: "+name+" ") {
			t.Fatalf("public consumer did not execute %s:\n%s", name, output)
		}
	}
	afh.RecordLive(t.Name(), afh.LiveExercised, "compiled selected-source external spawner consumer: seat TMPDIR is its session directory, removed on end and kill, swept after daemon death")
	t.Logf("public session-scratch consumer source=%s:\n%s", source, output)
}

const step58SessionScratchConsumer = `package consumer_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon"
)

const (
	repo            = "github.com/smoke/scratch"
	crashRecordEnv  = "SMOKE_SESSION_SCRATCH_CRASH_RECORD"
	crashCaptureEnv = "SMOKE_SESSION_SCRATCH_CRASH_CAPTURE"
	crashSessionID  = "smoke-scratch-died-with-daemon"
)

var derived = regexp.MustCompile("^/tmp/" + regexp.QuoteMeta(daemon.DefaultSessionTmpDerivedPrefix) + "[0-9a-f]{12}$")

// TestMain routes this binary into crash-helper mode: a daemon that launches
// one session and dies mid-session, before any terminal cleanup can run.
func TestMain(m *testing.M) {
	if record := os.Getenv(crashRecordEnv); record != "" {
		os.Exit(crashMidSession(record, os.Getenv(crashCaptureEnv)))
	}
	os.Exit(m.Run())
}

// safeRemove deletes only a path of a shape this consumer creates. A path a
// worker reports is whatever TMPDIR it saw; if the system under test is
// broken that is the host's shared temp, which a smoke must never delete.
func safeRemove(dir string) {
	if derived.MatchString(dir) || strings.HasPrefix(dir, "/tmp/smoke-scratch-") {
		_ = os.RemoveAll(dir)
	}
}

// seatWorker records what the seat sees, then holds until the test writes
// <root>/<session>/release (its content is the exit code) or 20 s pass.
func seatWorker(root string) []string {
	script := "root='" + root + "'/\"$DONMAI_SESSION_ID\"; mkdir -p \"$root\"; " +
		"{ printf 'TMPDIR=%s\\n' \"$TMPDIR\"; printf 'ADVISORY=%s\\n' \"$DONMAI_SESSION_TMPDIR\"; printf 'MODE=%s\\n' \"$(ls -ld \"$TMPDIR\" | cut -c1-10)\"; } > \"$root/seen.partial\" && mv \"$root/seen.partial\" \"$root/seen\"; " +
		"i=0; while [ ! -s \"$root/release\" ] && [ \"$i\" -lt 400 ]; do sleep 0.05; i=$((i+1)); done; " +
		"exit \"$(cat \"$root/release\" 2>/dev/null || echo 0)\""
	return []string{"/bin/sh", "-c", script}
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func seen(t *testing.T, root, sessionID string) map[string]string {
	t.Helper()
	path := filepath.Join(root, sessionID, "seen")
	waitFile(t, path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			out[key] = value
		}
	}
	return out
}

func ends(s *daemon.WorkerSpawner) <-chan struct{} {
	ch := make(chan struct{}, 8)
	s.On(func(ev daemon.SessionEvent) {
		if ev.Kind == daemon.SessionEventEnded {
			ch <- struct{}{}
		}
	})
	return ch
}

func waitEndAndIdle(t *testing.T, s *daemon.WorkerSpawner, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the session to end")
	}
	deadline := time.Now().Add(30 * time.Second)
	for s.ActiveCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("ActiveCount = %d, want 0", s.ActiveCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertGone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("session directory %s survives the session: %v", dir, err)
	}
}

func spec(id string) daemon.SessionSpec {
	return daemon.SessionSpec{SessionID: id, Repository: repo, Ref: "main"}
}

func newSpawner(t *testing.T, root string, hook func(daemon.SessionSpec, []string) ([]string, error)) *daemon.WorkerSpawner {
	return daemon.NewWorkerSpawner(daemon.SpawnerOptions{
		Projects:                []daemon.ProjectConfig{{ID: "smoke", Repository: repo}},
		WorkerCommand:           seatWorker(root),
		SessionTmpRecordDir:     t.TempDir(),
		SessionTmpDerivedPrefix: daemon.DefaultSessionTmpDerivedPrefix,
		OnPreSpawn:              hook,
	})
}

// An undeclared session runs in its own derived 0700 directory, named to it
// as DONMAI_SESSION_TMPDIR, and the directory is gone once the session
// completes or is killed.
func TestPublicScratchDerivedAndRemoved(t *testing.T) {
	for _, end := range []string{"completes", "killed"} {
		t.Run(end, func(t *testing.T) {
			root := t.TempDir()
			s := newSpawner(t, root, nil)
			ended := ends(s)
			id := "smoke-scratch-" + end
			if _, err := s.AcceptWork(spec(id)); err != nil {
				t.Fatalf("accept: %v", err)
			}
			got := seen(t, root, id)
			dir := got["TMPDIR"]
			t.Cleanup(func() { safeRemove(dir) })
			if !derived.MatchString(dir) {
				t.Fatalf("seat TMPDIR = %q, want its own %s directory", dir, derived)
			}
			if got["ADVISORY"] != dir {
				t.Fatalf("seat DONMAI_SESSION_TMPDIR = %q, want %q", got["ADVISORY"], dir)
			}
			if got["MODE"] != "drwx------" {
				t.Fatalf("seat TMPDIR mode = %q, want drwx------", got["MODE"])
			}
			if end == "killed" {
				if !s.StopSession(id) {
					t.Fatal("StopSession refused a live session")
				}
			} else if err := os.WriteFile(filepath.Join(root, id, "release"), []byte("0"), 0o600); err != nil {
				t.Fatal(err)
			}
			waitEndAndIdle(t, s, ended)
			assertGone(t, dir)
		})
	}
}

// A directory the embedder's pre-spawn hook declares is the one the seat
// runs in, and it is removed with the session.
func TestPublicScratchHookDeclaredBinds(t *testing.T) {
	dir := fmt.Sprintf("/tmp/smoke-scratch-hook-%d", os.Getpid())
	safeRemove(dir)
	t.Cleanup(func() { safeRemove(dir) })
	root := t.TempDir()
	s := newSpawner(t, root, func(_ daemon.SessionSpec, env []string) ([]string, error) {
		return append(append([]string(nil), env...), daemon.SessionTmpDirEnv+"="+dir), nil
	})
	ended := ends(s)
	const id = "smoke-scratch-hook"
	if _, err := s.AcceptWork(spec(id)); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got := seen(t, root, id); got["TMPDIR"] != dir || got["MODE"] != "drwx------" {
		t.Fatalf("seat saw %v, want TMPDIR=%s with mode drwx------", got, dir)
	}
	if err := os.WriteFile(filepath.Join(root, id, "release"), []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitEndAndIdle(t, s, ended)
	assertGone(t, dir)
}

func crashMidSession(record, capture string) int {
	s := daemon.NewWorkerSpawner(daemon.SpawnerOptions{
		Projects:                []daemon.ProjectConfig{{ID: "smoke", Repository: repo}},
		WorkerCommand:           []string{"/bin/sh", "-c", "printf '%s' \"$TMPDIR\" > \"$CRASH_CAPTURE.partial\" && mv \"$CRASH_CAPTURE.partial\" \"$CRASH_CAPTURE\"; sleep 2"},
		BaseEnv:                 map[string]string{"CRASH_CAPTURE": capture},
		SessionTmpRecordDir:     record,
		SessionTmpDerivedPrefix: daemon.DefaultSessionTmpDerivedPrefix,
	})
	if _, err := s.AcceptWork(spec(crashSessionID)); err != nil {
		fmt.Fprintln(os.Stderr, "crash helper: accept:", err)
		return 2
	}
	for i := 0; i < 800; i++ {
		if _, err := os.Stat(capture); err == nil {
			return 0 // die mid-session: os.Exit runs no terminal cleanup
		}
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "crash helper: the seat never reported its TMPDIR")
	return 3
}

// A daemon that dies mid-session leaves its session's directory behind; the
// restarted daemon's startup sweep reclaims it.
func TestPublicScratchSweptAfterDaemonDies(t *testing.T) {
	record := t.TempDir()
	capture := filepath.Join(t.TempDir(), "crashed-tmpdir")
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), crashRecordEnv+"="+record, crashCaptureEnv+"="+capture)
	if out, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	dir := string(raw)
	t.Cleanup(func() { safeRemove(dir) })
	if !derived.MatchString(dir) {
		t.Fatalf("crashed seat TMPDIR = %q, want a derived session directory", dir)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("the dead daemon's session directory should still exist before the sweep: %v", err)
	}
	restarted := daemon.NewWorkerSpawner(daemon.SpawnerOptions{SessionTmpRecordDir: record})
	report := restarted.SweepSessionTmpDirs(nil)
	if report.Removed != 1 {
		t.Fatalf("startup sweep report = %+v, want one reclaimed directory", report)
	}
	assertGone(t, dir)
}
`
