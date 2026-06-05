// Package harness is the OSS-public test harness for the Donmai `donmai`
// binary.
//
// The harness provides reusable primitives for building, spawning, probing,
// and exercising the `donmai` binary and its local daemon process — without
// any dependency on the Rensei SaaS platform. It is consumed by:
//
//   - donmai-smokes itself, which ships donmai-only smoke tests on top of
//     these primitives (Phase 10 onward).
//   - rensei-smokes, which extends the harness with platform-specific smokes
//     (WorkOS auth, Linear orchestration, on-demand sandbox provisioning,
//     etc.) and uses these primitives to drive the OSS portions of its run.
//
// Public API surface (Wave 10 Phase 9):
//
//   - Runner — subprocess executor with dry-run, verbose, timeout, and
//     binary-override support.
//   - Build helpers — compile a fresh `donmai` (or sibling) binary from a
//     workspace checkout for hermetic test runs.
//   - Spawn helpers — boot a `donmai daemon run` child with isolated HOME
//     and poll /healthz until ready.
//   - Daemon-detect helpers — probe whether a daemon runtime is reachable
//     (subcommand vs legacy binary vs absent).
//   - Cleanup aggregation — run a sequence of teardown hooks and aggregate
//     errors without short-circuiting.
//   - String / error / TTY helpers — small primitives extracted from the
//     rensei-smokes step files because they have no platform coupling.
//   - Help-output parsing — parse a Cobra `--help` Available Commands
//     section, used both by rensei-smokes' help-mirror regression guard
//     and by donmai-smokes' own help-deprecation guard.
//
// Stability:
//
// Once donmai-smokes ships its first tag, the exported surface here
// becomes the boundary contract between the OSS smoke layer and
// rensei-smokes. Breaking changes follow the same semver discipline as
// donmai (see Wave 9 push order: v0.7.0 → v0.8.0 only with a
// migration note); additive changes are unrestricted.
//
// See donmai-smokes/AGENTS.md for the boundary discipline that
// determines whether a candidate primitive belongs here or in
// rensei-smokes.
package harness
