# agentfactory-smokes

OSS-canonical smoke harness for the **AgentFactory `af` binary**.

**Module**: `github.com/RenseiAI/agentfactory-smokes`

## Purpose

This repo exercises every code path the OSS layer ships standalone: build the `af` binary from a sibling `agentfactory-tui` checkout (or the released cask), spawn the local daemon, drive the `af daemon` lifecycle and the four daemon-targeted command surfaces (`provider`, `kit`, `workarea`, `routing`) against the live `/api/daemon/*` HTTP control API, and assert behaviour without depending on the Rensei SaaS control plane at all.

The harness is built so a forked OSS deployment of `agentfactory-tui` can run it against its own daemon with no Rensei-tenant assumptions baked in. If a smoke here ever requires the SaaS plane to assert anything, the boundary has been violated and the smoke needs to relocate to [`rensei-smokes`](https://github.com/RenseiAI/rensei-smokes).

## Boundary

This repo is the **OSS-public canonical smoke harness**. Its sibling [`rensei-smokes`](https://github.com/RenseiAI/rensei-smokes) is the **Rensei-platform smoke harness** — it covers WorkOS password-grant auth, `rsk_*` token issuance, the Linear → orchestrator → PR → merge full-loop, on-demand Vercel sandbox provisioning, org-activation chains, and every other smoke that requires the Rensei SaaS plane to function.

The boundary discipline — verbatim from [`agentfactory-architecture/001-layered-execution-model.md`](https://github.com/RenseiAI/agentfactory-architecture) § "The agentfactory ↔ Rensei Platform contract":

> 1. The OSS layer defines all interfaces in this corpus.
> 2. The OSS layer ships a working implementation of every interface — never *only* the type.
> 3. The SaaS control plane extends with alternate implementations and centralized administration (registries, signing, policy enforcement, multi-tenant management, the SaaS dashboard, the routing-intelligence panel).
> 4. The OSS layer never depends on the SaaS plane to function. Removing the platform leaves a usable single-machine product.
> 5. The boundary between them is a small set of pluggable function callbacks (`setAgentLauncher`-shaped), not subprocess or RPC. The platform composes the OSS layer as a library; both ship as one binary to end users.

**Operational implication for agents working in this repo:** never let Rensei-platform-coupled content land here. Concretely:

- **No WorkOS auth.** No `WORKOS_TEST_EMAIL` / `WORKOS_TEST_PASSWORD` / `WORKOS_API_KEY` env-var reads, no calls to `https://api.workos.com/user_management/authenticate`, no platform-config-shaped JSON injection (`~/.config/rensei/config.json` carrying `active_auth`). `af` is OSS; OSS auth — when it ships — does not transit WorkOS.
- **No Linear orchestration.** No direct calls to `https://api.linear.app/graphql`, no `LINEAR_API_KEY` reads, no `issueCreate` / `issueArchive` mutations. Linear-coupled smokes belong in `rensei-smokes`.
- **No Rensei platform endpoints.** No `https://platform.rensei.dev` / `127.0.0.1:3010` calls, no `/api/cli/*` references (that's the platform's CLI-auth namespace), no `/api/workers/register` and friends. Daemon endpoints (`/api/daemon/*` on `127.0.0.1:7734`) are OSS-shipped and fair game; CLI/platform endpoints are not.
- **No `rsk_*` tokens.** No `RENSEI_TEST_TOKEN` reads, no token-injection helpers, no scope-string assertions (`worker:register`, `worker:poll`, `worker:heartbeat`, `worker:session`). Tokens are platform-issued credentials; the OSS daemon does not need them to function on a single machine.
- **No GitHub orchestration.** No `GITHUB_TOKEN` reads against the public GitHub API for PR / merge / branch-delete operations. (The `gh` CLI as a build dep, e.g., for `gh repo create` if the harness ever needs to scaffold a fixture repo, is fine.)

If a proposed smoke needs any of the above to assert anything, the right move is to land it in `rensei-smokes` instead. If the smoke is genuinely OSS-shippable but reaches into one of these platform corners *only* to cross-check something the OSS layer can detect another way, refactor to use the OSS detection.

## Architecture corpus

Canonical architecture lives in [`agentfactory-architecture`](https://github.com/RenseiAI/agentfactory-architecture). Read in this order when working on this repo:

1. [`001-layered-execution-model.md`](https://github.com/RenseiAI/agentfactory-architecture) — the boundary itself, the eight Provider Families, the layered-execution model.
2. [`011-local-daemon-fleet.md`](https://github.com/RenseiAI/agentfactory-architecture) — the daemon operations model: install paths, first-run setup, config knobs, drain semantics, recovery, the HTTP control API.
3. [`ADR-2026-05-07-daemon-http-control-api.md`](https://github.com/RenseiAI/agentfactory-architecture) — the canonical `/api/daemon/*` endpoint surface. This is the contract the smoke harness asserts against.

If this repo's docs conflict with `agentfactory-architecture`, the corpus wins. Either update this repo's docs to align, or open an ADR to amend the corpus.

## Conventions

- **Testing**: stdlib `testing` + table-driven tests. No `testify`. `httptest` for HTTP fixtures and mock daemon shapes. Match the testing discipline declared in [`agentfactory-tui/AGENTS.md`](https://github.com/RenseiAI/agentfactory-tui) § "Conventions" (80% coverage target, 70% minimum).
- **Errors**: `fmt.Errorf("context: %w", err)` for wrapping. Sentinel errors in a `harness/errors.go` once the harness package lands. Never panic. Never `log.Fatal`.
- **Logging**: `log/slog` to stderr. Smoke output goes through the harness's verbose-vs-quiet toggle so CI logs stay readable.
- **Linting**: `golangci-lint` with the same checks `agentfactory-tui` declares (govet, staticcheck, gofumpt, errcheck, gosec, gocritic, revive). `make lint` runs them.
- **GOWORK behavior**: `make test` runs with `GOWORK=off` to keep this harness's module resolution decoupled from any sibling `go.work` at the org root. When the harness builds the `af` binary from a sibling `agentfactory-tui` checkout via subprocess, the build subprocess gets `GOWORK=` (cleared) so it resolves `agentfactory-tui`'s own `go.mod`/`go.sum` rather than a workspace overlay. This matches the Wave 9 fix in `rensei-smokes` (commit `a2a4a4b`); see `step11_daemon_command_surface.go:159` and `org_activate_user_auth_test.go:97-104` in that repo for the precedent.
- **Naming**: lowercase single-word packages, PascalCase exports. Test files use `_test.go` suffix; the smoke binary entry-point (if/when one ships post-Phase-10) lives at `cmd/smoke/main.go` mirroring `rensei-smokes`'s top-level `main.go`.

## Read order

**Pending Phase 9 + Phase 10.** The harness package and the af-only smoke tests have not yet migrated. Until they do, treat the relevant files in [`rensei-smokes`](https://github.com/RenseiAI/rensei-smokes) as the operational reference for harness shape:

- `harness.go` — build-binary + spawn-daemon primitives. Phase 9 extracts these to `agentfactory-smokes/harness`.
- `daemon_detect.go` — daemon-runtime probe. Phase 9 extracts.
- `cleanup.go` — subprocess teardown helpers. Phase 9 extracts.
- `uid.go` — unique-id helpers. Phase 9 extracts.
- `step11_daemon_command_surface.go` + `step11_daemon_command_surface_test.go` — the canonical four-surface live-daemon smoke from Wave 9. Phase 10 extracts the af-binary mirror.

A concrete OSS-canonical reading order will land in this AGENTS.md in Phase 10's final commit, replacing this placeholder.

## Status

**Wave 10 Phase 7 — repo stand-up.** Phase 8 (smokes audit) is in flight in parallel; Phase 9 (harness primitive extraction) and Phase 10 (af-only smoke ports) follow. This AGENTS.md will be revised in Phase 10 to point at concrete tests once they land.

## How this corpus changes

Permissive-direct-to-main norms; both humans and fleet agents may commit directly. Substantive changes (new smoke tests that materially shift coverage, harness primitive renames that affect `rensei-smokes`' import surface) follow the ADR pattern declared in `agentfactory-architecture/AGENTS.md` § "How to disagree with this doc" — open an ADR in the architecture repo, declare its `boundary:` in frontmatter, and update affected smoke files in the same commit that flips the ADR to Accepted.

Non-substantive edits (typos, comment fixes, `.gitignore` adjustments, broken-link repairs) commit directly without ceremony.
