package released_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

const (
	releasedModule         = "github.com/RenseiAI/donmai"
	releasedVersion        = "v0.54.0"
	releasedModuleSum      = "h1:rJ4ozWp85r5+I5m+UtRWFA7yCj1mqV6d8TajVpI94iU="
	releasedModuleGoModSum = "h1:MWoWFIhXU0nKcNmPrlwaGHIIVK/efcrg6YPY76vuvxw="
	releasedCommit         = "f911e1af373d4610b4f4b262008ff386504c5140"

	releaseBaseURL         = "https://github.com/RenseiAI/donmai/releases/download/v0.54.0/"
	checksumManifestSHA256 = "1d684c0551b1fa0b8417004f29210f2496c0de3ad5f6a5ef7d7297b9681cc71a"
	checksumManifest       = "d39fcaa55d1e131883f39d0adccc4c82fe5f1cbdd34d16ec3611bf3e0828857b  donmai_0.54.0_darwin_amd64.tar.gz\n" +
		"74d27408250bdbb510a143008d7e4db633825770963a557be4e6db4e3300e0af  donmai_0.54.0_darwin_arm64.tar.gz\n" +
		"fdf7a42ac1a1763c7b96e1130c6d77736e99c3a9ee274c655a24a159c8eb604b  donmai_0.54.0_linux_amd64.tar.gz\n" +
		"1e91bcb11531e0c446abe5a54ead9f1b1048188dfe44fe7702771b185c01b2fc  donmai_0.54.0_linux_arm64.tar.gz\n"

	testSessionID       = "11111111-1111-4111-8111-111111111111"
	testInvocationID    = "22222222-2222-4222-8222-222222222222"
	testClaimID         = "33333333-3333-4333-8333-333333333333"
	testWrongClaimID    = "44444444-4444-4444-8444-444444444444"
	testTerminalResult  = "tr_11111111111111111111111111111111"
	testWorkareaID      = "wa_22222222222222222222222222222222"
	testReceiverKey     = "rcv_33333333333333333333333333333333"
	testQuarantineID    = "twq_44444444444444444444444444444444"
	staleAuthorization  = "Bearer stale-release-token"
	freshAuthorization  = "Bearer fresh-release-token"
	terminalStatusRoute = "/terminal-status"
)

var releasedArchiveSHA256 = map[string]string{
	"darwin/amd64": "d39fcaa55d1e131883f39d0adccc4c82fe5f1cbdd34d16ec3611bf3e0828857b",
	"darwin/arm64": "74d27408250bdbb510a143008d7e4db633825770963a557be4e6db4e3300e0af",
	"linux/amd64":  "fdf7a42ac1a1763c7b96e1130c6d77736e99c3a9ee274c655a24a159c8eb604b",
	"linux/arm64":  "1e91bcb11531e0c446abe5a54ead9f1b1048188dfe44fe7702771b185c01b2fc",
}

func TestReleasedModuleAndBinaryIdentity(t *testing.T) {
	root := repositoryRoot(t)
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	goModText := string(goMod)
	if !strings.Contains(goModText, releasedModule+" "+releasedVersion) {
		t.Fatalf("go.mod does not pin %s %s", releasedModule, releasedVersion)
	}
	for _, forbidden := range []string{"replace ", "../donmai", "=>"} {
		if strings.Contains(goModText, forbidden) {
			t.Fatalf("go.mod contains forbidden released-consumer fallback %q", forbidden)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "go.work")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("released consumer must not use a repository go.work: %v", err)
	}

	listCommand := exec.Command("go", "list", "-m", "-json", releasedModule)
	listCommand.Dir = root
	listCommand.Env = append(os.Environ(), "GOWORK=off")
	listOutput, err := listCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve selected module: %v\n%s", err, listOutput)
	}
	var selected struct {
		Path    string
		Version string
		Sum     string
		Replace *struct {
			Path    string
			Version string
		}
	}
	if err := json.Unmarshal(listOutput, &selected); err != nil {
		t.Fatalf("decode go list output: %v\n%s", err, listOutput)
	}
	if selected.Path != releasedModule || selected.Version != releasedVersion || selected.Sum != releasedModuleSum {
		t.Fatalf("selected module mismatch: %+v", selected)
	}
	if selected.Replace != nil {
		t.Fatalf("selected module unexpectedly replaced by %+v", selected.Replace)
	}

	downloadCommand := exec.Command("go", "mod", "download", "-json", releasedModule+"@"+releasedVersion)
	downloadCommand.Dir = root
	downloadCommand.Env = append(os.Environ(), "GOWORK=off")
	downloadOutput, err := downloadCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("download immutable module: %v\n%s", err, downloadOutput)
	}
	var downloaded struct {
		Path     string
		Version  string
		Sum      string
		GoModSum string
		Origin   struct {
			VCS  string
			URL  string
			Hash string
			Ref  string
		}
	}
	if err := json.Unmarshal(downloadOutput, &downloaded); err != nil {
		t.Fatalf("decode go mod download output: %v\n%s", err, downloadOutput)
	}
	if downloaded.Path != releasedModule || downloaded.Version != releasedVersion || downloaded.Sum != releasedModuleSum || downloaded.GoModSum != releasedModuleGoModSum {
		t.Fatalf("downloaded module mismatch: %+v", downloaded)
	}
	if downloaded.Origin.VCS != "git" || downloaded.Origin.URL != "https://github.com/RenseiAI/donmai" || downloaded.Origin.Hash != releasedCommit || downloaded.Origin.Ref != "refs/tags/"+releasedVersion {
		t.Fatalf("downloaded module origin mismatch: %+v", downloaded.Origin)
	}

	verifyCommand := exec.Command("go", "mod", "verify")
	verifyCommand.Dir = root
	verifyCommand.Env = append(os.Environ(), "GOWORK=off")
	verifyOutput, err := verifyCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod verify: %v\n%s", err, verifyOutput)
	}
	if strings.TrimSpace(string(verifyOutput)) != "all modules verified" {
		t.Fatalf("unexpected go mod verify output: %q", verifyOutput)
	}

	manifest := downloadReleaseFile(t, "checksums.txt", 1<<20)
	manifestDigest := sha256.Sum256(manifest)
	if got := hex.EncodeToString(manifestDigest[:]); got != checksumManifestSHA256 {
		t.Fatalf("checksums.txt sha256 = %s, want %s", got, checksumManifestSHA256)
	}
	if string(manifest) != checksumManifest {
		t.Fatalf("checksums.txt content mismatch:\n%s", manifest)
	}

	platform := runtime.GOOS + "/" + runtime.GOARCH
	expectedArchiveDigest, ok := releasedArchiveSHA256[platform]
	if !ok {
		t.Fatalf("Donmai %s publishes no binary asset for %s", releasedVersion, platform)
	}
	archiveName := fmt.Sprintf("donmai_0.54.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := downloadReleaseFile(t, archiveName, 64<<20)
	archiveDigest := sha256.Sum256(archive)
	if got := hex.EncodeToString(archiveDigest[:]); got != expectedArchiveDigest {
		t.Fatalf("%s sha256 = %s, want %s", archiveName, got, expectedArchiveDigest)
	}
	if !strings.Contains(checksumManifest, expectedArchiveDigest+"  "+archiveName+"\n") {
		t.Fatalf("checksums.txt lacks exact entry for %s", archiveName)
	}

	binaryPath := extractDonmaiBinary(t, archive)
	versionOutput, err := exec.Command(binaryPath, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("donmai --version: %v\n%s", err, versionOutput)
	}
	if got := strings.TrimSpace(string(versionOutput)); got != "donmai version 0.54.0" {
		t.Fatalf("donmai --version = %q", got)
	}
}

func TestReleasedTerminalAcknowledgementLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workareaPath := filepath.Join(root, "retained-workarea")
	if err := os.MkdirAll(workareaPath, 0o750); err != nil {
		t.Fatalf("create workarea: %v", err)
	}
	clock := time.Date(2026, 7, 20, 16, 0, 0, 123_000_000, time.UTC)
	leaseDir := filepath.Join(root, "terminal-leases")
	quarantineDir := filepath.Join(root, "terminal-quarantine")
	receiverDir := filepath.Join(root, "terminal-receivers")
	storeOptions := workarea.StoreOptions{
		Dir:           leaseDir,
		QuarantineDir: quarantineDir,
		ReceiverDir:   receiverDir,
		Now:           func() time.Time { return clock },
	}
	store, err := workarea.NewLeaseStore(storeOptions)
	if err != nil {
		t.Fatalf("open terminal lease store: %v", err)
	}
	if err := store.RegisterReceiver(testReceiverKey, "https://receiver.invalid"+terminalStatusRoute); err != nil {
		t.Fatalf("register initial receiver: %v", err)
	}

	lease, err := store.Acquire(ctx, workarea.AcquireSpec{
		SessionID:          testSessionID,
		TerminalResultID:   testTerminalResult,
		WorkareaID:         testWorkareaID,
		WorkareaPath:       workareaPath,
		Policy:             workarea.DefaultLeasePolicy(),
		ReleaseRequested:   true,
		ReleaseDisposition: "destroy",
		ReleaseMetadata:    map[string]string{"source": "released-consumer"},
	})
	if err != nil {
		t.Fatalf("reserve terminal workarea: %v", err)
	}
	assertRetainedPath(t, store, workareaPath, true)
	if lease.State != workarea.LeaseActive || !lease.RetainsWorkarea() {
		t.Fatalf("new lease state = %s, retains=%v", lease.State, lease.RetainsWorkarea())
	}

	claimResult, err := store.ClaimExecution(ctx, workarea.ExecutionClaimSpec{
		LeaseID:          lease.LeaseID,
		SessionID:        testSessionID,
		TerminalResultID: testTerminalResult,
		WorkareaID:       testWorkareaID,
		InvocationID:     testInvocationID,
		ClaimID:          testClaimID,
	})
	if err != nil {
		t.Fatalf("claim terminal lease: %v", err)
	}
	if claimResult.Claim.LeaseID != lease.LeaseID || claimResult.Lease.ExecutionClaim == nil {
		t.Fatalf("durable execution claim mismatch: %+v", claimResult)
	}

	poster, err := result.NewPoster(result.Options{
		PlatformURL: "https://receiver.invalid",
		AuthToken:   staleAuthorization,
		WorkerID:    "worker-stable",
		ReceiverKey: testReceiverKey,
		BaseDelay:   0,
	})
	if err != nil {
		t.Fatalf("construct released result poster: %v", err)
	}
	projection := lease.Descriptor().Projection()
	projectionBytes, err := workarea.CanonicalBytes(projection)
	if err != nil {
		t.Fatalf("canonical lease projection: %v", err)
	}
	expectedProjection := fmt.Sprintf(
		`{"leaseId":%q,"workareaId":%q,"terminalResultId":%q,"expiresAt":%q}`,
		lease.LeaseID,
		lease.WorkareaID,
		lease.TerminalResultID,
		lease.ExpiresAt.Format("2006-01-02T15:04:05.000Z"),
	)
	if string(projectionBytes) != expectedProjection {
		t.Fatalf("canonical projection = %s, want %s", projectionBytes, expectedProjection)
	}

	statusBody, err := poster.PrepareTerminalStatusBody(ctx, testSessionID, agent.Result{
		Status:       "completed",
		WorktreePath: workareaPath,
		Summary:      "released consumer acknowledgement",
		WorkResult:   "passed",
		CommitSHA:    "0123456789abcdef0123456789abcdef01234567",
	}, &projection)
	if err != nil {
		t.Fatalf("prepare terminal status body: %v", err)
	}
	if bytes.Contains(statusBody, []byte(workareaPath)) || bytes.Contains(statusBody, []byte("worktreePath")) || bytes.Contains(statusBody, []byte("workareaPath")) {
		t.Fatalf("terminal status leaked a host-local path: %s", statusBody)
	}
	projectionField := append([]byte(`"terminalWorkareaLease":`), projectionBytes...)
	if !bytes.Contains(statusBody, projectionField) {
		t.Fatalf("status body lacks canonical projection %s: %s", projectionBytes, statusBody)
	}

	persisted, err := store.SaveTerminalStatus(ctx, workarea.TerminalStatusSaveSpec{
		LeaseID:           lease.LeaseID,
		SessionID:         testSessionID,
		TerminalResultID:  testTerminalResult,
		WorkareaID:        testWorkareaID,
		ReceiverKey:       testReceiverKey,
		Body:              statusBody,
		ExpectedExpiresAt: lease.ExpiresAt,
		DeadlineAt:        lease.ExpiresAt,
	})
	if err != nil {
		t.Fatalf("persist immutable terminal status: %v", err)
	}
	persistedBody, err := persisted.TerminalStatus.Body()
	if err != nil {
		t.Fatalf("read persisted terminal status: %v", err)
	}
	if !bytes.Equal(persistedBody, statusBody) {
		t.Fatalf("persisted terminal status changed:\n got %s\nwant %s", persistedBody, statusBody)
	}
	mutatedBody := append(append([]byte(nil), statusBody...), ' ')
	if _, err := store.SaveTerminalStatus(ctx, workarea.TerminalStatusSaveSpec{
		LeaseID:           lease.LeaseID,
		SessionID:         testSessionID,
		TerminalResultID:  testTerminalResult,
		WorkareaID:        testWorkareaID,
		ReceiverKey:       testReceiverKey,
		Body:              mutatedBody,
		ExpectedExpiresAt: lease.ExpiresAt,
		DeadlineAt:        lease.ExpiresAt,
	}); !errors.Is(err, workarea.ErrLeaseConflict) {
		t.Fatalf("mutated outbox body error = %v, want ErrLeaseConflict", err)
	}
	assertTreeOmits(t, root, staleAuthorization, freshAuthorization)

	restarted, err := workarea.NewLeaseStore(storeOptions)
	if err != nil {
		t.Fatalf("restart terminal lease store: %v", err)
	}
	assertRetainedPath(t, restarted, workareaPath, true)
	capture := &fakeTerminalReceiver{}
	receiver := httptest.NewServer(capture)
	t.Cleanup(receiver.Close)
	if err := restarted.RegisterReceiver(testReceiverKey, receiver.URL+terminalStatusRoute); err != nil {
		t.Fatalf("rotate receiver endpoint after restart: %v", err)
	}
	capture.configure(restarted, workareaPath, statusBody, freshAuthorization)
	var authorizationCalls atomic.Int64
	sender := restarted.TerminalStatusHTTPSender(receiver.Client(), func(_ context.Context, receiverKey string) (string, error) {
		authorizationCalls.Add(1)
		if receiverKey != testReceiverKey {
			return "", fmt.Errorf("unexpected receiver key %q", receiverKey)
		}
		return freshAuthorization, nil
	})
	considered, err := restarted.ReplayTerminalResults(ctx, 1, time.Second, sender)
	if err != nil {
		t.Fatalf("replay terminal status after restart: %v", err)
	}
	if considered != 1 || authorizationCalls.Load() != 1 {
		t.Fatalf("replay considered=%d authorization calls=%d", considered, authorizationCalls.Load())
	}
	requests, replayedBody, replayedAuthorization, receiverErr := capture.snapshot()
	if receiverErr != nil {
		t.Fatalf("fake receiver rejected replay: %v", receiverErr)
	}
	if requests != 1 || !bytes.Equal(replayedBody, statusBody) || replayedAuthorization != freshAuthorization {
		t.Fatalf("fake receiver requests=%d auth=%q body=%s", requests, replayedAuthorization, replayedBody)
	}
	assertTreeOmits(t, root, staleAuthorization, freshAuthorization)

	ack := workarea.TerminalResultAcknowledgement{
		SchemaVersion:    workarea.TerminalLeaseAcknowledgementSchemaV1,
		Acknowledged:     true,
		InvocationID:     testInvocationID,
		ClaimID:          testClaimID,
		LeaseID:          lease.LeaseID,
		SessionID:        testSessionID,
		TerminalResultID: testTerminalResult,
		WorkareaID:       testWorkareaID,
	}
	nonSemantic := ack
	nonSemantic.Acknowledged = false
	if _, err := restarted.Acknowledge(ctx, nonSemantic); !errors.Is(err, workarea.ErrAcknowledgementRequired) {
		t.Fatalf("non-semantic acknowledgement error = %v", err)
	}
	assertRetainedPath(t, restarted, workareaPath, true)

	wrong := ack
	wrong.ClaimID = testWrongClaimID
	wrongOutcome, err := restarted.Acknowledge(ctx, wrong)
	if err != nil {
		t.Fatalf("apply wrong acknowledgement: %v", err)
	}
	if wrongOutcome.Outcome != workarea.AcknowledgementRejected || wrongOutcome.Reason == nil || *wrongOutcome.Reason != workarea.AcknowledgementIdentityMismatch || wrongOutcome.LeaseState != workarea.LeaseActive {
		t.Fatalf("wrong acknowledgement outcome = %+v", wrongOutcome)
	}
	wrongPersisted, err := restarted.Get(lease.LeaseID)
	if err != nil {
		t.Fatalf("read lease after wrong acknowledgement: %v", err)
	}
	if wrongPersisted.State != workarea.LeaseActive || len(wrongPersisted.AcknowledgementBytes) != 0 || wrongPersisted.TerminalStatus.ApplicationState != workarea.TerminalApplicationRejected {
		t.Fatalf("wrong acknowledgement released or mutated authority: %+v", wrongPersisted)
	}
	assertRetainedPath(t, restarted, workareaPath, true)

	applied, err := restarted.Acknowledge(ctx, ack)
	if err != nil {
		t.Fatalf("apply exact acknowledgement: %v", err)
	}
	if applied.Outcome != workarea.AcknowledgementApplied || applied.Reason != nil || applied.LeaseState != workarea.LeaseReleasePending || applied.ProviderReleaseComplete {
		t.Fatalf("exact acknowledgement outcome = %+v", applied)
	}
	canonicalAck, err := workarea.CanonicalBytes(ack)
	if err != nil {
		t.Fatalf("canonical acknowledgement: %v", err)
	}
	acknowledgedLease, err := restarted.Get(lease.LeaseID)
	if err != nil {
		t.Fatalf("read acknowledged lease: %v", err)
	}
	if acknowledgedLease.State != workarea.LeaseReleasePending || !bytes.Equal(acknowledgedLease.AcknowledgementBytes, canonicalAck) || acknowledgedLease.ReleaseReason != "acknowledgement" || acknowledgedLease.TerminalStatus.ApplicationState != workarea.TerminalApplicationApplied {
		t.Fatalf("acknowledged lease mismatch: %+v", acknowledgedLease)
	}
	assertRetainedPath(t, restarted, workareaPath, true)

	replayedAck, err := restarted.Acknowledge(ctx, ack)
	if err != nil {
		t.Fatalf("replay exact acknowledgement: %v", err)
	}
	if replayedAck.Outcome != workarea.AcknowledgementAlreadyApplied || replayedAck.LeaseState != workarea.LeaseReleasePending {
		t.Fatalf("exact acknowledgement replay outcome = %+v", replayedAck)
	}

	releaseUnavailable := errors.New("provider release temporarily unavailable")
	var releaseAttempts atomic.Int64
	releaser := func(_ context.Context, candidate workarea.TerminalLease) error {
		attempt := releaseAttempts.Add(1)
		if candidate.LeaseID != lease.LeaseID || candidate.WorkareaPath != workareaPath || candidate.ReleaseDisposition != "destroy" {
			return fmt.Errorf("provider received wrong release candidate: %+v", candidate)
		}
		if attempt == 1 {
			return releaseUnavailable
		}
		return nil
	}
	considered, err = restarted.ReapSnapshot(ctx, workarea.SchedulerOptions{
		BatchSize: 1, Concurrency: 1, AttemptTimeout: time.Second,
	}, releaser)
	if considered != 1 || !errors.Is(err, releaseUnavailable) {
		t.Fatalf("first reaper pass considered=%d err=%v", considered, err)
	}
	pending, err := restarted.Get(lease.LeaseID)
	if err != nil {
		t.Fatalf("read release-pending lease: %v", err)
	}
	if pending.State != workarea.LeaseReleasePending || pending.ReleaseAttempts != 1 || pending.NextReleaseAttempt == nil || pending.LastReleaseError == "" {
		t.Fatalf("failed release did not remain pending: %+v", pending)
	}
	assertRetainedPath(t, restarted, workareaPath, true)

	clock = clock.Add(workarea.DefaultReleaseRetryBase + time.Millisecond)
	considered, err = restarted.ReapSnapshot(ctx, workarea.SchedulerOptions{
		BatchSize: 1, Concurrency: 1, AttemptTimeout: time.Second,
	}, releaser)
	if err != nil || considered != 1 {
		t.Fatalf("second reaper pass considered=%d err=%v", considered, err)
	}
	released, err := restarted.Get(lease.LeaseID)
	if err != nil {
		t.Fatalf("read released lease: %v", err)
	}
	if released.State != workarea.LeaseReleased || released.ReleaseAttempts != 2 || released.ReleasedAt == nil || releaseAttempts.Load() != 2 {
		t.Fatalf("at-least-once provider release mismatch: %+v calls=%d", released, releaseAttempts.Load())
	}
	assertRetainedPath(t, restarted, workareaPath, false)
}

func TestReleasedQuarantineCleanup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	quarantinedPath := filepath.Join(root, "quarantined-workarea")
	if err := os.MkdirAll(quarantinedPath, 0o750); err != nil {
		t.Fatalf("create quarantined workarea: %v", err)
	}
	clock := time.Date(2026, 7, 20, 17, 0, 0, 456_000_000, time.UTC)
	pathDigest := sha256.Sum256([]byte(filepath.Clean(quarantinedPath)))
	quarantine := workarea.TerminalWorkareaQuarantine{
		SchemaVersion:    workarea.TerminalWorkareaQuarantineSchemaV1,
		QuarantineID:     testQuarantineID,
		WorkareaID:       testWorkareaID,
		SessionID:        testSessionID,
		TerminalResultID: testTerminalResult,
		WorkareaPath:     filepath.Clean(quarantinedPath),
		PathSHA256:       hex.EncodeToString(pathDigest[:]),
		Reason:           "lease-acquisition-failed",
		State:            workarea.QuarantineQuarantined,
		CreatedAt:        clock,
		UpdatedAt:        clock,
	}
	canonical, err := workarea.CanonicalBytes(quarantine)
	if err != nil {
		t.Fatalf("canonical quarantine: %v", err)
	}
	quarantineDir := filepath.Join(root, "terminal-quarantine")
	recordsDir := filepath.Join(quarantineDir, "records")
	if err := os.MkdirAll(recordsDir, 0o750); err != nil {
		t.Fatalf("create quarantine authority: %v", err)
	}
	if err := os.WriteFile(filepath.Join(recordsDir, testQuarantineID+".json"), canonical, 0o600); err != nil {
		t.Fatalf("seed quarantine record: %v", err)
	}

	store, err := workarea.NewLeaseStore(workarea.StoreOptions{
		Dir:           filepath.Join(root, "terminal-leases"),
		QuarantineDir: quarantineDir,
		ReceiverDir:   filepath.Join(root, "terminal-receivers"),
		Now:           func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("restart with quarantine authority: %v", err)
	}
	assertRetainedPath(t, store, quarantinedPath, true)
	items, err := store.Quarantines()
	if err != nil || len(items) != 1 || items[0].QuarantineID != testQuarantineID {
		t.Fatalf("loaded quarantines = %+v, err=%v", items, err)
	}

	var destroyCalls atomic.Int64
	considered, err := store.CleanupQuarantines(ctx, workarea.SchedulerOptions{
		BatchSize: 1, Concurrency: 1, AttemptTimeout: time.Second,
	}, func(_ context.Context, item workarea.TerminalWorkareaQuarantine) error {
		destroyCalls.Add(1)
		if item.QuarantineID != testQuarantineID || item.WorkareaPath != quarantinedPath {
			return fmt.Errorf("wrong quarantine cleanup item: %+v", item)
		}
		return os.RemoveAll(item.WorkareaPath)
	})
	if err != nil || considered != 1 || destroyCalls.Load() != 1 {
		t.Fatalf("quarantine cleanup considered=%d calls=%d err=%v", considered, destroyCalls.Load(), err)
	}
	items, err = store.Quarantines()
	if err != nil || len(items) != 0 {
		t.Fatalf("quarantine authority not cleared: %+v err=%v", items, err)
	}
	assertRetainedPath(t, store, quarantinedPath, false)
	if _, err := os.Stat(quarantinedPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("quarantined workarea was not destroyed: %v", err)
	}
}

type fakeTerminalReceiver struct {
	mu                    sync.Mutex
	store                 *workarea.LeaseStore
	retainedPath          string
	expectedBody          []byte
	expectedAuthorization string
	requests              int
	body                  []byte
	authorization         string
	err                   error
}

func (f *fakeTerminalReceiver) configure(store *workarea.LeaseStore, retainedPath string, expectedBody []byte, expectedAuthorization string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store = store
	f.retainedPath = retainedPath
	f.expectedBody = append([]byte(nil), expectedBody...)
	f.expectedAuthorization = expectedAuthorization
}

func (f *fakeTerminalReceiver) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	body, readErr := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	f.mu.Lock()
	store := f.store
	retainedPath := f.retainedPath
	expectedBody := append([]byte(nil), f.expectedBody...)
	expectedAuthorization := f.expectedAuthorization
	f.mu.Unlock()

	var receiverErr error
	switch {
	case readErr != nil:
		receiverErr = fmt.Errorf("read terminal status: %w", readErr)
	case request.Method != http.MethodPost:
		receiverErr = fmt.Errorf("method = %s", request.Method)
	case request.URL.Path != terminalStatusRoute:
		receiverErr = fmt.Errorf("path = %s", request.URL.Path)
	case !bytes.Equal(body, expectedBody):
		receiverErr = errors.New("replayed body changed")
	case request.Header.Get("Authorization") != expectedAuthorization:
		receiverErr = fmt.Errorf("authorization = %q", request.Header.Get("Authorization"))
	case store == nil:
		receiverErr = errors.New("lease store unavailable at receiver")
	default:
		retained, err := store.RetainedPath(retainedPath)
		if err != nil {
			receiverErr = fmt.Errorf("check retained path: %w", err)
		} else if !retained {
			receiverErr = errors.New("status arrived before durable reservation")
		}
	}

	f.mu.Lock()
	f.requests++
	f.body = append([]byte(nil), body...)
	f.authorization = request.Header.Get("Authorization")
	if f.err == nil {
		f.err = receiverErr
	}
	f.mu.Unlock()
	if receiverErr != nil {
		http.Error(response, receiverErr.Error(), http.StatusConflict)
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

func (f *fakeTerminalReceiver) snapshot() (int, []byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, append([]byte(nil), f.body...), f.authorization, f.err
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Dir(filepath.Dir(file))
}

func downloadReleaseFile(t *testing.T, name string, maximumBytes int64) []byte {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Minute}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, releaseBaseURL+name, nil)
	if err != nil {
		t.Fatalf("build release asset request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("download immutable release asset %s: %v", name, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("download immutable release asset %s: HTTP %d", name, response.StatusCode)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	if err != nil {
		t.Fatalf("read immutable release asset %s: %v", name, err)
	}
	if int64(len(contents)) > maximumBytes {
		t.Fatalf("immutable release asset %s exceeds %d bytes", name, maximumBytes)
	}
	return contents
}

func extractDonmaiBinary(t *testing.T, archive []byte) string {
	t.Helper()
	compressed, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("open released binary archive: %v", err)
	}
	defer func() { _ = compressed.Close() }()
	reader := tar.NewReader(compressed)
	binaryPath := filepath.Join(t.TempDir(), "donmai")
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read released binary archive: %v", err)
		}
		if path.Clean(header.Name) != "donmai" {
			continue
		}
		if found || !header.FileInfo().Mode().IsRegular() || header.Size <= 0 || header.Size > 128<<20 {
			t.Fatalf("invalid donmai archive entry: %+v", header)
		}
		binary, err := os.OpenFile(binaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatalf("create released binary: %v", err)
		}
		_, copyErr := io.CopyN(binary, reader, header.Size)
		closeErr := binary.Close()
		if copyErr != nil {
			t.Fatalf("extract released binary: %v", copyErr)
		}
		if closeErr != nil {
			t.Fatalf("close released binary: %v", closeErr)
		}
		found = true
	}
	if !found {
		t.Fatal("released binary archive lacks donmai")
	}
	return binaryPath
}

func assertRetainedPath(t *testing.T, store *workarea.LeaseStore, retainedPath string, want bool) {
	t.Helper()
	got, err := store.RetainedPath(retainedPath)
	if err != nil {
		t.Fatalf("check retained path %s: %v", retainedPath, err)
	}
	if got != want {
		t.Fatalf("retained path %s = %v, want %v", retainedPath, got, want)
	}
}

func assertTreeOmits(t *testing.T, root string, forbidden ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		for _, value := range forbidden {
			if bytes.Contains(contents, []byte(value)) {
				return fmt.Errorf("%s persisted forbidden ephemeral authorization %q", filePath, value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
