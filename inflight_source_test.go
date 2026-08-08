package smokes

// inflight_source_test.go — the one place this suite decides which donmai
// checkout a step builds from.
//
// This replaces five near-identical per-step helpers (archGoNativeSourceDir,
// siblingSourceDir, openCodeSourceDir, gatewaySourceDir, piSourceDir). Each
// read the same env var, then fell back to the literal relative path
// "../donmai" — correct from a primary checkout, wrong from a linked worktree
// (<root>/donmai-smokes.wt/<name>, where the sibling is at ../../donmai), and
// the build failure that produced was string-matched straight into a t.Skip.
// One of the five had already grown a private "../../donmai" patch for
// exactly that reason, which is the tell that the fix belonged in one place.
//
// The fallback is gone. An empty return hands resolution to
// harness.LocateDonmaiSource, which finds the checkout by identity (module
// path + cmd/donmai) from any of the layouts we run in and FAILS loudly when
// it cannot — instead of skipping.
//
// Three of the five helpers also preferred a hard-coded unmerged feature
// worktree (../donmai.wt/oc-matrix-pins, ../donmai.wt/sibling-context-repos,
// ../donmai.wt/af-arch-deprecation) ahead of the canonical checkout. All
// three branches have since merged and the worktrees are gone, so those
// paths were dead — and worse than dead: had one lingered on a developer's
// machine, the smoke would have silently tested stale unmerged code in
// preference to main. DONMAI_ARCH_SOURCE_DIR remains the supported way to
// aim a step at an in-flight port.

import (
	"os"
	"strings"
)

// inFlightSourceDirEnv points a step's build at an in-flight source port
// (e.g. an unmerged donmai worktree) instead of the canonical checkout. It is
// part of the CI-operator contract in AGENTS.md — keep it honored.
const inFlightSourceDirEnv = "DONMAI_ARCH_SOURCE_DIR"

// inFlightSourceDir returns the operator's explicit donmai source override,
// or "" to let the harness locate the canonical checkout.
//
// A set-but-invalid value is NOT silently ignored: harness.RequireDonmaiSourceAt
// fails on it. Naming a path is a statement of intent, and quietly building a
// different commit than the operator asked for is the same class of lie as
// skipping a smoke and reporting ok.
func inFlightSourceDir() string {
	return strings.TrimSpace(os.Getenv(inFlightSourceDirEnv))
}
