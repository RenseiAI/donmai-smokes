# donmai-smokes

OSS-canonical smoke harness for the **Donmai `donmai` binary** — the local daemon, the runner, the eight Provider Families, and the workflow engine that ship from [`donmai`](https://github.com/RenseiAI/donmai).

This repo exercises every code path the OSS layer ships standalone: build the `donmai` binary from a sibling checkout (or the released cask), spawn the daemon, drive `donmai daemon …`, `donmai provider …`, `donmai kit …`, `donmai workarea …`, `donmai routing …` against the live local-only HTTP control API, and assert behaviour without depending on the Rensei platform at all.

## What lives here vs. its sibling

This repository is the **OSS-public canonical smoke harness**. Smokes that depend on platform endpoints, WorkOS auth, or Linear/GitHub round-trips through the platform live in a separate internal harness. Where a smoke can run against the OSS-only `donmai` binary on a single machine with no platform involvement, it lives here. Where a smoke depends on platform endpoints (`/api/cli/*`), `rsk_*` tokens, WorkOS auth, or Linear/GitHub round-trips through the platform, it lives in that separate internal harness.

The boundary is the same one declared in [`donmai-architecture`](https://github.com/RenseiAI/donmai-architecture)'s `001-layered-execution-model.md` § "The donmai ↔ Rensei Platform contract":

> 1. The OSS layer defines all interfaces in this corpus.
> 2. The OSS layer ships a working implementation of every interface — never *only* the type.
> 3. The SaaS control plane extends with alternate implementations and centralized administration.
> 4. The OSS layer never depends on the SaaS plane to function. Removing the platform leaves a usable single-machine product.
> 5. The boundary between them is a small set of pluggable function callbacks (`setAgentLauncher`-shaped), not subprocess or RPC.

That boundary discipline — particularly point (4), "removing the platform leaves a usable single-machine product" — determines what ships here. If a proposed smoke needs the SaaS control plane to assert anything, it belongs in the internal platform harness, not here.

## Scope

Concretely, `donmai-smokes` will exercise:

- `donmai` binary presence + signing (macOS Developer-ID + hardened runtime check on the released binary; skipped for `--build-local`).
- `donmai daemon install / status / drain / stop / uninstall` end-to-end against a real local daemon (`launchd` on macOS, `systemd` on Linux).
- `donmai daemon`'s HTTP control API (`/api/daemon/*`, locked in [`ADR-2026-05-07-daemon-http-control-api.md`](https://github.com/RenseiAI/donmai-architecture)) — the four migrated command surfaces (`provider`, `kit`, `workarea`, `routing`) hit the daemon, not a platform stub.
- `donmai --help` deprecation guard — pins that the Wave 9 surface (`provider` / `kit` / `workarea` / `routing`) renders correctly on every commit, mirroring the internal platform harness's `TestRenseiHelpMirrorsAfForMigratedSurfaces` from the `donmai` side.
- (Deferred candidate) `donmai agent run` against a queued work item using the `donmai`-resident provider stack.

What does **not** live here: WorkOS auth, `rsk_*` tokens, Linear/GitHub orchestrator loops, on-demand Vercel pools, multi-machine fleet aggregation, org-activation flows, or anything else that requires reaching out to platform-resident endpoints. Those all stay in the internal platform harness.

## Status

**Wave 10 Phase 10 — first three donmai-only smokes landed.** The harness package (Phase 9) and the donmai-only smoke tests are in place.

## Tests

The smoke tests build the `donmai` binary from the sibling `donmai` worktree, spawn a foreground `donmai daemon run` on a free port with isolated `HOME`, and exercise it.

| Test file | What it pins |
|---|---|
| [`step1_af_daemon_lifecycle_test.go`](step1_af_daemon_lifecycle_test.go) | `TestAfDaemonLifecycle` — build → spawn → poll `/healthz` → exercise `donmai daemon status` + `donmai daemon stats` → `SIGTERM` → assert graceful exit + bind-port release. Foreground spawn only; service-unit install is deferred. |
| [`step2_af_daemon_command_surface_test.go`](step2_af_daemon_command_surface_test.go) | `TestAfDaemonCommandSurface` — exercises the four migrated command surfaces (`donmai provider list`, `donmai kit list`, `donmai workarea list`, `donmai routing show --plain`) against the live `/api/daemon/*` HTTP control API. Mirrors the internal platform harness's `TestRenseiHostDaemonCommandSurface` from the donmai side. |
| [`step3_af_help_deprecation_guard_test.go`](step3_af_help_deprecation_guard_test.go) | `TestAfHelpDeprecationGuard` — pins the Wave 9 verb shape on the donmai binary itself (top-level surface includes `provider`/`kit`/`workarea`/`routing`; per-surface subcommand sets match the v0.7.0 baseline). Self-pin counterpart to the internal platform harness's cross-binary mirror test. |

Both `step1` and `step2` share build-and-spawn setup via [`setup_live_daemon_test.go`](setup_live_daemon_test.go).

### Running

```sh
make test                                           # GOWORK=off go test -race ./...
GOWORK=off go test -race ./... -timeout 8m          # explicit equivalent
GOWORK=off go test -short ./...                     # skip live-daemon tests
DONMAI_SMOKES_SKIP_LIVE_DAEMON=1 go test ./...      # opt out of live-daemon without -short
DONMAI_SMOKES_SKIP_INSTALLER=1 go test ./...        # opt out of install lifecycle smoke
DONMAI_SMOKES_SKIP_LIVE_API=1 go test ./...         # opt out of live external-API smokes
make lint                                           # golangci-lint run ./...
```

The first build of `donmai` takes 60-90s on a cold cache; warm runs are sub-second. All tests `t.Skip` cleanly when the donmai sibling worktree or Go toolchain isn't available, so the harness can run standalone for CI flag-parsing checks.

## Conventions

- **Module**: `github.com/RenseiAI/donmai-smokes`. Public, OSS-licensed.
- **Module dep**: tracks `github.com/RenseiAI/donmai` at v0.7.0 minimum (the Wave 9 release that locked the four daemon-targeted command surfaces and the `/api/daemon/*` HTTP control API).
- **Go version**: 1.25.9 (matches `donmai`).
- **Testing**: stdlib `testing` + table-driven tests. No `testify`. `httptest` for HTTP fixtures. Same testing discipline as `donmai` declares in its `AGENTS.md` § "Conventions".
- **GOWORK behavior**: `GOWORK=off` for the `make test` target. When this harness builds the `donmai` binary from a sibling `donmai` checkout via subprocess, the build subprocess gets `GOWORK=` (cleared) so it resolves `donmai`'s own `go.mod`/`go.sum` rather than the org root's `go.work`. The harness itself runs with `GOWORK=off` so tests don't accidentally pull in the workspace's other modules.
- **Linting**: `golangci-lint` with the same enabled checks as `donmai`.

## Architecture reference

Cross-repo:

- [`donmai-architecture`](https://github.com/RenseiAI/donmai-architecture) — canonical OSS architecture corpus. Read `001-layered-execution-model.md` for the boundary, `011-local-daemon-fleet.md` for the daemon operations model, and `ADR-2026-05-07-daemon-http-control-api.md` for the `/api/daemon/*` endpoint surface this harness exercises.
- [`donmai`](https://github.com/RenseiAI/donmai) — the `donmai` binary, the `afclient`/`afcli`/`worker` public packages, and the daemon implementation under test.
- Internal platform harness — sibling platform-specific smoke harness; mirrors this one for the `rensei` binary + Rensei platform. (Not publicly available.)

## How this corpus changes

Permissive-direct-to-main norms; both humans and fleet agents may commit directly. Substantive changes (new smoke tests that materially shift coverage, harness primitive renames that affect the internal platform harness's import surface) follow the same ADR pattern declared in `donmai-architecture/AGENTS.md` § "How to disagree with this doc" — open an ADR in the architecture repo, declare its `boundary:` in frontmatter, and update affected smoke files in the same commit that flips the ADR to Accepted.

Non-substantive edits (typos, comment fixes, `.gitignore` adjustments, broken-link repairs) commit directly without ceremony.
