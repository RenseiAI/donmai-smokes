package publicationfixture

// This temporary consumer imports the production worktree manager. Every Git
// remote is a private bare repository or a loopback listener owned by the test.

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

const fixtureSessionID = "11111111-1111-4111-8111-111111111111"

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // literal local fixture arguments
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"), "PATH=/usr/bin:/bin",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("private git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func privateRepo(t *testing.T) (string, *worktree.Manager, string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "--bare", "-q", bare)
	git(t, root, "init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("published\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "tracked.txt")
	git(t, seed, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-m", "initial")
	git(t, seed, "remote", "add", "origin", bare)
	git(t, seed, "push", "-q", "origin", "main")
	git(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{
		SessionID: fixtureSessionID, RepoURL: bare, Strategy: worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := "agent/" + fixtureSessionID
	git(t, path, "checkout", "-q", "-b", branch)
	return bare, manager, path, branch
}

func assessment(t *testing.T, manager *worktree.Manager, repository, branch string) worktree.PublicationAssessment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	lease, got, err := manager.AcquireInteractiveTerminalLease(ctx,
		worktree.InteractivePublicationSpec{
			SessionID:    fixtureSessionID,
			Repositories: []worktree.PublicationTarget{{Name: "remote", Repository: repository, Branch: branch}},
		},
		workarea.AcquireSpec{
			SessionID: fixtureSessionID, TerminalResultID: "tr_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true,
		}, false)
	if err != nil {
		t.Fatalf("production publication manager: %v", err)
	}
	if got.Retain && (lease == nil || lease.ReleaseDisposition != "archive") {
		t.Fatalf("uncertain/unpublished assessment did not retain workarea: lease=%+v assessment=%+v", lease, got)
	}
	if !got.Retain && lease != nil {
		t.Fatalf("published checkout unexpectedly retained: lease=%+v assessment=%+v", lease, got)
	}
	return got
}

func canaryScript(t *testing.T, name string) (script, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "executed")
	script = filepath.Join(dir, name)
	body := "#!/bin/sh\nprintf touched > " + strconv.Quote(marker) + "\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, marker
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ambient executable ran or marker status uncertain: %s: %v", path, err)
	}
}

func TestPublicationRejectsAmbientRewrite(t *testing.T) {
	bare, manager, checkout, branch := privateRepo(t)
	other := filepath.Join(t.TempDir(), "other.git")
	git(t, "", "clone", "--bare", "-q", bare, other)
	git(t, checkout, "push", "-q", other, "HEAD:refs/heads/"+branch)
	global := filepath.Join(t.TempDir(), "global.gitconfig")
	if err := os.WriteFile(global, []byte(fmt.Sprintf("[url %q]\n\tinsteadOf = %s\n", other, bare)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	got := assessment(t, manager, bare, branch)
	if !got.Retain || got.Reason != worktree.PublicationReasonUnpublished {
		t.Fatalf("ambient rewrite forged publication: %+v", got)
	}
}

func TestPublicationDoesNotExecuteCleanFilter(t *testing.T) {
	bare, manager, checkout, branch := privateRepo(t)
	for name, body := range map[string]string{
		"filtered.txt":   "before\n",
		".gitattributes": "filtered.txt filter=fixture\n",
	} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, checkout, "add", ".")
	git(t, checkout, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-m", "tracked filter")
	git(t, checkout, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	marker := filepath.Join(t.TempDir(), "filter-executed")
	script := filepath.Join(t.TempDir(), "clean-filter")
	filter := "#!/bin/sh\nprintf hit > " + strconv.Quote(marker) + "\ncat\n"
	if err := os.WriteFile(script, []byte(filter), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, checkout, "config", "filter.fixture.clean", script)
	if err := os.WriteFile(filepath.Join(checkout, "filtered.txt"), []byte("after!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, checkout, "status", "--porcelain")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control did not arm the repository clean filter: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	got := assessment(t, manager, bare, branch)
	if !got.Retain || got.Reason != worktree.PublicationReasonDirty {
		t.Fatalf("dirty filtered bytes were deemed disposable: %+v", got)
	}
	absent(t, marker)
	if _, err := os.Stat(checkout); err != nil {
		t.Fatalf("dirty checkout was removed: %v", err)
	}
}

func TestPublicationRetainsStagedUncommittedChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(*testing.T, string)
	}{
		{"addition", func(t *testing.T, checkout string) {
			if err := os.WriteFile(filepath.Join(checkout, "staged.txt"), []byte("unpublished staged bytes\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git(t, checkout, "add", "staged.txt")
		}},
		{"modification", func(t *testing.T, checkout string) {
			if err := os.WriteFile(filepath.Join(checkout, "tracked.txt"), []byte("changed staged bytes\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git(t, checkout, "add", "tracked.txt")
		}},
		{"deletion", func(t *testing.T, checkout string) {
			git(t, checkout, "rm", "-q", "tracked.txt")
		}},
		{"mode", func(t *testing.T, checkout string) {
			path := filepath.Join(checkout, "tracked.txt")
			if err := os.Chmod(path, 0o700); err != nil {
				t.Fatal(err)
			}
			git(t, checkout, "add", "tracked.txt")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bare, manager, checkout, branch := privateRepo(t)
			git(t, checkout, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
			tc.stage(t, checkout)
			if staged := git(t, checkout, "diff", "--cached", "--name-only"); staged == "" {
				t.Fatal("control did not stage an uncommitted change")
			}
			got := assessment(t, manager, bare, branch)
			if !got.Retain || got.Reason != worktree.PublicationReasonDirty {
				t.Fatalf("staged unpublished change was deemed disposable: %+v", got)
			}
			if _, err := os.Stat(checkout); err != nil {
				t.Fatalf("staged checkout was removed: %v", err)
			}
		})
	}
}

func TestPublicationRejectsGitExecutionCanaries(t *testing.T) {
	cases := []struct {
		name string
		arm  func(*testing.T) (string, string)
	}{
		{"remote_helper", func(t *testing.T) (string, string) {
			script, marker := canaryScript(t, "git-remote-fixture")
			t.Setenv("PATH", filepath.Dir(script)+string(os.PathListSeparator)+os.Getenv("PATH"))
			return "fixture://example.invalid/repo", marker
		}},
		{"configured_ssh_command", func(t *testing.T) (string, string) {
			script, marker := canaryScript(t, "ssh-command")
			global := filepath.Join(t.TempDir(), "global.gitconfig")
			git(t, "", "config", "--file", global, "core.sshCommand", script)
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			return "ssh://git@127.0.0.1:1/repo.git", marker
		}},
		{"askpass", func(t *testing.T) (string, string) {
			script, marker := canaryScript(t, "askpass")
			t.Setenv("GIT_ASKPASS", script)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(server.Close)
			return server.URL + "/repo.git", marker
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, manager, checkout, branch := privateRepo(t)
			remote, marker := tc.arm(t)
			got := assessment(t, manager, remote, branch)
			if !got.Retain || got.Reason != worktree.PublicationReasonUncertain {
				t.Fatalf("ambient execution uncertainty not retained: %+v", got)
			}
			absent(t, marker)
			if _, err := os.Stat(checkout); err != nil {
				t.Fatalf("uncertain checkout was removed: %v", err)
			}
		})
	}
}

// gitHTTPBackend delegates the protocol to Git itself, keeping the fixture
// transport local while avoiding any canned ref or parser implementation.
func gitHTTPBackend(t *testing.T, projectRoot string, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		t.Errorf("read local Git HTTP request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	cmd := exec.CommandContext(r.Context(), "git", "http-backend") //nolint:gosec // fixed Git CGI over private bare repo
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin", "HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_PROJECT_ROOT=" + projectRoot, "GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=" + r.URL.Path, "QUERY_STRING=" + r.URL.RawQuery,
		"REQUEST_METHOD=" + r.Method, "CONTENT_TYPE=" + r.Header.Get("Content-Type"),
		"CONTENT_LENGTH=" + strconv.Itoa(len(body)),
	}
	out, err := cmd.Output()
	if err != nil {
		t.Errorf("local git http-backend: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	head, payload, ok := bytes.Cut(out, []byte("\r\n\r\n"))
	if !ok {
		head, payload, ok = bytes.Cut(out, []byte("\n\n"))
	}
	if !ok {
		t.Errorf("git http-backend omitted CGI header separator")
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	status := http.StatusOK
	for _, line := range strings.Split(strings.ReplaceAll(string(head), "\r", ""), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.EqualFold(key, "Status") {
			if fields := strings.Fields(value); len(fields) > 0 {
				status, _ = strconv.Atoi(fields[0])
			}
		} else {
			w.Header().Set(key, value)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func TestPublicationAdmittedHTTPS(t *testing.T) {
	bare, manager, checkout, branch := privateRepo(t)
	git(t, checkout, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	const syntheticHeader = "Authorization: Basic Zml4dHVyZTpwYXNz"
	var authorized atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != strings.TrimPrefix(syntheticHeader, "Authorization: ") {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authorized.Add(1)
		gitHTTPBackend(t, filepath.Dir(bare), w, r)
	}))
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "fixture-ca.pem")
	cert := server.TLS.Certificates[0].Certificate[0]
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0o600); err != nil {
		t.Fatal(err)
	}
	url := server.URL + "/repo.git"
	t.Setenv("GIT_SSL_CAINFO", ca)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http."+url+".extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", syntheticHeader)
	askpass, marker := canaryScript(t, "unselected-askpass")
	t.Setenv("GIT_ASKPASS", askpass)
	t.Setenv("GIT_SSL_NO_VERIFY", "")
	got := assessment(t, manager, url, branch)
	if got.Retain || got.Reason != worktree.PublicationReasonPublished {
		t.Fatalf("exact admitted HTTPS authentication not published: %+v", got)
	}
	if authorized.Load() == 0 {
		t.Fatal("Git HTTP backend saw no exact admitted Authorization header")
	}
	absent(t, marker)
}

func TestPublicationStandardSSHBoundary(t *testing.T) {
	// The transport must be admitted and attempted by the fixed SSH binary,
	// while hostile core.sshCommand/GIT_SSH_COMMAND values stay inert. A full
	// private sshd auth run is required before claiming SSH authentication.
	_, manager, checkout, branch := privateRepo(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()
	global := filepath.Join(t.TempDir(), "global.gitconfig")
	command, marker := canaryScript(t, "hostile-ssh-command")
	git(t, "", "config", "--file", global, "core.sshCommand", command)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_SSH_COMMAND", command)
	remote := fmt.Sprintf("ssh://git@127.0.0.1:%d/repo.git", listener.Addr().(*net.TCPAddr).Port)
	got := assessment(t, manager, remote, branch)
	if !got.Retain || got.Reason != worktree.PublicationReasonUncertain {
		t.Fatalf("uncompleted SSH proof should retain checkout: %+v", got)
	}
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("fixed SSH transport did not reach owned loopback listener")
	}
	absent(t, marker)
	if _, err := os.Stat(checkout); err != nil {
		t.Fatalf("uncertain SSH checkout was removed: %v", err)
	}
	t.Log("SSH-GAP: bounded fixed SSH connection observed; authenticated private sshd round trip not exercised")
}
