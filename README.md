# agentfactory-smokes

OSS-canonical smoke harness for the **AgentFactory `af` binary** — the local daemon, the runner, the eight Provider Families, and the workflow engine that ship from [`agentfactory-tui`](https://github.com/RenseiAI/agentfactory-tui).

This repo exercises every code path the OSS layer ships standalone: build the `af` binary from a sibling checkout (or the released cask), spawn the daemon, drive `af daemon …`, `af provider …`, `af kit …`, `af workarea …`, `af routing …` against the live local-only HTTP control API, and assert behaviour without depending on the Rensei platform at all.

## What lives here vs. its sibling

This repository is the **OSS-public canonical smoke harness**. Its sibling, [`rensei-smokes`](https://github.com/RenseiAI/rensei-smokes), is the **Rensei-platform smoke harness** — it covers the org-activation chain, WorkOS password-grant auth, Linear → orchestrator → PR → merge full-loop, on-demand sandbox provisioning, and every other smoke that requires the Rensei SaaS control plane to function. Where a smoke can run against the OSS-only `af` binary on a single machine with no platform involvement, it lives here. Where a smoke depends on platform endpoints (`/api/cli/*`), `rsk_*` tokens, WorkOS auth, or Linear/GitHub round-trips through the platform, it lives in `rensei-smokes`.

The boundary is the same one declared in [`agentfactory-architecture`](https://github.com/RenseiAI/agentfactory-architecture)'s `001-layered-execution-model.md` § "The agentfactory ↔ Rensei Platform contract":

> 1. The OSS layer defines all interfaces in this corpus.
> 2. The OSS layer ships a working implementation of every interface — never *only* the type.
> 3. The SaaS control plane extends with alternate implementations and centralized administration.
> 4. The OSS layer never depends on the SaaS plane to function. Removing the platform leaves a usable single-machine product.
> 5. The boundary between them is a small set of pluggable function callbacks (`setAgentLauncher`-shaped), not subprocess or RPC.

That boundary discipline — particularly point (4), "removing the platform leaves a usable single-machine product" — determines what ships here. If a proposed smoke needs the SaaS control plane to assert anything, it belongs in `rensei-smokes`, not here.

## Scope

Concretely, `agentfactory-smokes` will exercise:

- `af` binary presence + signing (macOS Developer-ID + hardened runtime check on the released binary; skipped for `--build-local`).
- `af daemon install / status / drain / stop / uninstall` end-to-end against a real local daemon (`launchd` on macOS, `systemd` on Linux).
- `af daemon`'s HTTP control API (`/api/daemon/*`, locked in [`ADR-2026-05-07-daemon-http-control-api.md`](https://github.com/RenseiAI/agentfactory-architecture)) — the four migrated command surfaces (`provider`, `kit`, `workarea`, `routing`) hit the daemon, not a platform stub.
- `af --help` deprecation guard — pins that the Wave 9 surface (`provider` / `kit` / `workarea` / `routing`) renders correctly on every commit, mirroring `rensei-smokes`'s `TestRenseiHelpMirrorsAfForMigratedSurfaces` from the `af` side.
- (Deferred candidate) `af agent run` against a queued work item using the `af`-resident provider stack.

What does **not** live here: WorkOS auth, `rsk_*` tokens, Linear/GitHub orchestrator loops, on-demand Vercel pools, multi-machine fleet aggregation, org-activation flows, or anything else that requires reaching out to platform-resident endpoints. Those all stay in `rensei-smokes`.

## Status

**Wave 10 Phase 10 — first three af-only smokes landed.** The harness package (Phase 9) and the af-only smoke tests are in place. `TestAfAgentRunSmoke` is deferred per the Wave 10 plan and is not implemented in this wave.

## Tests

Three live-daemon smokes ship at the top level of this repo. All three build the `af` binary from the sibling `agentfactory-tui` worktree, spawn a foreground `af daemon run` on a free port with isolated `HOME`, and exercise it.

| Test file | What it pins |
|---|---|
| [`step1_af_daemon_lifecycle_test.go`](step1_af_daemon_lifecycle_test.go) | `TestAfDaemonLifecycle` — build → spawn → poll `/healthz` → exercise `af daemon status` + `af daemon stats` → `SIGTERM` → assert graceful exit + bind-port release. Foreground spawn only; service-unit install is deferred. |
| [`step2_af_daemon_command_surface_test.go`](step2_af_daemon_command_surface_test.go) | `TestAfDaemonCommandSurface` — exercises the four migrated command surfaces (`af provider list`, `af kit list`, `af workarea list`, `af routing show --plain`) against the live `/api/daemon/*` HTTP control API. Mirrors `rensei-smokes`' `TestRenseiHostDaemonCommandSurface` from the af side. |
| [`step3_af_help_deprecation_guard_test.go`](step3_af_help_deprecation_guard_test.go) | `TestAfHelpDeprecationGuard` — pins the Wave 9 verb shape on the af binary itself (top-level surface includes `provider`/`kit`/`workarea`/`routing`; per-surface subcommand sets match the v0.7.0 baseline). Self-pin counterpart to `rensei-smokes`' cross-binary mirror test. |

Both `step1` and `step2` share build-and-spawn setup via [`setup_live_daemon_test.go`](setup_live_daemon_test.go).

### Running

```sh
make test                                       # GOWORK=off go test -race ./...
GOWORK=off go test -race ./... -timeout 8m      # explicit equivalent
GOWORK=off go test -short ./...                 # skip live-daemon tests
RENSEI_SMOKES_SKIP_LIVE_DAEMON=1 go test ./...  # opt out without -short
make lint                                       # golangci-lint run ./...
```

The first build of `af` takes 60-90s on a cold cache; warm runs are sub-second. All three tests `t.Skip` cleanly when the agentfactory-tui sibling worktree or Go toolchain isn't available, so the harness can run standalone for CI flag-parsing checks.

## Conventions

- **Module**: `github.com/RenseiAI/agentfactory-smokes`. Public, OSS-licensed.
- **Module dep**: tracks `github.com/RenseiAI/agentfactory-tui` at v0.7.0 minimum (the Wave 9 release that locked the four daemon-targeted command surfaces and the `/api/daemon/*` HTTP control API).
- **Go version**: 1.25.9 (matches `agentfactory-tui`).
- **Testing**: stdlib `testing` + table-driven tests. No `testify`. `httptest` for HTTP fixtures. Same testing discipline as `agentfactory-tui` declares in its `AGENTS.md` § "Conventions".
- **GOWORK behavior**: `GOWORK=off` for the `make test` target. Matches the Wave 9 fix in `rensei-smokes` (`a2a4a4b`): when this harness builds the `af` binary from a sibling `agentfactory-tui` checkout via subprocess, the build subprocess gets `GOWORK=` (cleared) so it resolves `agentfactory-tui`'s own `go.mod`/`go.sum` rather than the org root's `go.work`. The harness itself runs with `GOWORK=off` so tests don't accidentally pull in the workspace's other modules.
- **Linting**: `golangci-lint` with the same enabled checks as `agentfactory-tui`.

## Architecture reference

Cross-repo:

- [`agentfactory-architecture`](https://github.com/RenseiAI/agentfactory-architecture) — canonical OSS architecture corpus. Read `001-layered-execution-model.md` for the boundary, `011-local-daemon-fleet.md` for the daemon operations model, and `ADR-2026-05-07-daemon-http-control-api.md` for the `/api/daemon/*` endpoint surface this harness exercises.
- [`agentfactory-tui`](https://github.com/RenseiAI/agentfactory-tui) — the `af` binary, the `afclient`/`afcli`/`worker` public packages, and the daemon implementation under test.
- [`rensei-smokes`](https://github.com/RenseiAI/rensei-smokes) — sibling platform-specific smoke harness; mirrors this one for the `rensei` binary + Rensei platform.

## How this corpus changes

Permissive-direct-to-main norms; both humans and fleet agents may commit directly. Substantive changes (new smoke tests that materially shift coverage, harness primitive renames that affect `rensei-smokes`' import surface) follow the same ADR pattern declared in `agentfactory-architecture/AGENTS.md` § "How to disagree with this doc" — open an ADR in the architecture repo, declare its `boundary:` in frontmatter, and update affected smoke files in the same commit that flips the ADR to Accepted.

Non-substantive edits (typos, comment fixes, `.gitignore` adjustments, broken-link repairs) commit directly without ceremony.
