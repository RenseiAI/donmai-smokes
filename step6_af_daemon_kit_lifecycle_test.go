package smokes

// step6_af_daemon_kit_lifecycle_test.go — customer-visible Wave 12
// acceptance test. Drives a real `donmai daemon run` through the full kit
// lifecycle:
//
//   1. Permissive mode + git-source install of an unsigned kit →
//      HTTP 200 + trust=unsigned, persisted under kit.scanPaths[0],
//      visible on GET /api/daemon/kits.
//   2. GET /api/daemon/kits/<id>/verify-signature against the persisted
//      manifest → trust=unsigned (no sibling .sigstore present).
//   3. Tamper test: write a malformed sibling `.sigstore` file next to
//      the persisted manifest, re-issue verify-signature →
//      trust=signed-unverified (the verifier sees the bundle, fails to
//      parse it, reports unverified).
//   4. Restart the daemon with trust.mode=signed-by-allowlist + empty
//      trust.issuerSet:
//        4a. POST /api/daemon/kits/<id>/install on the same unsigned
//            kit → HTTP 403 with body trust=signed-unverified.
//        4b. POST install with trustOverride: "allowed-this-once" →
//            HTTP 200 (gate bypassed; structured slog audit log is
//            emitted by the daemon — covered by Phase 3 unit tests, not
//            asserted over the wire here).
//
// Per WAVE12_PLAN.md § "Phase 6". This is the Wave 12 acceptance gate.
//
// CARRYOVER (Wave 13+): The "signed-verified" honest end-to-end against
// the live `af` binary requires a real Sigstore-public-good-instance
// signed bundle (the daemon embeds the public production trust root
// at build time and exposes no override knob). A hermetic
// VirtualSigstore CA can sign offline but cannot chain to the embedded
// production trust root, so any offline-signed bundle would deliver
// trust=signed-unverified rather than trust=signed-verified. The
// Phase 3 / Phase 4 unit tests inside donmai's `daemon`
// package cover signed-verified end-to-end via the
// `newKitVerifierWithMaterial` test seam (see kit_trust_test.go and
// kit_install_git_test.go for the in-process exercise).
//
// To unblock the live-binary signed-verified path, Wave 13+ needs one
// of:
//   - REN-1344's productionized signing CI emitting bundles signed
//     against the public Sigstore good instance (which the embedded
//     trust root validates), OR
//   - daemon-side support for an alternative trust root (e.g.,
//     daemon.yaml: `trust.rootPath: <path>` or RENSEI_TRUST_ROOT env
//     var) so a hermetic VirtualSigstore root can be injected at
//     spawn time.
//
// Both fall outside the Wave 12 sealed scope (Phase 3 + Phase 4 daemon
// surface) and outside the donmai-smokes write-target boundary
// for this Phase 6 sub-agent.
//
// Skip-mode: honours DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 + -short, matching
// step1-step5's pattern.
//
// Timing: warm cache ~3-5s (single live spin-up per trust mode + ~6
// HTTP calls + a daemon restart). Cold cache adds 60-90s for the donmai
// binary build.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// kitLifecycleManifestTOML is the minimal-valid kit manifest we install.
// The kit.id is a plain segment (no slash) so the persisted on-disk
// filename matches the kit.id verbatim — keeps the tamper-bundle test's
// path arithmetic simple. authorIdentity is set so the audit log's
// signerId backfill is deterministic.
const kitLifecycleManifestTOML = `api = "rensei.dev/v1"

[kit]
id = "smoke-lifecycle-kit"
name = "Smoke Lifecycle Kit"
version = "0.0.1"
description = "Fixture kit for TestAfDaemonKitLifecycleHonestEndToEnd."
authorIdentity = "did:web:rensei.dev"
`

// kitLifecycleID matches kit.id in kitLifecycleManifestTOML. The
// persisted filename is `<sanitizedID>.kit.toml`; with no slashes
// in the id, the sanitizer is a no-op so the persisted basename
// is `smoke-lifecycle-kit.kit.toml`.
const kitLifecycleID = "smoke-lifecycle-kit"

// kitLifecyclePersistedFilename is the on-disk basename the daemon's
// installFromGit path writes into kit.scanPaths[0]. Anchored so the
// tamper test can find the persisted manifest deterministically.
const kitLifecyclePersistedFilename = "smoke-lifecycle-kit.kit.toml"

// permissiveDaemonYAML carries permissive trust mode + a single
// kit.scanPaths entry. The %s placeholder is interpolated with the
// per-test kit scan dir before LiveDaemonWithConfig writes the file.
//
// Standard projects/orchestrator/capacity boilerplate mirrors the
// step4 + step5 shape so the daemon clears its config validation gate.
const permissiveDaemonYAML = `apiVersion: rensei.dev/v1
kind: LocalDaemon
machine:
  id: smoke-machine
capacity:
  maxConcurrentSessions: 2
  maxVCpuPerSession: 2
  maxMemoryMbPerSession: 2048
  reservedForSystem:
    vCpu: 1
    memoryMb: 1024
projects:
  - id: smoke-alpha
    repository: github.com/foo/rensei-smokes-alpha
    cloneStrategy: shallow
orchestrator:
  url: http://127.0.0.1:1
autoUpdate:
  channel: stable
  schedule: manual
  drainTimeoutSeconds: 5
trust:
  mode: permissive
kit:
  scanPaths:
    - %s
`

// allowlistDaemonYAML is the same shape but with trust.mode=signed-by-
// allowlist + an explicitly-empty trust.issuerSet. Empty issuerSet means
// the embedded trust root still gates which CAs are trusted, but the
// SAN-pattern filter is skipped (matches Phase 3's
// buildIdentityPolicies behaviour). For our unsigned-kit smoke, the
// gate rejects regardless because the manifest never even produces a
// trust=signed-verified outcome.
const allowlistDaemonYAML = `apiVersion: rensei.dev/v1
kind: LocalDaemon
machine:
  id: smoke-machine
capacity:
  maxConcurrentSessions: 2
  maxVCpuPerSession: 2
  maxMemoryMbPerSession: 2048
  reservedForSystem:
    vCpu: 1
    memoryMb: 1024
projects:
  - id: smoke-alpha
    repository: github.com/foo/rensei-smokes-alpha
    cloneStrategy: shallow
orchestrator:
  url: http://127.0.0.1:1
autoUpdate:
  channel: stable
  schedule: manual
  drainTimeoutSeconds: 5
trust:
  mode: signed-by-allowlist
  issuerSet: []
kit:
  scanPaths:
    - %s
`

// TestAfDaemonKitLifecycleHonestEndToEnd is the Wave 12 customer-
// visible acceptance test. Drives a real `donmai daemon run` through the
// kit-install + verify-signature lifecycle in both permissive and
// signed-by-allowlist trust modes against a local git fixture.
//
// See file-level godoc for the carryover explaining why the
// trust=signed-verified happy-path defers to Wave 13+.
func TestAfDaemonKitLifecycleHonestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end live-daemon test; skipped under -short")
	}
	if os.Getenv("DONMAI_SMOKES_SKIP_LIVE_DAEMON") == "1" {
		t.Skip("DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 — operator opted out of the live-daemon smoke")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary unavailable on PATH: %v", err)
	}

	// ─── Local git fixture ─────────────────────────────────────────────
	//
	// Build a real git repo on disk with the kit manifest as the only
	// committed file. The fetcher clones via go-git over a `file://`
	// URL — the same code path Phase 4's gitKitFetcher exercises in
	// kit_install_git_test.go, here driven through the daemon's HTTP
	// install handler against a live binary.
	//
	// We set Author/Committer via env so the test doesn't depend on the
	// operator's global git config (CI environments often have no
	// configured user.email).
	repoDir := t.TempDir()
	manifestRelPath := "smoke-lifecycle-kit.kit.toml"
	if err := os.WriteFile(filepath.Join(repoDir, manifestRelPath), []byte(kitLifecycleManifestTOML), 0o600); err != nil {
		t.Fatalf("write fixture manifest: %v", err)
	}
	gitInitFixture(t, repoDir)
	repoURL := "file://" + repoDir

	// ─── Permissive daemon ─────────────────────────────────────────────
	//
	// kitScanDir is allocated from a separate t.TempDir() so the
	// scanPaths entry survives the daemon restart later in the test
	// (each daemon spawn gets its own HOME but we keep scanPaths
	// pointing at this stable location). This lets us assert that the
	// re-installed kit lands in the same on-disk location across both
	// trust-mode runs.
	kitScanDir := filepath.Join(t.TempDir(), "smoke-kits")
	if err := os.MkdirAll(kitScanDir, 0o700); err != nil {
		t.Fatalf("mkdir kit scan dir: %v", err)
	}

	permissiveYAML := fmt.Sprintf(permissiveDaemonYAML, kitScanDir)
	live, _, logBuf, _ := afh.LiveDaemonWithConfig(t, permissiveYAML)

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// ─── 1. Permissive install of unsigned kit ─────────────────────────
	//
	// The daemon clones repoURL @ HEAD, finds the manifest at the repo
	// root, runs the verifier (no sibling .sigstore → trust=unsigned),
	// gate allows under permissive mode, persists into kitScanDir.
	{
		installURL := live.URL + "/api/daemon/kits/" + kitLifecycleID + "/install"
		body := map[string]any{
			"source": map[string]any{
				"kind": "git",
				"url":  repoURL,
			},
		}
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal install body: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, installURL, bytes.NewReader(bodyBytes))
		if err != nil {
			t.Fatalf("build install request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("POST install: %v\n--- daemon log tail ---\n%s", err, logBuf.String())
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("install status = %d, want 200\n--- body ---\n%s\n--- daemon log tail ---\n%s",
				resp.StatusCode, respBody, logBuf.String())
		}

		var installResp struct {
			Kit struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Version  string `json:"version"`
				Trust    string `json:"trust"`
				SignerID string `json:"signerId"`
				Status   string `json:"status"`
			} `json:"kit"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(respBody, &installResp); err != nil {
			t.Fatalf("decode install response: %v\n--- body ---\n%s", err, respBody)
		}
		if installResp.Kit.ID != kitLifecycleID {
			t.Errorf("install kit.id = %q, want %q", installResp.Kit.ID, kitLifecycleID)
		}
		if installResp.Kit.Trust != "unsigned" {
			t.Errorf("install kit.trust = %q, want unsigned (no sibling .sigstore in fixture)", installResp.Kit.Trust)
		}
		// SignerID backfilled from manifest authorIdentity per Phase 4's
		// installFromGit fallback.
		if installResp.Kit.SignerID != "did:web:rensei.dev" {
			t.Errorf("install kit.signerId = %q, want did:web:rensei.dev (from manifest authorIdentity)",
				installResp.Kit.SignerID)
		}
		if installResp.Message == "" {
			t.Errorf("install response missing Message")
		}
		t.Logf("permissive install: kit=%s trust=%s message=%q",
			installResp.Kit.ID, installResp.Kit.Trust, installResp.Message)
	}

	// Confirm the manifest was persisted under kitScanDir at the
	// expected path. Anchors the smoke against the daemon's
	// sanitizeKitFilename + scanPaths[0] write contract.
	persistedManifestPath := filepath.Join(kitScanDir, kitLifecyclePersistedFilename)
	if _, err := os.Stat(persistedManifestPath); err != nil {
		t.Fatalf("persisted manifest %q not found after install: %v", persistedManifestPath, err)
	}

	// ─── 2. Kit appears on GET /api/daemon/kits ────────────────────────
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, live.URL+"/api/daemon/kits", nil)
		if err != nil {
			t.Fatalf("build list kits request: %v", err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/daemon/kits: %v\n--- daemon log tail ---\n%s", err, logBuf.String())
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list kits status = %d, want 200\n--- body ---\n%s", resp.StatusCode, respBody)
		}

		var kitsResp struct {
			Kits []struct {
				ID    string `json:"id"`
				Trust string `json:"trust"`
			} `json:"kits"`
		}
		if err := json.Unmarshal(respBody, &kitsResp); err != nil {
			t.Fatalf("decode list kits: %v\n--- body ---\n%s", err, respBody)
		}
		var found bool
		for _, k := range kitsResp.Kits {
			if k.ID == kitLifecycleID {
				found = true
				if k.Trust != "unsigned" {
					t.Errorf("list kits trust = %q, want unsigned", k.Trust)
				}
				break
			}
		}
		if !found {
			t.Fatalf("GET /api/daemon/kits did not surface %q after install\n--- body ---\n%s",
				kitLifecycleID, respBody)
		}
		t.Logf("permissive list-kits: %s present with trust=unsigned", kitLifecycleID)
	}

	// ─── 3. verify-signature against the persisted (unsigned) manifest ─
	//
	// The on-disk path has no sibling .sigstore — the verifier walks
	// the manifest, finds no bundle, returns trust=unsigned. This is
	// the read-back of the install-time outcome, proving the verifier
	// runs against persisted state and not just the freshly-cloned tree.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		verifyURL := live.URL + "/api/daemon/kits/" + kitLifecycleID + "/verify-signature"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL, nil)
		if err != nil {
			t.Fatalf("build verify request: %v", err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET verify-signature: %v\n--- daemon log tail ---\n%s", err, logBuf.String())
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("verify-signature status = %d, want 200\n--- body ---\n%s", resp.StatusCode, respBody)
		}

		var verifyResp struct {
			KitID string `json:"kitId"`
			Trust string `json:"trust"`
			OK    bool   `json:"ok"`
		}
		if err := json.Unmarshal(respBody, &verifyResp); err != nil {
			t.Fatalf("decode verify-signature: %v\n--- body ---\n%s", err, respBody)
		}
		if verifyResp.KitID != kitLifecycleID {
			t.Errorf("verify-signature kitId = %q, want %q", verifyResp.KitID, kitLifecycleID)
		}
		if verifyResp.Trust != "unsigned" {
			t.Errorf("verify-signature trust = %q, want unsigned (no sibling .sigstore)", verifyResp.Trust)
		}
		if !verifyResp.OK {
			t.Errorf("verify-signature ok = false, want true (verifier ran cleanly)")
		}
		t.Logf("permissive verify-signature: trust=%s ok=%v", verifyResp.Trust, verifyResp.OK)
	}

	// ─── 4. Tamper test: write a bogus sibling .sigstore on disk ───────
	//
	// The daemon's verifier reads <manifestPath>.sigstore via
	// bundle.LoadJSONFromPath. Garbage content fails to parse →
	// trust=signed-unverified per Phase 3's VerifyManifest contract:
	//
	//   if err := bundle.LoadJSONFromPath(...); err != nil {
	//       res.Trust = afclient.KitTrustSignedUnverified
	//       res.Details = fmt.Sprintf("parse bundle: %v", err)
	//   }
	//
	// Anchors the on-disk verify path end-to-end — the daemon really
	// does honour the sibling .sigstore on every verify-signature call.
	persistedBundlePath := persistedManifestPath + ".sigstore"
	if err := os.WriteFile(persistedBundlePath, []byte("not-a-real-sigstore-bundle"), 0o600); err != nil {
		t.Fatalf("write tampered bundle: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(persistedBundlePath) })

	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		verifyURL := live.URL + "/api/daemon/kits/" + kitLifecycleID + "/verify-signature"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL, nil)
		if err != nil {
			t.Fatalf("build tamper-verify request: %v", err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET verify-signature (tampered): %v\n--- daemon log tail ---\n%s",
				err, logBuf.String())
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("tamper verify-signature status = %d, want 200 (verifier reports outcome via Trust, not HTTP)\n--- body ---\n%s",
				resp.StatusCode, respBody)
		}

		var verifyResp struct {
			KitID   string `json:"kitId"`
			Trust   string `json:"trust"`
			OK      bool   `json:"ok"`
			Details string `json:"details"`
		}
		if err := json.Unmarshal(respBody, &verifyResp); err != nil {
			t.Fatalf("decode tamper verify-signature: %v\n--- body ---\n%s", err, respBody)
		}
		if verifyResp.Trust != "signed-unverified" {
			t.Errorf("tampered-bundle trust = %q, want signed-unverified", verifyResp.Trust)
		}
		if !verifyResp.OK {
			t.Errorf("tampered-bundle ok = %v, want true (verifier ran, just rejected bundle)", verifyResp.OK)
		}
		if verifyResp.Details == "" {
			t.Errorf("tampered-bundle details empty, want parse-bundle error explanation")
		}
		t.Logf("tamper test: trust=%s details=%q", verifyResp.Trust, verifyResp.Details)
	}

	// Stop the permissive daemon before bringing up the allowlist one.
	// LiveDaemon.Stop is idempotent so the t.Cleanup hook chained inside
	// LiveDaemonWithConfig still runs harmlessly.
	live.Stop()

	// Remove the tampered bundle so the allowlist install path sees a
	// clean unsigned kit (the bundle would otherwise interfere with the
	// install-time verification the daemon runs against the freshly-
	// fetched manifest, but more importantly it would conflate the
	// re-install assertion).
	if err := os.Remove(persistedBundlePath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("cleanup tampered bundle: %v", err)
	}
	// Also remove the persisted manifest so the re-install lands cleanly.
	// The daemon's installFromGit overwrites via atomic rename so this
	// isn't strictly required, but it makes the precondition explicit.
	if err := os.Remove(persistedManifestPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("cleanup persisted manifest: %v", err)
	}

	// ─── 5. Allowlist daemon ───────────────────────────────────────────
	//
	// Same kitScanDir so the install side-effects are observable across
	// the restart boundary. trust.mode=signed-by-allowlist with empty
	// issuerSet means any non-signed-verified outcome is rejected at the
	// install gate.
	allowlistYAML := fmt.Sprintf(allowlistDaemonYAML, kitScanDir)
	allowLive, _, allowLog, _ := afh.LiveDaemonWithConfig(t, allowlistYAML)

	// ─── 5a. Allowlist rejects unsigned install ────────────────────────
	{
		installURL := allowLive.URL + "/api/daemon/kits/" + kitLifecycleID + "/install"
		body := map[string]any{
			"source": map[string]any{
				"kind": "git",
				"url":  repoURL,
			},
		}
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal allowlist install body: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, installURL, bytes.NewReader(bodyBytes))
		if err != nil {
			t.Fatalf("build allowlist install request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("POST install (allowlist): %v\n--- daemon log tail ---\n%s",
				err, allowLog.String())
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("allowlist install status = %d, want 403 (gate rejection)\n--- body ---\n%s\n--- daemon log tail ---\n%s",
				resp.StatusCode, respBody, allowLog.String())
		}

		var rejectResp struct {
			Error string `json:"error"`
			KitID string `json:"kitId"`
			Trust string `json:"trust"`
		}
		if err := json.Unmarshal(respBody, &rejectResp); err != nil {
			t.Fatalf("decode allowlist reject body: %v\n--- body ---\n%s", err, respBody)
		}
		if rejectResp.Trust != "signed-unverified" {
			t.Errorf("allowlist reject body trust = %q, want signed-unverified", rejectResp.Trust)
		}
		if rejectResp.Error == "" {
			t.Errorf("allowlist reject body missing error string")
		}
		t.Logf("allowlist reject: status=403 trust=%s error=%q",
			rejectResp.Trust, rejectResp.Error)

		// Manifest must NOT have been persisted on rejection — the gate
		// runs before the scanPath copy.
		if _, err := os.Stat(persistedManifestPath); !os.IsNotExist(err) {
			t.Errorf("manifest unexpectedly persisted at %q after allowlist rejection: %v",
				persistedManifestPath, err)
		}
	}

	// ─── 5b. trustOverride: allowed-this-once bypasses the gate ────────
	//
	// Same install body plus the override field. The daemon emits a
	// structured slog audit log (covered by Phase 3's
	// TestKitRegistry_InstallTrustOverrideAuditLogs unit test); we
	// assert only the HTTP 200 here because the audit log goes through
	// the daemon's slog sink, not the wire response.
	{
		installURL := allowLive.URL + "/api/daemon/kits/" + kitLifecycleID + "/install"
		body := map[string]any{
			"source": map[string]any{
				"kind": "git",
				"url":  repoURL,
			},
			"trustOverride": "allowed-this-once",
		}
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal override install body: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, installURL, bytes.NewReader(bodyBytes))
		if err != nil {
			t.Fatalf("build override install request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("POST install (trustOverride): %v\n--- daemon log tail ---\n%s",
				err, allowLog.String())
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("override install status = %d, want 200 (gate bypassed)\n--- body ---\n%s\n--- daemon log tail ---\n%s",
				resp.StatusCode, respBody, allowLog.String())
		}

		var overrideResp struct {
			Kit struct {
				ID    string `json:"id"`
				Trust string `json:"trust"`
			} `json:"kit"`
		}
		if err := json.Unmarshal(respBody, &overrideResp); err != nil {
			t.Fatalf("decode override response: %v\n--- body ---\n%s", err, respBody)
		}
		if overrideResp.Kit.ID != kitLifecycleID {
			t.Errorf("override kit.id = %q, want %q", overrideResp.Kit.ID, kitLifecycleID)
		}
		// Trust on the override path stays at unsigned — the override
		// bypasses the gate but doesn't change the verifier outcome.
		if overrideResp.Kit.Trust != "unsigned" {
			t.Errorf("override kit.trust = %q, want unsigned (override bypasses gate; verifier outcome unchanged)",
				overrideResp.Kit.Trust)
		}
		t.Logf("override install: kit=%s trust=%s status=200",
			overrideResp.Kit.ID, overrideResp.Kit.Trust)

		// Manifest persisted now that the gate was bypassed.
		if _, err := os.Stat(persistedManifestPath); err != nil {
			t.Errorf("persisted manifest %q missing after override install: %v",
				persistedManifestPath, err)
		}
	}
}

// gitInitFixture initialises a fresh git repository at repoDir, commits
// every file already staged in the working tree, and exits cleanly. The
// commit author is supplied via env vars so the test does not depend on
// the operator's global git config.
//
// Shell-out to `git` keeps the smokes module dep-light (no go-git import
// needed) and matches the donmai Phase 4 fixture pattern at
// the operator-facing level — the daemon's gitKitFetcher will clone
// this repo via go-git over a `file://` URL once we hand it that URL.
func gitInitFixture(t *testing.T, repoDir string) {
	t.Helper()

	commitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Smoke Fixture",
		"GIT_AUTHOR_EMAIL=fixture@rensei.dev",
		"GIT_COMMITTER_NAME=Smoke Fixture",
		"GIT_COMMITTER_EMAIL=fixture@rensei.dev",
		// HOME redirect prevents `git init` from reading the operator's
		// global gitconfig (init.defaultBranch, signing keys, etc.). The
		// fixture only needs an in-tree HEAD; defaults are fine.
		"HOME="+t.TempDir(),
	)

	// Fixed 30-second budget per git invocation — `git init` + `git add` +
	// `git commit` are all sub-second on a clean tempdir, so this is
	// generous headroom for slow CI agents.
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"add", "."},
		{"commit", "--quiet", "-m", "seed: initial fixture commit"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // hermetic test fixture
		cmd.Dir = repoDir
		cmd.Env = commitEnv
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("git %v in %s failed: %v\n--- output ---\n%s",
				args, repoDir, err, out)
		}
	}
}
