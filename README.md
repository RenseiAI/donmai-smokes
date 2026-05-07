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

**Wave 10 Phase 7 scaffolding.** Repo just stood up. The harness package + first smoke tests land in subsequent phases:

- **Phase 9** — extracts the OSS-pure harness primitives from `rensei-smokes` (the `daemon_detect.go` probe, `harness.go` build-binary + spawn-daemon helpers, `cleanup.go` subprocess teardown, `uid.go` unique-id helpers) into a new public Go package, e.g., `github.com/RenseiAI/agentfactory-smokes/harness`. After Phase 9 lands, `rensei-smokes` deletes its local copies and imports from this repo — same boundary discipline as `rensei-tui` → `agentfactory-tui`.
- **Phase 10** — ports the `af`-only smoke tests into this repo: `TestAfDaemonLifecycle`, `TestAfDaemonCommandSurface`, `TestAfHelpDeprecationGuard`, and the deferred `TestAfAgentRunSmoke` candidate. See `runs/WAVE10_PLAN.md` § "Track Smokes" for the full migration list.

**Reading order is TBD.** Once Phase 9 lands the harness package and Phase 10 lands the af-only smoke tests, this README's reading order will point at the relevant test files and `harness/` subpackages. Until then, treat `rensei-smokes`'s `README.md` as the operational reference for harness shape and step layout, noting that the platform-coupled steps (1's `rensei`/codesign vs `af`/codesign rename, 2's WorkOS/`rsk_` auth, 3-10's platform smokes) all belong on the platform side.

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
