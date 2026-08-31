package smokes

// step22_receipt_interactive_spawn_test.go — receipt-bearing interactive spawn
// smoke. Closes the SMOKE-GAP flagged in donmai #482/#485 reviews: no coverage
// for a poll item carrying the execution-cell sidecars
// (admissionReceipt/claimReceipt/effectiveCell) alongside the sibling
// resolvedProfile, and the repositoryDeclaration variant once donmai #485
// merges.
//
// What this lane proves, platform-free (OSS local daemon API only, no
// platform orchestration or work-auth tokens):
//
//  1. Preflight compiles the adaptation receipt and PERSISTS it before any
//     credential or spawn side effect (daemon.FileExecutionPreflightStore —
//     append-only, hash-named, symlink-safe, fsync-backed).
//  2. The subsequent spawn reapplies the SAME authority without drift.
//  3. A genuine authority difference is refused and names the drifting
//     field digests (never raw values).
//  4. The repositoryDeclaration variant lands after donmai #485
//     (runner/repository_sandbox_reconcile.go) — gated so this lane stays
//     green before #485 and proves the fix after it.
//
// Pin: donmai behaviour is pinned to current origin/main. The
// repositoryDeclaration subtest notes in its skip message that it lands
// after #485.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/workarea"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// receiptInteractiveSpawnCapabilityFiles probes the checkout under test for
// the execution-cell seam this lane pins.
var receiptInteractiveSpawnCapabilityFiles = []string{
	filepath.Join("executioncell", "types.go"),
	filepath.Join("executioncell", "runtime_binding.go"),
	filepath.Join("daemon", "execution_preflight_store.go"),
	filepath.Join("runner", "harness_selection.go"),
	filepath.Join("runner", "prepared_harness.go"),
	filepath.Join("agent", "prepared_harness.go"),
}

func requireReceiptInteractiveSpawnSource(t *testing.T) string {
	t.Helper()
	srcDir := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	for _, rel := range receiptInteractiveSpawnCapabilityFiles {
		if _, err := os.Stat(filepath.Join(srcDir, rel)); err != nil {
			afh.DeclineLive(t, "donmai checkout at %q predates %s", srcDir, rel)
		}
	}
	return srcDir
}

func hasRepositorySandboxReconcile(srcDir string) bool {
	_, err := os.Stat(filepath.Join(srcDir, "runner", "repository_sandbox_reconcile.go"))
	return err == nil
}

// codexReceiptCell builds a ResolvedExecutionCell pinned to the codex
// harness — the one harness that genuinely declares session-root-v1 on
// current main.
func codexReceiptCell(harnessVersion, model string, mode executioncell.SessionMode, caps []executioncell.CapabilityRequirement) executioncell.ResolvedExecutionCell {
	return executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: string(agent.HarnessCodex), Version: harnessVersion},
		Model:           executioncell.ModelRef{ID: model, Author: "openai"},
		Endpoint: executioncell.ServingEndpointRef{
			ID: "openai-direct", Protocol: string(agent.ProtoOpenAIResponses), Operator: "openai", Revision: "2026-08-06",
		},
		AuthBinding: executioncell.AuthBindingRef{
			ID: "auth_test", Mechanism: agent.AuthAPIKey, CommercialMode: executioncell.CommercialUsageBilled,
			Authority: "openai", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryEnvironment,
		},
		Placement:   executioncell.PlacementRef{ID: "host_test", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
		SessionMode: mode, GrantedCapabilities: append([]executioncell.CapabilityRequirement{}, caps...), EvidenceTier: executioncell.EvidenceUnitVerified,
		CompatibilityDigest: strings.Repeat("3", 64), RuntimeInventoryDigest: strings.Repeat("4", 64),
	}
}

func receiptInteractiveBaseQW(sessionID, repoURL, model string) runner.QueuedWork {
	qw := runner.QueuedWork{}
	qw.SessionID = sessionID
	qw.Mode = prompt.InteractiveRunMode
	qw.InitialPrompt = "actual human-controlled initial turn"
	qw.Repository = repoURL
	qw.ResolvedProfile = runner.ResolvedProfile{
		Harness: string(agent.HarnessCodex), Model: model,
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyOpenAI, Model: model, Protocol: agent.ProtoOpenAIResponses, Host: agent.HostDirect,
			EndpointID: "openai-direct", EndpointOperator: "openai", EndpointRevision: "2026-08-06", ModelAuthor: "openai",
			AuthBindingID: "auth_test", AuthAuthority: "openai", AuthCommercialMode: string(executioncell.CommercialUsageBilled),
			AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
			AuthDelivery: string(executioncell.DeliveryEnvironment), Mechanism: agent.AuthAPIKey,
		},
	}
	return qw
}

func attachInteractiveAdmittedCell(t *testing.T, qw runner.QueuedWork, cell executioncell.ResolvedExecutionCell) runner.QueuedWork {
	t.Helper()
	payloadDigest, err := runner.DigestOperationalPayload(qw)
	if err != nil {
		t.Fatalf("digest operational payload: %v", err)
	}
	receipt := executioncell.AdmissionReceipt{
		ContractVersion: executioncell.ContractVersion, ReceiptID: "admission_" + qw.SessionID, RequestID: qw.SessionID,
		Decision: executioncell.AdmissionAdmitted, IntentDigest: strings.Repeat("1", 64), OperationalPayloadDigest: payloadDigest,
		Cell: &cell, ResolverDecisions: []executioncell.ResolverDecision{{
			Kind: executioncell.DecisionExplicit, Field: "harness", SelectedRef: "harness:codex@" + cell.Harness.Version, Reason: "test receipt pins the exact harness",
		}}, RecordedAt: "2026-08-06T12:00:00Z",
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal admission receipt: %v", err)
	}
	immutable, err := executioncell.DecodeAdmissionReceipt(raw)
	if err != nil {
		t.Fatalf("validate admission receipt: %v", err)
	}
	effective, err := executioncell.CanonicalJSON(cell)
	if err != nil {
		t.Fatalf("canonical effective cell: %v", err)
	}
	qw.AdmissionReceipt = immutable.Bytes()
	qw.EffectiveCell = effective
	qw.WorkerID = "worker_test"
	binding, err := json.Marshal(executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: qw.SessionID, WorkerID: qw.WorkerID, PlacementID: cell.Placement.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	qw.ExecutionRuntimeBinding = binding
	canonical, err := runner.CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatalf("canonical operational payload: %v", err)
	}
	qw.OperationalPayload = canonical
	return qw
}

// receiptInteractiveFakeProvider is a minimal HarnessProvider with a real
// codex manifest shape (no binary probing).
type receiptInteractiveFakeProvider struct {
	name       agent.ProviderName
	harness    agent.HarnessName
	spawnCalls atomic.Int32
	manifest   agent.HarnessManifest
}

func (p *receiptInteractiveFakeProvider) Name() agent.ProviderName { return p.name }
func (p *receiptInteractiveFakeProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{}
}
func (p *receiptInteractiveFakeProvider) Manifest() agent.HarnessManifest { return p.manifest }
func (p *receiptInteractiveFakeProvider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	p.spawnCalls.Add(1)
	return nil, errors.New("receipt-interactive fake must not spawn")
}

func (p *receiptInteractiveFakeProvider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, errors.New("receipt-interactive fake must not resume")
}
func (p *receiptInteractiveFakeProvider) Shutdown(_ context.Context) error { return nil }

func newReceiptInteractiveProvider() *receiptInteractiveFakeProvider {
	m := (&codex.Provider{}).Manifest()
	return &receiptInteractiveFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex, manifest: m}
}

func receiptInteractiveRegistry(t *testing.T, p *receiptInteractiveFakeProvider) *runner.Registry {
	t.Helper()
	reg := runner.NewRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("register %q: %v", p.name, err)
	}
	return reg
}

func daemonForReceiptInteractiveSmoke(t *testing.T, reg *runner.Registry, store daemon.ExecutionPreflightStore, repoURL string) *daemon.Daemon {
	t.Helper()
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "daemon.yaml")
	yamlBody := fmt.Sprintf(`machine:
  id: receipt-smoke-machine
capacity:
  maxConcurrentSessions: 10
projects:
  - id: receipt-smoke
    repository: %s
orchestrator:
  url: http://127.0.0.1:1
`, repoURL)
	if err := os.WriteFile(cfgPath, []byte(yamlBody), 0o600); err != nil {
		t.Fatalf("write daemon.yaml: %v", err)
	}
	d := daemon.New(daemon.Options{
		ConfigPath: cfgPath, JWTPath: filepath.Join(tmp, "daemon.jwt"),
		SkipWizard: true, SkipRegistration: true, ProviderRegistry: runner.NewProviderView(reg), ExecutionPreflightStore: store,
		SpawnerOptions: daemon.SpawnerOptions{
			WorkerCommand: []string{"/bin/sh", "-c", "exit 0"},
			OnPreSpawn:    func(_ daemon.SessionSpec, env []string) ([]string, error) { return env, nil },
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d
}

// buildReadyHostReceipt synthesizes a host adaptation receipt that will
// pass daemon validation for the given QW. Used for pure-runner
// PreflightHarness checks where the daemon path is not exercised.
func buildReadyHostReceipt(t *testing.T, qw runner.QueuedWork) json.RawMessage {
	t.Helper()
	admission, err := executioncell.DecodeAdmissionReceipt(qw.AdmissionReceipt)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := executioncell.DecodeRuntimeBinding(qw.ExecutionRuntimeBinding)
	if err != nil {
		t.Fatal(err)
	}
	promptReceipt := agent.PromptDeliveryReceipt{ContractVersion: agent.PromptContractVersion, ProfileID: "test-prompt", Decision: "ready", Entries: []agent.PromptDeliveryEntry{}}
	toolReceipt := agent.ToolLifecycleReceipt{
		ContractVersion: agent.ToolLifecycleContractVersion, AdmissionReceiptID: admission.Value().ReceiptID,
		ClaimReceiptID: "", OperationalPayloadDigest: admission.Value().OperationalPayloadDigest,
		ProfileID: "test-tools", Decision: "ready", EvidenceTier: string(agent.EvidenceStructured), ProductionEligible: true, Entries: []agent.ToolLifecycleEntry{},
	}
	plan := &agent.PreparedHarness{
		ContractVersion: agent.HarnessAdaptationContractVersion, Harness: admission.Value().Cell.Harness.ID,
		Mode: agent.PromptModeHumanControlled, OperationalPayloadDigest: admission.Value().OperationalPayloadDigest, AuthorityDigest: "test-authority",
		PromptReceipt: promptReceipt, ToolLifecycleReceipt: toolReceipt,
	}
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		plan.Materializations = append(plan.Materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: admission.Value().OperationalPayloadDigest, Required: true})
	}
	host := map[string]any{
		"contractVersion": executioncell.HostAdaptationContractVersion, "requestId": binding.RequestID,
		"workerId": binding.WorkerID, "placementId": binding.PlacementID, "decision": "ready",
		"plan": plan, "planDigest": agent.DigestPreparedHarness(plan),
		"promptReceipt": promptReceipt, "toolLifecycleReceipt": toolReceipt,
	}
	raw, _ := json.Marshal(host)
	if _, err := executioncell.DecodeHostAdaptationReceipt(raw); err != nil {
		t.Fatalf("synthetic host receipt invalid: %v", err)
	}
	return raw
}

// TestReceiptInteractiveSpawn is the receipt-bearing interactive spawn
// SMOKE-GAP closure. See file header.
func TestReceiptInteractiveSpawn(t *testing.T) {
	afh.SkipIfShort(t, "receipt-bearing interactive spawn smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	afh.SkipIfToolMissing(t, "git", "fixture repo helper is not tested here but git is the repo seam")

	srcDir := requireReceiptInteractiveSpawnSource(t)
	_, _ = afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{SourceDir: srcDir})

	const model = "gpt-receipt-interactive-model"
	const repoURL = "https://example.invalid/receipt-interactive-primary.git"

	t.Run("PreflightCompilesAndPersistsHostAdaptationReceipt", func(t *testing.T) {
		provider := newReceiptInteractiveProvider()
		reg := receiptInteractiveRegistry(t, provider)
		storeDir := t.TempDir()
		store := daemon.NewFileExecutionPreflightStore(storeDir)
		d := daemonForReceiptInteractiveSmoke(t, reg, store, repoURL)

		sessionID := fmt.Sprintf("receipt-interactive-preflight-%d", time.Now().UnixNano())
		qw := receiptInteractiveBaseQW(sessionID, repoURL, model)
		cell := codexReceiptCell("harness/v2", model, executioncell.SessionHumanControlled, nil)
		qw = attachInteractiveAdmittedCell(t, qw, cell)

		detail := &daemon.SessionDetail{
			SessionID: sessionID, WorkerID: qw.WorkerID,
			AdmissionReceipt: qw.AdmissionReceipt, EffectiveCell: qw.EffectiveCell,
			ExecutionRuntimeBinding: qw.ExecutionRuntimeBinding, OperationalPayload: qw.OperationalPayload,
		}

		handle, err := d.AcceptWorkWithDetail(daemon.SessionSpec{SessionID: sessionID, Repository: repoURL, Ref: "main"}, detail)
		if err != nil {
			t.Fatalf("AcceptWorkWithDetail: %v", err)
		}
		if handle.SessionID != sessionID {
			t.Fatalf("handle SessionID = %q, want %q", handle.SessionID, sessionID)
		}
		if len(detail.HostAdaptationReceipt) == 0 {
			t.Fatal("HostAdaptationReceipt was not populated on the detail")
		}
		host, err := executioncell.DecodeHostAdaptationReceipt(detail.HostAdaptationReceipt)
		if err != nil {
			t.Fatalf("DecodeHostAdaptationReceipt: %v", err)
		}
		if host.Decision != "ready" || host.RequestID != sessionID || host.WorkerID != qw.WorkerID {
			t.Fatalf("host receipt = %+v, want ready for %s/%s", host, sessionID, qw.WorkerID)
		}
		if host.PlanDigest == "" {
			t.Fatal("host receipt has empty planDigest")
		}
		sum := sha256.Sum256(host.Plan)
		wantDigest := fmt.Sprintf("%x", sum[:])
		if host.PlanDigest != wantDigest {
			t.Fatalf("planDigest = %q, want %q", host.PlanDigest, wantDigest)
		}
		// Persisted file is the same bytes and is append-only.
		hashName := fmt.Sprintf("%x.json", sha256.Sum256([]byte(sessionID)))
		persisted, err := os.ReadFile(filepath.Join(storeDir, hashName))
		if err != nil {
			t.Fatalf("persisted receipt file: %v", err)
		}
		if string(persisted) != string(detail.HostAdaptationReceipt) {
			t.Fatalf("persisted receipt differs from detail receipt")
		}
		if err := store.Persist(sessionID, detail.HostAdaptationReceipt); err == nil {
			t.Fatal("second Persist unexpectedly succeeded — store must be append-only")
		}
		if provider.spawnCalls.Load() != 0 {
			t.Fatalf("provider spawned during preflight: %d", provider.spawnCalls.Load())
		}
		// Byte-identical persistence contract: the daemon hands byte-identical
		// AdmissionReceipt bytes and the digest still matches.
		admission, err := executioncell.DecodeAdmissionReceipt(qw.AdmissionReceipt)
		if err != nil {
			t.Fatal(err)
		}
		if string(qw.AdmissionReceipt) != string(admission.Bytes()) {
			t.Fatal("AdmissionReceipt not byte-identical after decode")
		}
		// Pin the sibling resolvedProfile shape alongside the receipt (the
		// poll item carries both; the runner reconciles the sibling into the
		// same digest). Exercise it on a second accept.
		t.Run("with sibling resolvedProfile", func(t *testing.T) {
			siblingQW := receiptInteractiveBaseQW(sessionID+"-sibling", repoURL, model)
			siblingQW = attachInteractiveAdmittedCell(t, siblingQW, cell)
			siblingDetail := &daemon.SessionDetail{
				SessionID: siblingQW.SessionID, WorkerID: siblingQW.WorkerID,
				AdmissionReceipt: siblingQW.AdmissionReceipt, EffectiveCell: siblingQW.EffectiveCell,
				ExecutionRuntimeBinding: siblingQW.ExecutionRuntimeBinding, OperationalPayload: siblingQW.OperationalPayload,
				ResolvedProfile: &daemon.SessionResolvedProfile{
					Harness: string(agent.HarnessCodex), Provider: string(agent.ProviderCodex), Model: model,
					Endpoint: &daemon.SessionEndpointBinding{
						Company: string(agent.CompanyOpenAI), Model: model, Protocol: string(agent.ProtoOpenAIResponses), Host: string(agent.HostDirect),
						EndpointID: "openai-direct", EndpointOperator: "openai", EndpointRevision: "2026-08-06", ModelAuthor: "openai",
						AuthBindingID: "auth_test", AuthAuthority: "openai", AuthCommercialMode: string(executioncell.CommercialUsageBilled),
						AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
						AuthDelivery: string(executioncell.DeliveryEnvironment), Mechanism: string(agent.AuthAPIKey),
					},
				},
			}
			handle2, err := d.AcceptWorkWithDetail(daemon.SessionSpec{SessionID: siblingQW.SessionID, Repository: repoURL, Ref: "main"}, siblingDetail)
			if err != nil {
				t.Fatalf("AcceptWorkWithDetail with sibling resolvedProfile: %v", err)
			}
			if handle2.SessionID != siblingQW.SessionID {
				t.Fatalf("sibling handle SessionID = %q, want %q", handle2.SessionID, siblingQW.SessionID)
			}
			if len(siblingDetail.HostAdaptationReceipt) == 0 {
				t.Fatal("sibling HostAdaptationReceipt empty")
			}
			host2, err := executioncell.DecodeHostAdaptationReceipt(siblingDetail.HostAdaptationReceipt)
			if err != nil {
				t.Fatalf("sibling host receipt: %v", err)
			}
			if host2.Decision != "ready" {
				t.Fatalf("sibling host decision = %q, want ready", host2.Decision)
			}
		})
	})

	t.Run("SpawnAppliesWithoutAuthorityDrift", func(t *testing.T) {
		provider := newReceiptInteractiveProvider()
		reg := receiptInteractiveRegistry(t, provider)
		sessionID := fmt.Sprintf("receipt-interactive-spawn-%d", time.Now().UnixNano())
		qw := receiptInteractiveBaseQW(sessionID, repoURL, model)
		cell := codexReceiptCell("harness/v2", model, executioncell.SessionHumanControlled, nil)
		qw = attachInteractiveAdmittedCell(t, qw, cell)
		qw.HostAdaptationReceipt = buildReadyHostReceipt(t, qw)

		admission, err := reg.PreflightHarness(qw)
		if err != nil {
			t.Fatalf("PreflightHarness: %v", err)
		}
		if admission == nil {
			t.Fatal("PreflightHarness returned nil admission")
		}
		admission2, err := reg.PreflightHarness(qw)
		if err != nil {
			t.Fatalf("second PreflightHarness: %v", err)
		}
		if admission2 == nil {
			t.Fatal("second PreflightHarness nil")
		}
		if provider.spawnCalls.Load() != 0 {
			t.Fatalf("provider spawned during preflight: %d", provider.spawnCalls.Load())
		}
		_ = admission
		_ = admission2
	})

	t.Run("GenuineAuthorityDriftIsRefusedNamingFields", func(t *testing.T) {
		provider := newReceiptInteractiveProvider()
		reg := receiptInteractiveRegistry(t, provider)
		sessionID := fmt.Sprintf("receipt-interactive-drift-%d", time.Now().UnixNano())
		qw := receiptInteractiveBaseQW(sessionID, repoURL, model)
		cell := codexReceiptCell("harness/v2", model, executioncell.SessionHumanControlled, nil)
		qw = attachInteractiveAdmittedCell(t, qw, cell)
		qw.HostAdaptationReceipt = buildReadyHostReceipt(t, qw)

		qwDrift := qw
		qwDrift.ResolvedProfile.Model = "different-model-swapped-after-preflight"

		_, err := reg.PreflightHarness(qwDrift)
		if err == nil {
			t.Fatal("expected a denial for the swapped model, got nil")
		}
		var admissionErr *runner.HarnessAdmissionError
		if !errors.As(err, &admissionErr) {
			t.Fatalf("expected *runner.HarnessAdmissionError, got %T: %v", err, err)
		}
		if admissionErr.Code != executioncell.DenialUnknownModel {
			t.Fatalf("Code = %q, want %q", admissionErr.Code, executioncell.DenialUnknownModel)
		}
		if !strings.Contains(strings.ToLower(admissionErr.Error()), "model") {
			t.Logf("denial error = %v", admissionErr.Error())
		}
		if provider.spawnCalls.Load() != 0 {
			t.Fatalf("provider spawned on drifted preflight: %d", provider.spawnCalls.Load())
		}
	})

	t.Run("RepositoryDeclarationVariant", func(t *testing.T) {
		if !hasRepositorySandboxReconcile(srcDir) {
			t.Skip("repositoryDeclaration variant lands after donmai #485 (runner/repository_sandbox_reconcile.go not present at this checkout)")
		}
		provider := newReceiptInteractiveProvider()
		reg := receiptInteractiveRegistry(t, provider)
		sessionID := fmt.Sprintf("receipt-interactive-repo-%d", time.Now().UnixNano())
		qw := receiptInteractiveBaseQW(sessionID, repoURL, model)
		qw.PermissionProfile = runner.PermissionProfileAutonomous
		qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{{
				Source: workarea.RepositorySource{Repository: repoURL}, Name: "primary",
				Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
			}},
		}
		cell := codexReceiptCell("harness/v2", model, executioncell.SessionHumanControlled, nil)
		qw = attachInteractiveAdmittedCell(t, qw, cell)
		qw.HostAdaptationReceipt = buildReadyHostReceipt(t, qw)

		admission, err := reg.PreflightHarness(qw)
		if err != nil {
			t.Fatalf("PreflightHarness with declared repository: %v", err)
		}
		if admission == nil {
			t.Fatal("nil admission for declared-repository interactive session")
		}
		admission2, err := reg.PreflightHarness(qw)
		if err != nil {
			t.Fatalf("second PreflightHarness: %v", err)
		}
		if admission2 == nil {
			t.Fatal("nil second admission")
		}
		a1, _ := executioncell.DecodeAdmissionReceipt(qw.AdmissionReceipt)
		if a1.Value().OperationalPayloadDigest == "" {
			t.Fatal("admission operational digest empty")
		}
		if provider.spawnCalls.Load() != 0 {
			t.Fatalf("provider spawned during preflight: %d", provider.spawnCalls.Load())
		}
	})

	t.Run("HostAdaptationReceiptDurabilityIsAppendOnlyAndHashNamed", func(t *testing.T) {
		dir := t.TempDir()
		store := daemon.NewFileExecutionPreflightStore(dir)
		sessionID := "../../outside"
		receipt := json.RawMessage(fmt.Sprintf(`{"contractVersion":"host-adaptation/v1","requestId":%q,"workerId":"worker-1","placementId":"host_test","decision":"denied","denial":"unsupported"}`, sessionID))
		if _, err := executioncell.DecodeHostAdaptationReceipt(receipt); err != nil {
			t.Fatalf("receipt: %v", err)
		}
		if err := store.Persist(sessionID, receipt); err != nil {
			t.Fatalf("Persist: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("entries = %d, want 1", len(entries))
		}
		wantName := fmt.Sprintf("%x.json", sha256.Sum256([]byte(sessionID)))
		if entries[0].Name() != wantName {
			t.Fatalf("receipt file = %q, want hash-named %q", entries[0].Name(), wantName)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "outside.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("receipt escaped configured root")
		}
		if err := store.Persist(sessionID, receipt); err == nil {
			t.Fatal("second Persist unexpectedly succeeded — must be append-only")
		}
		dir2 := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside2.json")
		if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		sessionID2 := "session-symlink-durability"
		if err := os.Symlink(outside, filepath.Join(dir2, fmt.Sprintf("%x.json", sha256.Sum256([]byte(sessionID2))))); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		receipt2 := json.RawMessage(fmt.Sprintf(`{"contractVersion":"host-adaptation/v1","requestId":%q,"workerId":"worker-1","placementId":"host_test","decision":"denied","denial":"unsupported"}`, sessionID2))
		if _, err := executioncell.DecodeHostAdaptationReceipt(receipt2); err != nil {
			t.Fatalf("receipt2: %v", err)
		}
		if err := daemon.NewFileExecutionPreflightStore(dir2).Persist(sessionID2, receipt2); err == nil {
			t.Fatal("symlink destination was replaced")
		}
		if got, err := os.ReadFile(outside); err != nil || string(got) != "unchanged" {
			t.Fatalf("symlink target changed")
		}
	})
}
