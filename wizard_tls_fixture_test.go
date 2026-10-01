package smokes

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
)

// This source exists only in a private Git archive copy. It changes no
// production command handler and installs the fixture CA only when explicitly
// selected by the test child. TLS verification remains enabled.
const wizardFixtureRootsSource = `package main

import (
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func init() {
	name := os.Getenv("DONMAI_TEST_WIZARD_CA_FILE")
	if name == "" {
		return
	}
	if !installWizardFixtureRoots(name) {
		fmt.Fprintln(os.Stderr, "wizard fixture CA unavailable")
		os.Exit(1)
	}
}

func installWizardFixtureRoots(name string) (ok bool) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return false
	}
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return false
	}
	defer root.Close()
	leaf := filepath.Base(name)
	before, err := root.Lstat(leaf)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() <= 0 || before.Size() > 64<<10 {
		return false
	}
	file, err := root.Open(leaf)
	if err != nil {
		return false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 || int64(len(raw)) != before.Size() {
		return false
	}
	after, err := root.Lstat(leaf)
	if err != nil || !os.SameFile(opened, after) || after.Size() != before.Size() || after.Mode().Perm() != 0o600 {
		return false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return false
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	x509.SetFallbackRoots(pool)
	return true
}
`

func wizardFixtureGODEBUG(current string) string {
	var parts []string
	for _, part := range strings.Split(current, ",") {
		key, _, _ := strings.Cut(strings.TrimSpace(part), "=")
		if part != "" && key != "x509usefallbackroots" {
			parts = append(parts, part)
		}
	}
	return strings.Join(append(parts, "x509usefallbackroots=1"), ",")
}

func wizardFixtureGit(t *testing.T, ctx context.Context, source string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", source}, args...)...)
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("verify selected tracked Donmai source: %v", err)
	}
	return out
}

func wizardFixtureTrackedFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("tracked source contains a non-regular entry")
		}
		rel, err := filepath.Rel(root, name)
		if err != nil || !filepath.IsLocal(rel) {
			return errors.New("tracked source path is not local")
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, io.LimitReader(file, (32<<20)+1))
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = hex.EncodeToString(digest.Sum(nil))
		return nil
	})
	if err != nil {
		t.Fatalf("verify archived production files: %v", err)
	}
	return files
}

func buildDarwinWizardFixture(t *testing.T, source, home string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	head := strings.TrimSpace(string(wizardFixtureGit(t, ctx, source, "rev-parse", "--verify", "HEAD^{commit}")))
	tree := strings.TrimSpace(string(wizardFixtureGit(t, ctx, source, "rev-parse", head+"^{tree}")))
	if len(head) != 40 || len(tree) != 40 {
		t.Fatal("selected Donmai source lacks a SHA-1 Git identity")
	}
	check := exec.CommandContext(ctx, "git", "-C", source, "diff", "--quiet", head, "--")
	if err := check.Run(); err != nil {
		t.Fatalf("selected Donmai source has changed tracked files: %v", err)
	}
	private := filepath.Join(home, "wizard-source")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(home, "wizard-source.tar")
	command := exec.CommandContext(ctx, "git", "-C", source, "archive", "--format=tar", "--output="+archive, head)
	if err := command.Run(); err != nil {
		t.Fatalf("archive exact tracked Donmai source: %v", err)
	}
	archiveFile, err := os.Open(archive)
	if err != nil {
		t.Fatal("tracked Donmai archive is unavailable")
	}
	raw, readErr := io.ReadAll(io.LimitReader(archiveFile, (64<<20)+1))
	if err := errors.Join(readErr, archiveFile.Close()); err != nil || len(raw) > 64<<20 {
		t.Fatal("tracked Donmai archive unavailable or oversized")
	}
	reader := tar.NewReader(bytes.NewReader(raw))
	globalHeaderSeen := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal("tracked Donmai archive is malformed")
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			if globalHeaderSeen || header.Name != "pax_global_header" || len(header.PAXRecords) != 1 || header.PAXRecords["comment"] != head {
				t.Fatal("tracked Donmai archive has unexpected global metadata")
			}
			globalHeaderSeen = true
			continue // Git's exact commit comment is metadata, not a source file.
		}
		name := filepath.Clean(header.Name)
		if !filepath.IsLocal(name) || name == "." {
			t.Fatal("tracked Donmai archive contains a nonlocal path")
		}
		path := filepath.Join(private, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > 32<<20 {
				t.Fatal("tracked Donmai archive contains an oversized file")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			mode := os.FileMode(0o600)
			if header.Mode&0o111 != 0 {
				mode = 0o700
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				t.Fatal(err)
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			if err := errors.Join(copyErr, file.Close()); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("tracked Donmai archive contains an unsupported entry")
		}
	}
	if !globalHeaderSeen {
		t.Fatal("tracked Donmai archive lacks its exact commit comment")
	}
	tracked := wizardFixtureTrackedFiles(t, private)
	listed := bytes.Split(wizardFixtureGit(t, ctx, source, "ls-tree", "-r", "-z", "--name-only", head), []byte{0})
	if len(listed)-1 != len(tracked) {
		t.Fatal("tracked Donmai archive file count differs from Git tree")
	}
	for _, name := range listed[:len(listed)-1] {
		if _, ok := tracked[string(name)]; !ok {
			t.Fatal("tracked Donmai archive path differs from Git tree")
		}
	}
	initFile := filepath.Join(private, "cmd", "donmai", "wizard_fixture_roots.go")
	file, err := os.OpenFile(initFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := io.WriteString(file, wizardFixtureRootsSource)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(home, "bin", "donmai-wizard-fixture")
	build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-buildvcs=false", "-o", binary, "./cmd/donmai")
	build.Dir = private
	build.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + home, "GOWORK=off", "GOTOOLCHAIN=local"}
	cache := struct{ GOCACHE, GOMODCACHE string }{}
	if os.Getenv("GOCACHE") == "" || os.Getenv("GOMODCACHE") == "" {
		metadata := exec.CommandContext(ctx, "go", "env", "-json", "GOCACHE", "GOMODCACHE")
		metadata.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local")
		output, err := metadata.Output()
		if err != nil || json.Unmarshal(output, &cache) != nil {
			t.Fatal("effective Go cache metadata is unavailable")
		}
	}
	for _, name := range []string{"GOCACHE", "GOMODCACHE"} {
		value := os.Getenv(name)
		if value == "" && name == "GOCACHE" {
			value = cache.GOCACHE
		} else if value == "" {
			value = cache.GOMODCACHE
		}
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			t.Fatal("effective Go cache path is not absolute and clean")
		}
		build.Env = append(build.Env, name+"="+value)
	}
	for _, name := range []string{"GOPROXY", "GOSUMDB"} {
		if value := os.Getenv(name); value != "" {
			build.Env = append(build.Env, name+"="+value)
		}
	}
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tracked-source Darwin wizard fixture: %v: %s", err, output)
	}
	after := wizardFixtureTrackedFiles(t, private)
	delete(after, "cmd/donmai/wizard_fixture_roots.go")
	if len(after) != len(tracked) {
		t.Fatal("Darwin fixture build changed tracked source file count")
	}
	for name, digest := range tracked {
		if after[name] != digest {
			t.Fatal("Darwin fixture build changed tracked production source")
		}
	}
	sum := sha256.Sum256(raw)
	ledger := map[string]any{"head": head, "tree": tree, "trackedFiles": len(tracked), "archiveSHA256": hex.EncodeToString(sum[:]), "fixtureInitSHA256": fmt.Sprintf("%x", sha256.Sum256([]byte(wizardFixtureRootsSource))), "goVersion": runtime.Version()}
	ledgerRaw, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "wizard-source-ledger.json"), ledgerRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Darwin fixture bootstrap: tracked source head=%s tree=%s files=%d archive_sha256=%x (not stock binary)", head, tree, len(tracked), sum)
	return binary
}

type wizardTLSTrace struct {
	connects, handshakes atomic.Int64
	reached              atomic.Bool
}

// A private CONNECT proxy keeps the production api.github.com origin and SNI.
// The leaf name is varied only for the negative hostname control.
func wizardTLSProxy(t *testing.T, leafName string) (string, string, *wizardTLSTrace) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "wizard fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{leafName}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	trace := &wizardTLSTrace{}
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace.reached.Store(true)
		if r.Method != http.MethodGet || r.URL.Path != "/repos/example/project" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42,"full_name":"example/project","default_branch":"release/next"}`)
	}))
	api.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			trace.handshakes.Add(1)
			return nil, nil
		},
	}
	api.StartTLS()
	t.Cleanup(api.Close)
	caFile := filepath.Join(t.TempDir(), "wizard-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "api.github.com:443" {
			http.Error(w, "fixture refused foreign CONNECT", http.StatusForbidden)
			return
		}
		trace.connects.Add(1)
		target, err := net.DialTimeout("tcp", api.Listener.Addr().String(), time.Second)
		if err != nil {
			http.Error(w, "fixture TLS target unavailable", http.StatusServiceUnavailable)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = target.Close()
			return
		}
		if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
			err = buffer.Flush()
		}
		if err != nil {
			_ = client.Close()
			_ = target.Close()
			return
		}
		go func() { _, _ = io.Copy(target, client); _ = target.Close() }()
		_, _ = io.Copy(client, target)
		_ = client.Close()
		_ = target.Close()
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, caFile, trace
}

func checkDarwinWizardTLSRefusals(t *testing.T, binary, fixtureHome string) {
	t.Helper()
	wrongCAProxy, _, wrongCATrace := wizardTLSProxy(t, "api.github.com")
	_, unrelatedCA, _ := wizardTLSProxy(t, "api.github.com")
	wrongHostProxy, wrongHostCA, wrongHostTrace := wizardTLSProxy(t, "other.example")
	for _, tc := range []struct {
		name, proxy, ca string
		trace           *wizardTLSTrace
	}{
		{"unrelated CA", wrongCAProxy, unrelatedCA, wrongCATrace},
		{"wrong hostname", wrongHostProxy, wrongHostCA, wrongHostTrace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wizardExpectTLSRefusal(t, binary, fixtureHome, tc.proxy, tc.ca, tc.trace)
		})
	}
}

func wizardExpectTLSRefusal(t *testing.T, binary, fixtureHome, proxy, ca string, trace *wizardTLSTrace) {
	t.Helper()
	home := t.TempDir()
	certDir := filepath.Join(home, "empty-cert-dir")
	if err := os.Mkdir(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, ".donmai", "daemon.yaml")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "host", "setup", "--config", config)
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home, "DONMAI_STATE_HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"), "TMPDIR=" + home,
		"PATH=" + filepath.Join(fixtureHome, "bin") + ":/usr/bin:/bin",
		"TERM=dumb", "NO_COLOR=1", "LANG=C",
		"GITHUB_TOKEN=" + profileFixtureToken,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"HTTPS_PROXY=" + proxy, "HTTP_PROXY=" + proxy,
		"NO_PROXY=127.0.0.1,localhost",
		"SSL_CERT_FILE=" + ca, "SSL_CERT_DIR=" + certDir,
		"DONMAI_TEST_WIZARD_CA_FILE=" + ca,
		"GODEBUG=" + wizardFixtureGODEBUG(os.Getenv("GODEBUG")),
	}
	terminal, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("start owned TLS refusal wizard: %v", err)
	}
	output := &wizardPTYOutput{notify: make(chan struct{}, 1)}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 4096)
		for {
			n, err := terminal.Read(buf)
			if n > 0 {
				output.append(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	joined := false
	t.Cleanup(func() {
		if !joined {
			_ = cmd.Process.Kill() // exact owned fixture PID only
			select {
			case <-waitDone:
			case <-time.After(5 * time.Second):
				t.Error("owned TLS refusal process did not join after exact-PID kill")
			}
		}
		_ = terminal.Close()
		select {
		case <-readerDone:
		case <-time.After(2 * time.Second):
			t.Error("owned TLS refusal PTY reader did not join")
		}
	})
	cursor := 0
	for _, prompt := range hostSetupPrompts[:11] {
		next, ok := waitWizardMarker(output, prompt.marker, cursor, 5*time.Second)
		if !ok {
			t.Fatal("owned TLS refusal wizard stopped before GitHub repository prompt")
		}
		cursor = next
		if _, err := fmt.Fprintln(terminal, prompt.answer); err != nil {
			t.Fatalf("answer owned TLS refusal wizard: %v", err)
		}
	}
	select {
	case err := <-waitDone:
		joined = true
		var exited *exec.ExitError
		if !errors.As(err, &exited) || exited.ExitCode() != 1 {
			t.Fatalf("TLS refusal wizard exited unexpectedly: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("TLS refusal wizard did not exit within bound: %v", ctx.Err())
	}
	select {
	case <-readerDone:
	case <-time.After(2 * time.Second):
		_ = terminal.Close()
		select {
		case <-readerDone:
		case <-time.After(2 * time.Second):
			t.Fatal("owned TLS refusal PTY reader did not join")
		}
	}
	text := output.snapshot()
	if !strings.Contains(text, "request did not produce a trusted response") || strings.Contains(text, "Issue label to watch") || trace.connects.Load() == 0 || trace.handshakes.Load() == 0 || trace.reached.Load() {
		t.Fatal("production wizard did not refuse the invalid TLS fixture before repository admission")
	}
	if _, err := os.Stat(config); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("TLS refusal wizard wrote a local configuration")
	}
}
