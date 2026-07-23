# donmai-smokes — OSS-canonical smoke harness for the donmai binary (OSS-public)

Go, module `github.com/RenseiAI/donmai-smokes`. Builds the `donmai` binary from
the sibling `../donmai` checkout, spawns a foreground daemon with an isolated
HOME on a free port, and drives the daemon lifecycle plus the four
daemon-targeted command surfaces (`provider`, `kit`, `workarea`, `routing`)
against the live localhost-only `/api/daemon/*` HTTP control API (default port
7734) — with NO SaaS control plane involved. A forked OSS deployment can run
this harness against its own daemon with zero tenant assumptions baked in.

## Operating context

- System under test: `../donmai`. Missing? `gh repo clone RenseiAI/donmai
  ../donmai` (from a worktree, siblings sit at `../../<repo>`).
- Governing corpus: `../donmai-architecture/` — the corpus wins over code;
  align the code or open an ADR. Shared playbook:
  `../donmai-architecture/agents/PROTOCOL.md`.
- Conventions mirror `../donmai/AGENTS.md` (same golangci-lint checks, same
  stdlib-testing discipline, 80% coverage target / 70% minimum).
- Commit norms: permissive direct-to-main for humans and fleet agents alike.
  Substantive changes (coverage-shifting smokes, harness-primitive renames that
  move the internal harness's import surface) follow the corpus ADR pattern;
  typo/comment/`.gitignore` fixes commit without ceremony.
- The commercial platform keeps a separate internal harness; anything
  platform-coupled lives there, never here (§Boundary below).

## Before you start — read in this order

| The moment you... | Read |
|---|---|
| start ANY task in this repo | this file, top to bottom (it is short) |
| assert against a daemon endpoint, CLI verb, or provider/kit/workarea/routing surface | `../donmai-architecture/001-layered-execution-model.md` → `011-local-daemon-fleet.md` → `ADR-2026-05-07-daemon-http-control-api.md` (the contract this harness pins) |
| write a new smoke or touch a harness primitive | §Harness map below + copy the skip pattern in `step1_af_daemon_lifecycle_test.go` |
| are about to add an env var, external service, or credential to a smoke | §Boundary below |
| are about to write "done"/"fixed" or push | Gates below + `../donmai-architecture/agents/PROTOCOL.md` §V |
| hit a failing test or `-race` flake you did not predict | `../donmai-architecture/agents/PROTOCOL.md` §D |

When a row matches, read that doc before your next edit and follow it literally.

## Gates — "done" means these passed

```bash
make test    # GOWORK=off go test -race ./...   (the race flag is mandatory)
make lint    # golangci-lint run ./...
make fmt     # gofumpt -w .
```

CI (`.github/workflows/test.yml`) runs `go vet`, `GOWORK=off go test -race -v
./...` (ubuntu + macos matrix), and golangci-lint — aligned with the Makefile
as of 2026-07-07. Still run the gates locally after your last edit and quote
each result line in your report.

## Harness map — read before writing a new smoke

- `harness/build.go` — `BuildDonmaiBinary`/`BuildBinary`: `go build` from `../donmai`.
- `harness/live_daemon.go` — `LiveDaemonWithConfig`: spawn + healthz-wait, optional daemon.yaml pre-write.
- `harness/daemon_detect.go` — `DaemonAvailable`: probe daemon reachability.
- `harness/runner.go` — `Runner`: subprocess executor (dry-run, verbose, timeout, binary override).
- `harness/help_parser.go` — `ParseHelpSubcommands`: parse Cobra `--help` Available Commands.
- `harness/errors.go` — `WrapStep`/`StepError` step-context wrapping + `IsUnknownSubcommand`.
- `harness/opencode_install.go` — `EnsureOpenCodeBinary`: resolves/installs the pinned opencode CLI (npm, isolated prefix) for the step18 opencode harness lane.
- `setup_live_daemon_test.go` — shared `setupLiveDaemon`; new live-daemon steps reuse it.

## Iron rules

- Tests: stdlib `testing`, table-driven, no testify; `httptest` for HTTP fixtures (divergence breaks `../donmai` parity).
- Errors: `fmt.Errorf("context: %w", err)`; step context via `harness.WrapStep`. Never `panic`, never `log.Fatal` (kills the whole suite).
- GOWORK is two-sided: tests run `GOWORK=off`; the subprocess building donmai gets `GOWORK=` (cleared) so `../donmai`'s own `go.mod` resolves — never "fix" either (workspace overlay corrupts both resolutions).
- Every smoke `t.Skip`s cleanly when `../donmai` or the Go toolchain is absent (hosted CI has no sibling).
- New live-daemon smokes honor `-short` AND `DONMAI_SMOKES_SKIP_LIVE_DAEMON=1` — copy step1's skip block (operators rely on the opt-out).
- Skip knobs are the CI-operator contract — never repurpose or drop one: `DONMAI_SMOKES_SKIP_LIVE_DAEMON=1` (live-daemon steps), `DONMAI_SMOKES_SKIP_INSTALLER=1` (install lifecycle), `DONMAI_SMOKES_SKIP_LIVE_API=1` (live external-API steps, step15). The opencode harness lane (step18) additionally honors `DONMAI_SMOKES_OPENCODE_BIN` (point at a pre-installed binary, skipping resolution/install entirely) and `DONMAI_SMOKES_OPENCODE_PIN` (override the installed/accepted version — used by the pin-bump protocol, `donmai-architecture` 07-design-opencode-spawn.md §8).
- `DONMAI_ARCH_SOURCE_DIR` points the step16 build at an in-flight source port — keep it honored (operators test unmerged ports with it).
- step16 runs against fake `gh`/`claude` shims prepended to PATH — extend the shims, never invoke the real tools (real calls burn credentials).
- Logging: `log/slog` to stderr through the harness verbose/quiet toggle (raw prints drown CI logs).

## Boundary — platform-free by contract (the heart of this repo)

OSS-public. If a smoke needs the SaaS control plane to assert ANYTHING, the
boundary is violated and the smoke belongs in the commercial platform's
internal harness — relocate it, do not land it. The concrete bans (each
deliberately NAMES the banned token):

- No WorkOS auth: no `WORKOS_TEST_EMAIL` / `WORKOS_TEST_PASSWORD` /
  `WORKOS_API_KEY` reads, no `api.workos.com` calls, no platform-config-shaped
  JSON injection carrying `active_auth`.
- No Linear orchestration: no `api.linear.app/graphql` calls, no
  `LINEAR_API_KEY` reads, no `issueCreate` / `issueArchive` mutations.
- No platform endpoints: no `/api/cli/*` (the platform's CLI-auth namespace),
  no `/api/workers/register` and friends. The daemon's own `/api/daemon/*` IS
  fair game — it ships in OSS.
- No `rsk_*` tokens: no `RENSEI_TEST_TOKEN` reads, no token-injection helpers,
  no scope-string assertions (`worker:register`, `worker:poll`,
  `worker:heartbeat`, `worker:session`).
- No GitHub orchestration: no `GITHUB_TOKEN`-driven PR / merge / branch-delete
  operations (`gh` as a build or fixture-scaffolding dep is fine).
- A smoke that reaches into one of these corners only to cross-check something
  the OSS layer can detect another way -> refactor to the OSS detection.

No automated leak guard exists in this repo (unlike `../donmai`'s `make
guard`) — this section is the guard. Re-read it before any push that adds a
smoke, an env-var read, or an outbound URL.

## Gotchas

- First `donmai` build is 60–90 s cold, sub-second warm — budget test timeouts
  accordingly (`GOWORK=off go test -race ./... -timeout 8m`).
- Install-lifecycle smokes (step6/step11) run with `--skip-service-manager`; a
  bare install test can clobber your real developer daemon service.
- `kit-toolchain-e2b/run.sh` is a gated cloud-sandbox kit-provisioning smoke:
  requires `E2B_API_KEY`, exits 0 (skip) without it — it never runs by accident.
- Hosted CI has no `../donmai` sibling: live steps skip via
  `harness.BuildBinary` → `t.Skipf`; the `harness/*` unit suite carries the run.

## Hard stops

- NEVER land a smoke that requires the SaaS control plane, even "temporarily"
  -> instead: describe it in your report for the internal harness.
- NEVER commit a private reference (tracker IDs, closed-source repo links,
  internal hostnames, secrets) -> instead: rewrite brand-neutrally first.
- NEVER make a failing smoke pass by weakening it (skip, deleted assert,
  loosened match) -> instead: quote the failure and propose the change.
- NEVER modify `../donmai` from a smokes session -> instead: note the needed
  upstream change in your report.
- NEVER run `git worktree remove/prune`, `git reset --hard`, `git clean -fd`,
  or checkout to another branch as a sub-agent -> instead: the orchestrator
  owns worktree lifecycle.
