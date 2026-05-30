#!/usr/bin/env python3
"""kit_toolchain_e2b.py — real-cloud (e2b) proof of the donmai "kits pivot".

GOAL (K3/K4): prove that a CLOUD sandbox installs the language toolchain via a
kit AFTER the repo is in place, then runs the kit's post_acquire + build/test.

LEVEL OF PROOF — read this honestly:

  This is a MECHANISM-LEVEL proof, not the full daemon session path.

  donmai's REAL code under test is the kit MANIFEST SCHEMA and the
  detect -> compose -> provision PIPELINE SEMANTICS. This driver re-implements,
  step for step, the exact ordering contract in:
    - donmai/daemon/kit_detect.go     (detectMatches: [detect].files ANY-exists,
                                       not_files exclusion, [supports].os gate)
    - donmai/internal/kit/compose.go  (Compose: OS-key select, sort keys within
                                       a kit for determinism, foundation-first
                                       order, conjunctive union + dedup of
                                       toolchain_install, OS-overlay hook resolve)
    - donmai/internal/kit/provisioner.go (Provision: toolchain_install THEN
                                       post_acquire, fail-closed on non-zero)
    - donmai/runner/loop.go           (resolveKitDemand -> Compose -> Provision
                                       runs AFTER worktree clone, line ~182)
  and runs the resulting commands against a REAL e2b sandbox via the e2b SDK.

  What this driver is NOT:
    - It does not invoke the donmai daemon / `donmai agent run` session path.
      The daemon wires KitRegistry.DetectForRepo -> kit.Compose ->
      kit.Provisioner.Provision through runner/loop.go, but the only Execer
      shipped is shellExecer (LOCAL worktree, runner/kit_execer.go). donmai has
      NO e2b/cloud-sandbox provider yet: the kit_execer.go docstring states the
      cloud Execer "in K2" is future work, and there is no provider/e2b package.
      So the full daemon->e2b path cannot be driven today regardless of harness.
    - The e2b transport here is the e2b Python SDK, standing in for the
      not-yet-written donmai cloud Execer.

  So: the kit definition, detection, composition and phase ordering under test
  are donmai's own contract; only the sandbox transport differs. A green run
  proves the kits pivot is sound on real cloud infra and de-risks the K2 cloud
  Execer. It does not prove the daemon session wiring end to end.

Gated by run.sh behind E2B_API_KEY. Exits non-zero on any failure. Keeps e2b
usage minimal: ONE sandbox per run, killed in finally.
"""

import os
import sys
import time
import fnmatch
import pathlib

try:
    import tomllib as _toml  # Python 3.11+
    _TOML_BINARY = True
except ImportError:
    try:
        import tomli as _toml  # type: ignore
        _TOML_BINARY = True
    except ImportError:
        print("[error] need Python 3.11+ (tomllib) or `pip install --user tomli`", file=sys.stderr)
        sys.exit(2)

try:
    from e2b import Sandbox
except ImportError:
    print("[error] e2b SDK not installed; run: python3 -m pip install --user e2b", file=sys.stderr)
    sys.exit(2)

HERE = pathlib.Path(__file__).resolve().parent
REPO_DIR = "/home/user/repo"
TARGET_OS = "linux"  # cloud sandboxes are linux even on a macOS host (compose.go:264)


def log(m):
    print(m, flush=True)


# --- donmai daemon/kit_detect.go: detectMatches (Phase-1 declarative). ----------
def detect_matches(manifest, present_files):
    """ANY of [detect].files exists; [detect].not_files exclusion; needs a
    positive matcher. Mirrors detectMatches() exactly."""
    det = manifest.get("detect", {})
    not_files = det.get("not_files", [])
    files = det.get("files", [])
    files_all = det.get("files_all", [])

    def exists(rel):
        return any(rel == f or fnmatch.fnmatch(f, rel) for f in present_files)

    for f in not_files:
        if exists(f):
            return False
    if not files and not files_all:
        return False  # no positive matcher -> no Phase-1 match
    for f in files_all:
        if not exists(f):
            return False
    if files and not any(exists(f) for f in files):
        return False
    return True


def os_supported(supported, target):
    return (not supported) or (target in supported)


_ORDER_RANK = {"foundation": 0, "framework": 1, "project": 2}


def order_rank(o):
    return _ORDER_RANK.get(o, 2)


# --- donmai internal/kit/compose.go: SortManifests + Compose. -------------------
def compose(manifests, target_os):
    """Replicates Compose(): ordered foundation->framework->project, OS-key
    select, sort keys within a kit, conjunctive union + dedup of
    toolchain_install, OS-overlay hook resolve. Returns the ToolchainDemand."""
    views = sorted(
        manifests,
        key=lambda m: (
            order_rank(m.get("composition", {}).get("order", "")),
            -int(m.get("kit", {}).get("priority", 0)),
            m.get("kit", {}).get("id", ""),
        ),
    )
    demand = {"kits": [], "os": target_os, "toolchain_install": [], "post_acquire": [], "pre_release": []}
    seen = set()
    for m in views:
        supports = m.get("supports", {}).get("os", [])
        if not os_supported(supports, target_os):
            continue
        kit = m.get("kit", {})
        ref = kit.get("id", "") + ("@" + kit["version"] if kit.get("version") else "")
        demand["kits"].append(ref)

        os_map = m.get("provide", {}).get("toolchain_install", {}).get(target_os, {})
        for key in sorted(os_map):  # sort keys within a kit (determinism)
            cmd = os_map[key]
            if not cmd or cmd in seen:
                continue
            seen.add(cmd)
            demand["toolchain_install"].append(cmd)

        hooks = m.get("provide", {}).get("hooks", {})
        os_overlay = hooks.get("os", {}).get(target_os, {})
        pa = os_overlay.get("post_acquire") or hooks.get("post_acquire")
        if pa:
            demand["post_acquire"].append(pa)
        pr = os_overlay.get("pre_release") or hooks.get("pre_release")
        if pr:
            demand["pre_release"].append(pr)
    return demand


def sb_run(sb, cmd, phase, as_root=False, quiet=False):
    """Run one command in REPO_DIR. e2b base template is non-root 'user'; the
    kit's linux toolchain_install assumes root (apt-get), so privileged
    commands are wrapped in sudo — the analogue of a root cloud sandbox.

    quiet=True redirects the command's (very chatty) output to a file inside
    the sandbox and only echoes a short tail. apt + NodeSource emit thousands
    of progress lines; streaming them all over the long-lived gRPC channel
    intermittently trips an e2b StreamReset. Capturing in-sandbox keeps the
    exit code authoritative while cutting stream volume."""
    privileged = as_root or "apt-get" in cmd or "deb.nodesource.com" in cmd
    inner = ("sudo bash -c " + _shq(cmd)) if privileged else cmd
    log(f"[{phase}] $ {cmd}")
    if quiet:
        logf = "/tmp/kit_phase.log"
        full = f"cd {REPO_DIR} && ({inner}) >{logf} 2>&1; rc=$?; echo \"--- exit $rc (tail) ---\"; tail -n 8 {logf}; exit $rc"
    else:
        full = f"cd {REPO_DIR} && {inner}"
    r = sb.commands.run(full, timeout=480)
    out = (r.stdout or "").strip()
    err = (r.stderr or "").strip()
    if out:
        log(_indent(out))
    if err:
        log(_indent("(stderr) " + err))
    return r


def provision(sb, demand):
    """Replicates Provisioner.Provision: toolchain_install THEN post_acquire,
    fail-closed (raise) on the first non-zero exit."""
    for cmd in demand["toolchain_install"]:
        r = sb_run(sb, cmd, "toolchain_install", quiet=True)
        if r.exit_code != 0:
            raise RuntimeError(f"kit provision toolchain_install: command exited {r.exit_code}")
    for cmd in demand["post_acquire"]:
        r = sb_run(sb, cmd, "post_acquire", quiet=True)
        if r.exit_code != 0:
            raise RuntimeError(f"kit provision post_acquire: command exited {r.exit_code}")


def _shq(s):
    return "'" + s.replace("'", "'\\''") + "'"


def _indent(s, n=4):
    pad = " " * n
    return "\n".join(pad + ln for ln in s.splitlines()[:80])


def load_manifests(kits_dir):
    out = []
    for p in sorted(pathlib.Path(kits_dir).glob("*.kit.toml")):
        with open(p, "rb") as f:
            m = _toml.load(f)
        if m.get("kit", {}).get("id"):
            out.append(m)
    return out


def main():
    if not os.environ.get("E2B_API_KEY"):
        print("[error] E2B_API_KEY not set", file=sys.stderr)
        return 2

    kits_dir = HERE / "kits"
    fixture = HERE / "fixtures" / "ts-next"
    manifests = load_manifests(kits_dir)
    if not manifests:
        print(f"[error] no .kit.toml under {kits_dir}", file=sys.stderr)
        return 2
    log(f"[info] loaded kit manifests: {[m['kit']['id'] for m in manifests]}")

    sb = None
    try:
        t0 = time.time()
        sb = Sandbox.create(timeout=600)
        log(f"[ok] e2b sandbox created: {sb.sandbox_id} ({time.time()-t0:.1f}s, debian/linux)")

        # --- Stage repo (post-clone state). Real git clone if TS_FIXTURE_REPO is
        # set + reachable; else upload the local fixture (deterministic). Either
        # way the toolchain is installed AFTER the repo is present. ---
        staged_via = None
        repo_url = os.environ.get("TS_FIXTURE_REPO", "").strip()
        if repo_url:
            r = sb.commands.run(f"git clone --depth 1 {repo_url} {REPO_DIR}", timeout=120)
            if r.exit_code == 0:
                staged_via = f"git clone {repo_url}"
            else:
                log(f"[warn] clone failed ({r.exit_code}): {(r.stderr or '').strip()[:160]}")
        if not staged_via:
            sb.commands.run(f"mkdir -p {REPO_DIR}/src")
            for rel in ["package.json", "tsconfig.json", "src/index.ts"]:
                sb.files.write(f"{REPO_DIR}/{rel}", (fixture / rel).read_text())
            staged_via = "uploaded local fixture"
        log(f"[ok] repo staged via: {staged_via}")

        # HONEST clean base: the e2b base image SHIPS node at /usr/local/bin/node
        # (v20.9.0). If we left it, "node after install" would be meaningless —
        # the kit might never have run. So we REMOVE the bundled node first, then
        # assert it is genuinely gone. The kit's NodeSource toolchain_install
        # then reinstalls it at /usr/bin/node (a different path), proving the
        # kit — not the base image — provisioned the toolchain.
        sb.commands.run(
            "sudo rm -f /usr/local/bin/node /usr/local/bin/npm /usr/local/bin/npx",
            timeout=60,
        )
        node_before = sb.commands.run("which node || echo NO_NODE").stdout.strip()
        log(f"[info] node before toolchain_install (after stripping base node): {node_before!r}")
        if "NO_NODE" not in node_before:
            raise RuntimeError(
                f"failed to establish clean base — node still present at {node_before!r}; "
                "the proof requires node ABSENT before the kit installs it")
        present = sb.commands.run(f"ls -1 {REPO_DIR}").stdout.strip().splitlines()
        log(f"[info] repo files present: {present}")

        # --- Detect (donmai detectMatches) against the staged file set. ---
        matched = [m for m in manifests
                   if os_supported(m.get("supports", {}).get("os", []), TARGET_OS)
                   and detect_matches(m, set(present))]
        if not matched:
            raise RuntimeError(f"no kit matched staged repo files={present}")
        log(f"[ok] detected kits: {[m['kit']['id'] for m in matched]}")

        # --- Compose (donmai Compose) -> ToolchainDemand. ---
        demand = compose(matched, TARGET_OS)
        log(f"[ok] composed demand: kits={demand['kits']} "
            f"toolchain_install={len(demand['toolchain_install'])} "
            f"post_acquire={len(demand['post_acquire'])}")

        # --- Provision (donmai Provisioner.Provision): install THEN deps. ---
        provision(sb, demand)

        # --- Assert node now present (was genuinely absent before). ---
        post = sb.commands.run("which node && node --version")
        if post.exit_code != 0 or not post.stdout.strip():
            raise RuntimeError("node not present after toolchain_install")
        post_lines = post.stdout.strip().splitlines()
        node_path_after = post_lines[0]
        node_after = post_lines[-1]
        log(f"[ok] node installed AFTER repo present: {node_after} at {node_path_after}")

        # --- Kit build/test: the verify the task asks for. ---
        build = sb_run(sb, "npm run build", "verify")
        if build.exit_code != 0:
            raise RuntimeError(f"kit build (npm run build) failed: exit {build.exit_code}")
        test = sb_run(sb, "npm test", "verify")
        if test.exit_code != 0 or "KIT_TOOLCHAIN_BUILD_TEST_OK" not in (test.stdout or ""):
            raise RuntimeError(f"kit test failed or marker missing: exit {test.exit_code}")

        log("")
        log("=== PROOF SUMMARY (mechanism-level, real e2b) ===")
        log(f"  sandbox:        {sb.sandbox_id} (e2b base, debian bookworm/linux)")
        log(f"  kits detected:  {demand['kits']}")
        log(f"  repo staged:    {staged_via}  (toolchain installed AFTER)")
        log(f"  node before:    {node_before} (base node stripped)")
        log(f"  node after:     {node_after} at {node_path_after}")
        log("  npm run build:  PASSED")
        log("  npm test:       PASSED (KIT_TOOLCHAIN_BUILD_TEST_OK observed)")
        log("[ok] KIT TOOLCHAIN E2B PROOF: PASS")
        return 0
    except Exception as e:  # noqa: BLE001
        log(f"[error] {type(e).__name__}: {e}")
        return 1
    finally:
        if sb is not None:
            try:
                sb.kill()
                log(f"[info] sandbox killed: {sb.sandbox_id}")
            except Exception as e:  # noqa: BLE001
                log(f"[warn] sandbox kill failed: {e}")


if __name__ == "__main__":
    sys.exit(main())
