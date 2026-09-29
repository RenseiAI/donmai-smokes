package harness

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// authRecorder is an httptest daemon stand-in that records the
// Authorization and Content-Type headers each request arrived with, keyed
// by method, and answers /healthz so SpawnDaemon can wait on it.
type authRecorder struct {
	mu          sync.Mutex
	auth        map[string]string
	contentType map[string]string
}

func newAuthRecorder(t *testing.T) (*authRecorder, *httptest.Server) {
	t.Helper()
	rec := &authRecorder{auth: map[string]string{}, contentType: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			rec.mu.Lock()
			rec.auth[r.Method] = r.Header.Get("Authorization")
			rec.contentType[r.Method] = r.Header.Get("Content-Type")
			rec.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *authRecorder) seen(method string) (auth, contentType string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	auth, ok = r.auth[method]
	return auth, r.contentType[method], ok
}

// send builds method through d.NewRequest (with a JSON body for every
// method but GET), sends it, and returns the Authorization header the
// server saw.
func send(t *testing.T, d *LiveDaemon, rec *authRecorder, method string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var body io.Reader
	wantContentType := ""
	if method != http.MethodGet {
		body = strings.NewReader(`{}`)
		wantContentType = "application/json"
	}
	req, err := d.NewRequest(ctx, method, "/api/daemon/probe", body)
	if err != nil {
		t.Fatalf("NewRequest(%s): %v", method, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s /api/daemon/probe: %v", method, err)
	}
	_ = resp.Body.Close()
	auth, contentType, ok := rec.seen(method)
	if !ok {
		t.Fatalf("server never saw %s", method)
	}
	if contentType != wantContentType {
		t.Errorf("%s Content-Type = %q, want %q", method, contentType, wantContentType)
	}
	return auth
}

func writeTokenFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir token dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
}

// TestLiveDaemonNewRequest_ControlToken pins the control-token contract of
// the daemon HTTP helper: with a token file, every mutating method carries
// `Authorization: Bearer <token>`; GET never does; with no token file (a
// daemon that predates the control token) no request carries a credential.
func TestLiveDaemonNewRequest_ControlToken(t *testing.T) {
	rec, srv := newAuthRecorder(t)
	present := filepath.Join(t.TempDir(), ".donmai", "control-token")
	writeTokenFile(t, present, "  smoke-control-token\n")

	cases := []struct {
		name      string
		tokenFile string
		want      map[string]string
	}{
		{
			name:      "token file present",
			tokenFile: present,
			want: map[string]string{
				http.MethodGet:    "",
				http.MethodPost:   "Bearer smoke-control-token",
				http.MethodPut:    "Bearer smoke-control-token",
				http.MethodPatch:  "Bearer smoke-control-token",
				http.MethodDelete: "Bearer smoke-control-token",
			},
		},
		{
			name:      "token file absent (daemon predates the token)",
			tokenFile: filepath.Join(t.TempDir(), ".donmai", "control-token"),
			want: map[string]string{
				http.MethodGet: "", http.MethodPost: "", http.MethodPut: "", http.MethodPatch: "", http.MethodDelete: "",
			},
		},
		{
			name:      "no token path resolved",
			tokenFile: "",
			want: map[string]string{
				http.MethodGet: "", http.MethodPost: "", http.MethodPut: "", http.MethodPatch: "", http.MethodDelete: "",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &LiveDaemon{URL: srv.URL + "/", controlTokenFile: tc.tokenFile}
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
				if got := send(t, d, rec, method); got != tc.want[method] {
					t.Errorf("%s Authorization = %q, want %q", method, got, tc.want[method])
				}
			}
		})
	}
}

// TestLiveDaemonNewRequest_UnreadableTokenFile pins that a token path that
// exists but cannot be read fails the request build instead of silently
// sending no credential, and that the error never echoes a token.
func TestLiveDaemonNewRequest_UnreadableTokenFile(t *testing.T) {
	dirAsFile := t.TempDir() // a directory: exists, but ReadFile fails
	d := &LiveDaemon{URL: "http://127.0.0.1:1", controlTokenFile: dirAsFile}
	if _, err := d.NewRequest(context.Background(), http.MethodPost, "/api/daemon/sessions", strings.NewReader(`{}`)); err == nil {
		t.Fatal("NewRequest(POST) with an unreadable token path: want error, got nil")
	}
	// GET never reads the token, so an unreadable path cannot break it.
	if _, err := d.NewRequest(context.Background(), http.MethodGet, "/api/daemon/status", nil); err != nil {
		t.Fatalf("NewRequest(GET) with an unreadable token path: %v", err)
	}
}

func TestControlTokenFileFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "absolute file override wins",
			env:  []string{"HOME=/h", "DONMAI_STATE_HOME=/s", "DONMAI_CONTROL_TOKEN_FILE=/o/token"},
			want: "/o/token",
		},
		{
			name: "relative file override resolves nothing",
			env:  []string{"HOME=/h", "DONMAI_CONTROL_TOKEN_FILE=rel/token"},
			want: "",
		},
		{
			name: "empty file override falls through",
			env:  []string{"HOME=/h", "DONMAI_CONTROL_TOKEN_FILE="},
			want: filepath.Join("/h", ".donmai", "control-token"),
		},
		{
			name: "state home beats HOME",
			env:  []string{"HOME=/h", "DONMAI_STATE_HOME=/s"},
			want: filepath.Join("/s", ".donmai", "control-token"),
		},
		{
			name: "HOME without a state home",
			env:  []string{"PATH=/usr/bin", "HOME=/h"},
			want: filepath.Join("/h", ".donmai", "control-token"),
		},
		{
			name: "last assignment wins",
			env:  []string{"HOME=/first", "HOME=/second"},
			want: filepath.Join("/second", ".donmai", "control-token"),
		},
		{
			name: "nothing resolves",
			env:  []string{"PATH=/usr/bin", "HOMEDIR=/not-home"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ControlTokenFileFromEnv(tc.env); got != tc.want {
				t.Errorf("ControlTokenFileFromEnv(%q) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

func TestLoadControlToken(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "control-token")
	writeTokenFile(t, present, "\n tok-123 \n")
	cases := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "present and trimmed", path: present, want: "tok-123"},
		{name: "missing file", path: filepath.Join(dir, "absent")},
		{name: "empty path", path: ""},
		{name: "unreadable path", path: dir, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadControlToken(tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadControlToken(%q) err = %v, wantErr %v", tc.path, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("LoadControlToken(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestSpawnDaemon_ResolvesControlTokenFromDaemonEnv proves the wiring end to
// end: SpawnDaemon derives the token path from the env the daemon was
// started with, and requests built through the returned LiveDaemon carry
// that daemon's token on mutating methods only. A sleep process stands in
// for the daemon; an httptest server answers /healthz and records headers.
func TestSpawnDaemon_ResolvesControlTokenFromDaemonEnv(t *testing.T) {
	stateHome := t.TempDir()
	stateToken := filepath.Join(stateHome, ".donmai", "control-token")
	writeTokenFile(t, stateToken, "state-home-token\n")
	override := filepath.Join(t.TempDir(), "override-token")
	writeTokenFile(t, override, "override-token\n")
	unmintedHome := t.TempDir()

	cases := []struct {
		name     string
		env      []string
		wantFile string
		wantAuth string
	}{
		{
			name:     "state home token",
			env:      []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "DONMAI_STATE_HOME=" + stateHome},
			wantFile: stateToken,
			wantAuth: "Bearer state-home-token",
		},
		{
			name:     "explicit token file",
			env:      []string{"PATH=/usr/bin:/bin", "HOME=" + stateHome, ControlTokenFileEnv + "=" + override},
			wantFile: override,
			wantAuth: "Bearer override-token",
		},
		{
			name:     "no token minted",
			env:      []string{"PATH=/usr/bin:/bin", "HOME=" + unmintedHome},
			wantFile: filepath.Join(unmintedHome, ".donmai", "control-token"),
			wantAuth: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, srv := newAuthRecorder(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			live, err := SpawnDaemon(ctx, SpawnOptions{
				Binary:         "/bin/sleep",
				Args:           []string{"30"},
				Env:            tc.env,
				HealthzBaseURL: srv.URL,
			})
			if err != nil {
				t.Fatalf("SpawnDaemon: %v", err)
			}
			t.Cleanup(live.Stop)
			if live.ControlTokenFile() != tc.wantFile {
				t.Errorf("ControlTokenFile() = %q, want %q", live.ControlTokenFile(), tc.wantFile)
			}
			if got := send(t, live, rec, http.MethodPost); got != tc.wantAuth {
				t.Errorf("POST Authorization = %q, want %q", got, tc.wantAuth)
			}
			if got := send(t, live, rec, http.MethodGet); got != "" {
				t.Errorf("GET Authorization = %q, want none", got)
			}
		})
	}
}

// TestRunHermeticAgainstDaemon_ControlTokenFile pins that the CLI runner
// hands a subprocess the daemon's token PATH (never the token) only when
// one is supplied. /usr/bin/env prints the environment it received.
func TestRunHermeticAgainstDaemon_ControlTokenFile(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), ".donmai", "control-token")
	cases := []struct {
		name      string
		tokenFile string
		want      string
	}{
		{name: "path supplied", tokenFile: tokenFile, want: ControlTokenFileEnv + "=" + tokenFile},
		{name: "no path", tokenFile: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := RunHermeticAgainstDaemon(ctx, HermeticRunOptions{
				Binary:           "/usr/bin/env",
				HomeDir:          t.TempDir(),
				DaemonURLEnvVar:  "DONMAI_DAEMON_URL",
				DaemonURL:        "http://127.0.0.1:1",
				ControlTokenFile: tc.tokenFile,
			})
			if err != nil {
				t.Fatalf("run /usr/bin/env: %v\n%s", err, out)
			}
			got := ""
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, ControlTokenFileEnv+"=") {
					got = line
				}
			}
			if got != tc.want {
				t.Errorf("subprocess %s line = %q, want %q\n--- env ---\n%s", ControlTokenFileEnv, got, tc.want, out)
			}
			if strings.Contains(out, "DONMAI_CONTROL_TOKEN=") {
				t.Errorf("subprocess received the token itself; only the path may travel\n--- env ---\n%s", out)
			}
		})
	}
}
