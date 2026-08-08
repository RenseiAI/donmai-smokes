# donmai-smokes — OSS-canonical smoke harness for the donmai binary (OSS-public)

Go, module `github.com/RenseiAI/donmai-smokes`. Builds the `donmai` binary from
the sibling `donmai` checkout, spawns a foreground daemon with an isolated
HOME on a free port, and drives the daemon lifecycle plus the four
daemon-targeted command surfaces (`provider`, `kit`, `workarea`, `routing`)
against the live localhost-only `/api/daemon/*` HTTP control API (default port
7734) — with NO SaaS control plane involved. A forked OSS deployment can run
this harness against its own daemon with zero tenant assumptions baked in.

## Operating context

- System under test: the sibling `donmai` checkout. `harness.LocateDonmaiSource`
  finds it by walking up from the working directory and IDENTIFYING each
  candidate (go.mod module path + `cmd/donmai`), so a primary checkout
  (`<root>/donmai-smokes`), a linked worktree (`<root>/donmai-smokes.wt/<name>`)
  and hosted CI's two-repo layout all resolve without special cases. Missing?
  `gh repo clone RenseiAI/donmai <parent-of-this-repo>/donmai`, or point
  `DONMAI_SMOKES_DONMAI_DIR` at one. **Never** hard-code `../donmai`: it is
  wrong from a worktree, and that is precisely how this suite once reported
  `ok` in 1.2s having built nothing (see §Liveness).
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

- `harness/sut.go` — `LocateDonmaiSource`: finds the donmai checkout by identity, from any layout; returns a `*SUTNotFoundError` naming every path it tried.
- `harness/live_gate.go` — the gate EVERY live smoke passes through: `SkipIfShort` / `SkipIfKnob` / `SkipIfToolMissing` / `DeclineLive` / `RequireDonmaiSource(At)` / `RequireDonmaiBinary`.
- `harness/liveness.go` — the ledger + `CheckLiveness`; `liveness_test.go`'s `TestMain` reds a run that proved nothing.
- `harness/build.go` — `BuildBinary`: `go build` from an explicit SourceDir. Prefer `RequireDonmaiBinary`.
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
- **A missing SUT is a FAILURE, not a skip.** `RequireDonmaiSource` / `RequireDonmaiBinary` `t.Fatal` when the checkout cannot be found or will not build. This reverses the old rule ("every smoke `t.Skip`s cleanly when `../donmai` is absent"), which is what let the whole live suite report `ok`. Never classify a build error by its message text to choose between skip and fail — `TestNoSkipDecidedByErrorText` fails the build if you do.
- **Never call `t.Skip` directly in a live smoke.** Route it so the skip records itself: `SkipIfShort`, `SkipIfKnob`, `SkipIfToolMissing`, `DeclineLive`. `TestSkipDisciplineInLiveSuite` enforces this; a genuinely new precondition goes in its `allowedRawSkips` map with the reason written down.
- **Order the gate: locate → probe → build.** A capability probe (`os.Stat` on a feature file under the checkout) run BEFORE the checkout is located cannot tell "predates the feature" from "there is no checkout", and used to report the first for the second.
- New live-daemon smokes honor `-short` AND `DONMAI_SMOKES_SKIP_LIVE_DAEMON=1` — copy step1's gate block (operators rely on the opt-out).
- Skip knobs are the CI-operator contract — never repurpose or drop one: `DONMAI_SMOKES_SKIP_LIVE_DAEMON=1` (live-daemon steps), `DONMAI_SMOKES_SKIP_INSTALLER=1` (install lifecycle), `DONMAI_SMOKES_SKIP_LIVE_API=1` (live external-API steps, step15). `DONMAI_SMOKES_DONMAI_DIR` points at the donmai checkout under test when it is not a discoverable sibling; a set-but-invalid value FAILS rather than falling back. The opencode harness lane (step18) additionally honors `DONMAI_SMOKES_OPENCODE_BIN` (point at a pre-installed binary, skipping resolution/install entirely) and `DONMAI_SMOKES_OPENCODE_PIN` (override the installed/accepted version — used by the pin-bump protocol, `donmai-architecture` 07-design-opencode-spawn.md §8).
- `DONMAI_ARCH_SOURCE_DIR` points a step's build at an in-flight source port — keep it honored (operators test unmerged ports with it). It is read in exactly one place, `inFlightSourceDir()` in `inflight_source_test.go`; do not re-read it per step, and do not add a hard-coded feature-worktree fallback (three of those went stale, and a stale one silently prefers unmerged code over main).
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

`scripts/guard-b-lint.sh` (vendored from `donmai-architecture`; see its header)
now runs this boundary as an automated check — self-test + `--all` in CI on
every push and PR, `--commits`/`--stdin` scoped to what a PR actually adds.
This section remains the guard for a contributor reading before they push: the
checked-in `.guard-allowlist` narrows the automated guard to the specific
lines that legitimately name a banned token (to forbid it, not to use it) —
re-read both before any push that adds a smoke, an env-var read, or an
outbound URL.

## Gotchas

- First `donmai` build is 60–90 s cold, sub-second warm — budget test timeouts
  accordingly (`GOWORK=off go test -race ./... -timeout 8m`).
- Install-lifecycle smokes (step6/step11) run with `--skip-service-manager`; a
  bare install test can clobber your real developer daemon service.
- `kit-toolchain-e2b/run.sh` is a gated cloud-sandbox kit-provisioning smoke:
  requires `E2B_API_KEY`, exits 0 (skip) without it — it never runs by accident.
- Hosted CI DOES check out the sibling donmai (since `ci: run the
  daemon-lifecycle smokes in hosted CI`, Wave 0.3), so the live steps execute
  there. A local checkout with no sibling now FAILS rather than skipping. Every
  run publishes a PASS/FAIL/SKIP tally to the job summary, and the suite itself
  prints a `live-smoke liveness:` line — read the second one: a PASS count says
  how many tests returned, the liveness line says whether any of them touched
  the binary.

## Liveness — why a green run is now evidence

`go test` exits 0 for a SKIP exactly as for a PASS. In 2026-08 this suite
reported `ok github.com/RenseiAI/donmai-smokes 1.251s` from a linked worktree:
the sibling sat one directory above the hard-coded `../donmai`, every build
failed, and nine copies of a `strings.Contains(err.Error(), "no such file")`
heuristic turned each failure into `t.Skip`. **Two RED CONTROLS "passed"
against it.** A cold donmai build alone takes 60-90s, so the timing was the
only tell.

Two mechanisms make that shape red now, and you need both because the first
only catches known causes:

1. **Loud resolution.** An unlocatable or unbuildable SUT fails the test.
2. **The liveness ledger.** Every live smoke records how its gate resolved —
   `opted-out` (a human asked), `exercised` (the binary was built and driven),
   or `declined` (the SUT was found but lacks the surface). A run that DECLINED
   and exercised nothing is red, whatever the reason for each individual skip.
   Opt-out-only runs (`-short`, a `DONMAI_SMOKES_SKIP_*` knob) stay green.

To verify the gate itself still bites, remove the SUT and run the suite: the
live smokes must FAIL, not skip.
- The sibling is pinned to `ref: main`, a ROLLING target. A smoke asserting
  donmai behaviour that has not merged upstream yet will fail, then pass on a
  re-run once it does — that is NOT a flake. The run's job summary records the
  exact donmai SHA under test; check the assertion's upstream change is in it
  before hunting for a race.

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
