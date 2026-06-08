# donmai-smokes

OSS-canonical smoke harness for the **Donmai `donmai` binary**.

**Module**: `github.com/RenseiAI/donmai-smokes`

## Purpose

This repo exercises every code path the OSS layer ships standalone: build the `donmai` binary from a sibling `donmai` checkout (or the released cask), spawn the local daemon, drive the `donmai daemon` lifecycle and the four daemon-targeted command surfaces (`provider`, `kit`, `workarea`, `routing`) against the live `/api/daemon/*` HTTP control API, and assert behaviour without depending on the Rensei SaaS control plane at all.

The harness is built so a forked OSS deployment of `donmai` can run it against its own daemon with no Rensei-tenant assumptions baked in. If a smoke here ever requires the SaaS plane to assert anything, the boundary has been violated and the smoke needs to relocate to the internal platform harness.

## Boundary

This repo is the **OSS-public canonical smoke harness**. Smokes that depend on platform endpoints, WorkOS auth, or Linear/GitHub round-trips through the platform live in a separate internal harness. That harness covers WorkOS password-grant auth, `rsk_*` token issuance, the Linear → orchestrator → PR → merge full-loop, on-demand Vercel sandbox provisioning, org-activation chains, and every other smoke that requires the Rensei SaaS plane to function.

The boundary discipline — verbatim from [`donmai-architecture/001-layered-execution-model.md`](https://github.com/RenseiAI/donmai-architecture) § "The donmai ↔ Rensei Platform contract":

> 1. The OSS layer defines all interfaces in this corpus.
> 2. The OSS layer ships a working implementation of every interface — never *only* the type.
> 3. The SaaS control plane extends with alternate implementations and centralized administration (registries, signing, policy enforcement, multi-tenant management, the SaaS dashboard, the routing-intelligence panel).
> 4. The OSS layer never depends on the SaaS plane to function. Removing the platform leaves a usable single-machine product.
> 5. The boundary between them is a small set of pluggable function callbacks (`setAgentLauncher`-shaped), not subprocess or RPC. The platform composes the OSS layer as a library; both ship as one binary to end users.

**Operational implication for agents working in this repo:** never let Rensei-platform-coupled content land here. Concretely:

- **No WorkOS auth.** No `WORKOS_TEST_EMAIL` / `WORKOS_TEST_PASSWORD` / `WORKOS_API_KEY` env-var reads, no calls to `https://api.workos.com/user_management/authenticate`, no platform-config-shaped JSON injection (`~/.config/rensei/config.json` carrying `active_auth`). `donmai` is OSS; OSS auth — when it ships — does not transit WorkOS.
- **No Linear orchestration.** No direct calls to `https://api.linear.app/graphql`, no `LINEAR_API_KEY` reads, no `issueCreate` / `issueArchive` mutations. Linear-coupled smokes belong in the internal platform harness.
- **No Rensei platform endpoints.** No `https://platform.rensei.dev` / `127.0.0.1:3010` calls, no `/api/cli/*` references (that's the platform's CLI-auth namespace), no `/api/workers/register` and friends. Daemon endpoints (`/api/daemon/*` on `127.0.0.1:7734`) are OSS-shipped and fair game; CLI/platform endpoints are not.
- **No `rsk_*` tokens.** No `RENSEI_TEST_TOKEN` reads, no token-injection helpers, no scope-string assertions (`worker:register`, `worker:poll`, `worker:heartbeat`, `worker:session`). Tokens are platform-issued credentials; the OSS daemon does not need them to function on a single machine.
- **No GitHub orchestration.** No `GITHUB_TOKEN` reads against the public GitHub API for PR / merge / branch-delete operations. (The `gh` CLI as a build dep, e.g., for `gh repo create` if the harness ever needs to scaffold a fixture repo, is fine.)

If a proposed smoke needs any of the above to assert anything, the right move is to land it in the internal platform harness instead. If the smoke is genuinely OSS-shippable but reaches into one of these platform corners *only* to cross-check something the OSS layer can detect another way, refactor to use the OSS detection.

## Architecture corpus

Canonical architecture lives in [`donmai-architecture`](https://github.com/RenseiAI/donmai-architecture). Read in this order when working on this repo:

1. [`001-layered-execution-model.md`](https://github.com/RenseiAI/donmai-architecture) — the boundary itself, the eight Provider Families, the layered-execution model.
2. [`011-local-daemon-fleet.md`](https://github.com/RenseiAI/donmai-architecture) — the daemon operations model: install paths, first-run setup, config knobs, drain semantics, recovery, the HTTP control API.
3. [`ADR-2026-05-07-daemon-http-control-api.md`](https://github.com/RenseiAI/donmai-architecture) — the canonical `/api/daemon/*` endpoint surface. This is the contract the smoke harness asserts against.

If this repo's docs conflict with `donmai-architecture`, the corpus wins. Either update this repo's docs to align, or open an ADR to amend the corpus.

## Conventions

- **Testing**: stdlib `testing` + table-driven tests. No `testify`. `httptest` for HTTP fixtures and mock daemon shapes. Match the testing discipline declared in [`donmai/AGENTS.md`](https://github.com/RenseiAI/donmai) § "Conventions" (80% coverage target, 70% minimum).
- **Errors**: `fmt.Errorf("context: %w", err)` for wrapping. Sentinel errors in a `harness/errors.go` once the harness package lands. Never panic. Never `log.Fatal`.
- **Logging**: `log/slog` to stderr. Smoke output goes through the harness's verbose-vs-quiet toggle so CI logs stay readable.
- **Linting**: `golangci-lint` with the same checks `donmai` declares (govet, staticcheck, gofumpt, errcheck, gosec, gocritic, revive). `make lint` runs them.
- **GOWORK behavior**: `make test` runs with `GOWORK=off` to keep this harness's module resolution decoupled from any sibling `go.work` at the org root. When the harness builds the `donmai` binary from a sibling `donmai` checkout via subprocess, the build subprocess gets `GOWORK=` (cleared) so it resolves `donmai`'s own `go.mod`/`go.sum` rather than a workspace overlay.
- **Naming**: lowercase single-word packages, PascalCase exports. Test files use `_test.go` suffix; the smoke binary entry-point (if/when one ships post-Phase-10) lives at `cmd/smoke/main.go`.
- **Env var skip knobs** (CI-operator contract):
  - `DONMAI_SMOKES_SKIP_LIVE_DAEMON=1` — skip live-daemon smokes (step1–step6 kit lifecycle) without `-short`.
  - `DONMAI_SMOKES_SKIP_INSTALLER=1` — skip install/uninstall lifecycle smoke (step6 install).
  - `DONMAI_SMOKES_SKIP_LIVE_API=1` — skip live external-API smokes (step15 codex/MCP).

## Read order

**Wave 10 Phase 10 — first donmai-only smokes landed.** The harness package (Phase 9) and the donmai-only smoke tests are in place.

- `harness/build.go` — `BuildDonmaiBinary` + `BuildBinary`: build the donmai binary from the sibling `../donmai` checkout.
- `harness/live_daemon.go` — `LiveDaemonWithConfig`: spawn + healthz-wait with optional daemon.yaml pre-write.
- `harness/daemon_detect.go` — `DaemonAvailable`: probe whether the daemon subcommand is reachable.
- `harness/runner.go` — `Runner`: subprocess executor with dry-run, verbose, timeout, and binary-override.
- `harness/help_parser.go` — `ParseHelpSubcommands`: parse Cobra `--help` Available Commands.
- `setup_live_daemon_test.go` — in-package `setupLiveDaemon` wrapper for step1/step2.
- `step1_af_daemon_lifecycle_test.go` — lifecycle smoke (build → spawn → status → stats → graceful shutdown).
- `step2_af_daemon_command_surface_test.go` — four daemon-targeted command surfaces.
- `step3_af_help_deprecation_guard_test.go` — help-surface pin against Wave 9 verb baseline.
- `step4_af_agent_run_test.go` — agent-run dispatch via local control API.
- `step5_af_daemon_operator_endpoints_honest_test.go` — Wave 11 acceptance: kit scan-paths, workarea live-pool, routing decision recording.
- `step6_af_daemon_install_lifecycle_test.go` — install/uninstall with `--skip-service-manager`.
- `step6_af_daemon_kit_lifecycle_test.go` — Wave 12 kit lifecycle (install, verify, tamper, trust-mode gate).
- `step16_arch_assess_native_test.go` — `donmai arch assess` native arch-intel (Layer 1+2) over a fixture PR diff: observations from the native diff-fetch, gate trigger/clear across policies, and the key-gated LLM lane (`mode:native`). Uses a fake `gh`/`claude` on PATH; no platform deps. Point the build at the in-flight port via `DONMAI_ARCH_SOURCE_DIR`.

## Status

**Wave 10 Phase 10 — main smoke suite live.** Wave 4C (env-var / README debrand) complete. Wave 6 (additional coverage) is next.

## How this corpus changes

Permissive-direct-to-main norms; both humans and fleet agents may commit directly. Substantive changes (new smoke tests that materially shift coverage, harness primitive renames that affect the internal platform harness's import surface) follow the ADR pattern declared in `donmai-architecture/AGENTS.md` § "How to disagree with this doc" — open an ADR in the architecture repo, declare its `boundary:` in frontmatter, and update affected smoke files in the same commit that flips the ADR to Accepted.

Non-substantive edits (typos, comment fixes, `.gitignore` adjustments, broken-link repairs) commit directly without ceremony.
