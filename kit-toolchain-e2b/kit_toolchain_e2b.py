#!/usr/bin/env python3
"""Mechanism-level kit toolchain proof against one gated e2b sandbox.

The driver mirrors donmai's declarative detect -> compose -> provision ordering:
repo first, toolchain_install second, post_acquire third, then kit build/test.
The default ts-next profile preserves the original smoke. The swift profile
tracks the signed default/swift@1.0.0 manifest's Linux swiftly installer and
runs a dependency-free SwiftPM fixture.

This is not the daemon session path and does not verify signature trust. The e2b
SDK is imported lazily only for a real run. ``--dry-run`` performs manifest,
fixture, composition, and exact command-plan validation without credentials,
SDK installation, network access, or sandbox creation.
"""

from __future__ import annotations

import argparse
import contextvars
import fnmatch
import os
import pathlib
import re
import sys
import time
from dataclasses import dataclass
from typing import Any, Callable

try:
    import tomllib as _toml  # Python 3.11+
except ImportError:
    try:
        import tomli as _toml  # type: ignore
    except ImportError:
        print(
            "[error] need Python 3.11+ (tomllib) or the tomli package",
            file=sys.stderr,
        )
        raise SystemExit(2)

HERE = pathlib.Path(__file__).resolve().parent
REPO_DIR = "/home/user/repo"
TARGET_OS = "linux"
SENSITIVE_ENV_SUFFIXES = ("_KEY", "_TOKEN", "_SECRET", "_PASSWORD")
SENSITIVE_ENV_NAMES = {"TS_FIXTURE_REPO"}
URL_USERINFO_RE = re.compile(r"(?P<scheme>https?://)[^/@\s]+@", re.IGNORECASE)
MAX_SANDBOX_TIMEOUT = 900
MAX_COMMAND_TIMEOUT = 600
FIXTURE_IGNORED_DIRS = {".build", "__pycache__", "node_modules"}
_RUN_REDACTIONS: contextvars.ContextVar[tuple[str, ...]] = contextvars.ContextVar(
    "kit_toolchain_run_redactions", default=()
)
SWIFT_ENV = (
    'if [ -f "$HOME/.local/share/swiftly/env.sh" ]; then '
    '. "$HOME/.local/share/swiftly/env.sh"; '
    'elif [ -f "$HOME/.swiftly/env.sh" ]; then '
    '. "$HOME/.swiftly/env.sh"; '
    'else echo "swiftly environment file missing" >&2; exit 1; fi'
)


@dataclass(frozen=True)
class KitProfile:
    name: str
    manifest_name: str
    fixture_name: str
    expected_id: str
    expected_version: str
    version_command: str
    version_pattern: str
    build_command: str
    test_command: str
    test_marker: str
    default_sandbox_timeout: int
    default_command_timeout: int
    clone_env: str | None = None


@dataclass(frozen=True)
class ProofEvidence:
    kit_reference: str
    build_step: str
    test_step: str


PROFILES = {
    "ts-next": KitProfile(
        name="ts-next",
        manifest_name="ts-next.kit.toml",
        fixture_name="ts-next",
        expected_id="ts-next",
        expected_version="0.1.0",
        version_command="command -v node && node --version",
        version_pattern=r"\bv20\.",
        build_command="npm run build",
        test_command="npm test",
        test_marker="KIT_TOOLCHAIN_BUILD_TEST_OK",
        default_sandbox_timeout=600,
        default_command_timeout=480,
        clone_env="TS_FIXTURE_REPO",
    ),
    "swift": KitProfile(
        name="swift",
        manifest_name="swift.kit.toml",
        fixture_name="swift",
        expected_id="default/swift",
        expected_version="1.0.0",
        version_command=(
            f"{SWIFT_ENV}; "
            'printf "swiftly=%s\\n" "$(command -v swiftly)"; '
            'printf "swift=%s\\n" "$(command -v swift)"; '
            "swift --version"
        ),
        version_pattern=r"\b6\.0\.3\b",
        build_command=f"{SWIFT_ENV}; swift build",
        test_command=f"{SWIFT_ENV}; swift test",
        test_marker="KIT_TOOLCHAIN_SWIFT_TEST_OK",
        default_sandbox_timeout=900,
        default_command_timeout=600,
    ),
}

_ORDER_RANK = {"foundation": 0, "framework": 1, "project": 2}


def sensitive_values() -> list[str]:
    """Return known secret values for output scrubbing without logging names."""
    values: list[str] = []
    for name, value in os.environ.items():
        if not value or len(value) < 4:
            continue
        if name in SENSITIVE_ENV_NAMES or name.endswith(SENSITIVE_ENV_SUFFIXES):
            values.append(value)
    return sorted(set(values), key=len, reverse=True)


def redact_text(value: Any, secrets: list[str] | None = None) -> str:
    """Scrub known secrets, run identifiers, and URL userinfo from evidence."""
    text = str(value)
    known_values = sensitive_values() if secrets is None else list(secrets)
    known_values.extend(_RUN_REDACTIONS.get())
    for secret in sorted(set(known_values), key=len, reverse=True):
        if secret:
            text = text.replace(secret, "<redacted>")
    return URL_USERINFO_RE.sub(r"\g<scheme><redacted>@", text)


def log(message: Any) -> None:
    print(redact_text(message), flush=True)


def detect_matches(manifest: dict[str, Any], present_files: set[str]) -> bool:
    """Mirror daemon/kit_detect.go's declarative file matching."""
    detect = manifest.get("detect", {})
    not_files = detect.get("not_files", [])
    files = detect.get("files", [])
    files_all = detect.get("files_all", [])

    def exists(relative: str) -> bool:
        return any(
            relative == present or fnmatch.fnmatch(present, relative)
            for present in present_files
        )

    if any(exists(relative) for relative in not_files):
        return False
    if not files and not files_all:
        return False
    if any(not exists(relative) for relative in files_all):
        return False
    return not files or any(exists(relative) for relative in files)


def os_supported(supported: list[str], target: str) -> bool:
    return not supported or target in supported


def order_rank(order: str) -> int:
    return _ORDER_RANK.get(order, 2)


def compose(manifests: list[dict[str, Any]], target_os: str) -> dict[str, Any]:
    """Mirror foundation-first deterministic toolchain/hook composition."""
    views = sorted(
        manifests,
        key=lambda manifest: (
            order_rank(manifest.get("composition", {}).get("order", "")),
            -int(manifest.get("kit", {}).get("priority", 0)),
            manifest.get("kit", {}).get("id", ""),
        ),
    )
    demand: dict[str, Any] = {
        "kits": [],
        "os": target_os,
        "toolchain_install": [],
        "post_acquire": [],
        "pre_release": [],
    }
    seen: set[str] = set()
    for manifest in views:
        supports = manifest.get("supports", {}).get("os", [])
        if not os_supported(supports, target_os):
            continue
        kit = manifest.get("kit", {})
        reference = kit.get("id", "")
        if kit.get("version"):
            reference += "@" + kit["version"]
        demand["kits"].append(reference)

        os_map = (
            manifest.get("provide", {})
            .get("toolchain_install", {})
            .get(target_os, {})
        )
        for key in sorted(os_map):
            command = os_map[key]
            if not command or command in seen:
                continue
            seen.add(command)
            demand["toolchain_install"].append(command)

        hooks = manifest.get("provide", {}).get("hooks", {})
        os_overlay = hooks.get("os", {}).get(target_os, {})
        post_acquire = os_overlay.get("post_acquire") or hooks.get("post_acquire")
        if post_acquire:
            demand["post_acquire"].append(post_acquire)
        pre_release = os_overlay.get("pre_release") or hooks.get("pre_release")
        if pre_release:
            demand["pre_release"].append(pre_release)
    return demand


def load_manifest(profile: KitProfile) -> dict[str, Any]:
    path = HERE / "kits" / profile.manifest_name
    with path.open("rb") as handle:
        manifest = _toml.load(handle)
    kit = manifest.get("kit", {})
    if kit.get("id") != profile.expected_id:
        raise ValueError(
            f"{path.name}: kit.id={kit.get('id')!r}, want {profile.expected_id!r}"
        )
    if kit.get("version") != profile.expected_version:
        raise ValueError(
            f"{path.name}: kit.version={kit.get('version')!r}, "
            f"want {profile.expected_version!r}"
        )
    return manifest


def fixture_root(profile: KitProfile) -> pathlib.Path:
    root = HERE / "fixtures" / profile.fixture_name
    if not root.is_dir():
        raise ValueError(f"missing fixture directory: {root}")
    return root


def fixture_files(profile: KitProfile) -> list[pathlib.Path]:
    root = fixture_root(profile)
    files = sorted(
        path
        for path in root.rglob("*")
        if path.is_file()
        and not FIXTURE_IGNORED_DIRS.intersection(path.relative_to(root).parts)
    )
    if not files:
        raise ValueError(f"fixture has no files: {root}")
    return files


def fixture_relative_files(profile: KitProfile) -> list[str]:
    root = fixture_root(profile)
    return [path.relative_to(root).as_posix() for path in fixture_files(profile)]


def build_plan(profile: KitProfile) -> dict[str, Any]:
    manifest = load_manifest(profile)
    present_files = set(fixture_relative_files(profile))
    if not detect_matches(manifest, present_files):
        raise ValueError(
            f"{profile.name}: manifest did not detect fixture files {sorted(present_files)}"
        )
    demand = compose([manifest], TARGET_OS)
    expected_ref = f"{profile.expected_id}@{profile.expected_version}"
    if demand["kits"] != [expected_ref]:
        raise ValueError(
            f"{profile.name}: composed kits={demand['kits']!r}, want {[expected_ref]!r}"
        )
    if not demand["toolchain_install"]:
        raise ValueError(f"{profile.name}: no Linux toolchain_install command")
    if not demand["post_acquire"]:
        raise ValueError(f"{profile.name}: no post_acquire command")
    return {
        "profile": profile,
        "manifest": manifest,
        "present_files": sorted(present_files),
        "demand": demand,
        "verify": [
            ("version", profile.version_command),
            ("build", profile.build_command),
            ("test", profile.test_command),
        ],
    }


def render_dry_run(profile: KitProfile) -> int:
    plan = build_plan(profile)
    demand = plan["demand"]
    log("[dry-run] no credentials loaded; no SDK import; no sandbox created")
    log(f"[plan] profile={profile.name}")
    log(f"[plan] fixture_files={plan['present_files']}")
    log(f"[plan] detected={demand['kits']}")
    for index, command in enumerate(demand["toolchain_install"], start=1):
        log(f"[plan] toolchain_install[{index}]={command}")
    for index, command in enumerate(demand["post_acquire"], start=1):
        log(f"[plan] post_acquire[{index}]={command}")
    for phase, command in plan["verify"]:
        log(f"[plan] verify_{phase}={command}")
    log(f"[ok] KIT TOOLCHAIN DRY RUN: {profile.name} PASS")
    return 0


def _shq(value: str) -> str:
    return "'" + value.replace("'", "'\\''") + "'"


def _indent(value: str, limit: int = 80) -> str:
    lines = redact_text(value).splitlines()[:limit]
    return "\n".join("    " + line for line in lines)


def _result_text(result: Any, stream: str) -> str:
    value = getattr(result, stream, "") or ""
    return str(value)


def require_zero(result: Any, context: str) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code != 0:
        raise RuntimeError(f"{context}: command exited {exit_code}")


def validate_version_output(profile: KitProfile, output: str) -> None:
    if re.search(profile.version_pattern, output) is None:
        raise RuntimeError(
            f"{profile.name} version output did not match {profile.version_pattern!r}"
        )
    if profile.name == "swift":
        swift_path = re.search(r"^swift=(.+)$", output, re.MULTILINE)
        if swift_path is None or "swiftly" not in swift_path.group(1):
            raise RuntimeError("swift resolved outside the swiftly-managed toolchain")


def sb_run(
    sandbox: Any,
    command: str,
    phase: str,
    *,
    timeout: int,
    quiet: bool = False,
) -> Any:
    """Run a fixed manifest/verification command with bounded, redacted output."""
    privileged = "apt-get" in command or "deb.nodesource.com" in command
    inner = "sudo bash -c " + _shq(command) if privileged else command
    log(f"[{phase}] $ {command}")
    if quiet:
        log_path = "/tmp/kit_toolchain_phase.log"
        full = (
            f"cd {_shq(REPO_DIR)} && ({inner}) >{log_path} 2>&1; "
            'rc=$?; printf "%s\\n" "--- exit $rc (tail) ---"; '
            f"tail -n 12 {log_path}; exit $rc"
        )
    else:
        full = f"cd {_shq(REPO_DIR)} && {inner}"
    result = sandbox.commands.run(full, timeout=timeout)
    stdout = _result_text(result, "stdout").strip()
    stderr = _result_text(result, "stderr").strip()
    if stdout:
        log(_indent(stdout))
    if stderr:
        log(_indent("(stderr) " + stderr))
    return result


def stage_fixture(sandbox: Any, profile: KitProfile) -> str:
    """Stage a deterministic fixture; Swift never uses an external repository."""
    if profile.clone_env:
        repo_url = os.environ.get(profile.clone_env, "").strip()
        if repo_url:
            clone = sandbox.commands.run(
                f"git clone --depth 1 {_shq(repo_url)} {_shq(REPO_DIR)}",
                timeout=120,
            )
            if getattr(clone, "exit_code", None) == 0:
                return "git clone (remote redacted)"
            log(
                f"[warn] optional fixture clone failed with exit "
                f"{getattr(clone, 'exit_code', None)}; using local fixture"
            )

    root = fixture_root(profile)
    relative_files = fixture_relative_files(profile)
    directories = sorted(
        {pathlib.PurePosixPath(relative).parent.as_posix() for relative in relative_files}
        - {"."}
    )
    mkdir_targets = [REPO_DIR, *[f"{REPO_DIR}/{directory}" for directory in directories]]
    prepare = sandbox.commands.run(
        "rm -rf " + _shq(REPO_DIR) + " && mkdir -p "
        + " ".join(_shq(target) for target in mkdir_targets),
        timeout=60,
    )
    require_zero(prepare, "stage fixture directories")
    for relative in relative_files:
        sandbox.files.write(
            f"{REPO_DIR}/{relative}", (root / relative).read_text(encoding="utf-8")
        )
    return "uploaded deterministic local fixture"


def establish_clean_base(sandbox: Any, profile: KitProfile) -> str:
    """Remove profile-owned preloads and fail closed if the manager remains."""
    if profile.name == "ts-next":
        result = sandbox.commands.run(
            "sudo rm -f /usr/local/bin/node /usr/local/bin/npm /usr/local/bin/npx; "
            "command -v node >/dev/null 2>&1 && command -v node || printf 'NO_NODE\\n'",
            timeout=60,
        )
        require_zero(result, "establish clean node base")
        before = _result_text(result, "stdout").strip()
        if "NO_NODE" not in before:
            raise RuntimeError(f"node remains on clean base at {before!r}")
        return before

    result = sandbox.commands.run(
        'rm -rf "$HOME/.local/share/swiftly" "$HOME/.swiftly"; '
        'rm -f "$HOME/.local/bin/swiftly"; '
        "command -v swiftly >/dev/null 2>&1 && command -v swiftly || "
        "printf 'NO_SWIFTLY\\n'; "
        "command -v swift >/dev/null 2>&1 && command -v swift || "
        "printf 'NO_SWIFT\\n'",
        timeout=60,
    )
    require_zero(result, "establish clean swiftly base")
    before = _result_text(result, "stdout").strip()
    if "NO_SWIFTLY" not in before:
        raise RuntimeError(f"swiftly remains on clean base: {before!r}")
    return before


def provision(sandbox: Any, demand: dict[str, Any], command_timeout: int) -> None:
    """Run toolchain_install before post_acquire and fail on the first error."""
    for command in demand["toolchain_install"]:
        result = sb_run(
            sandbox,
            command,
            "toolchain_install",
            timeout=command_timeout,
            quiet=True,
        )
        require_zero(result, "kit provision toolchain_install")
    for command in demand["post_acquire"]:
        result = sb_run(
            sandbox,
            command,
            "post_acquire",
            timeout=command_timeout,
            quiet=True,
        )
        require_zero(result, "kit provision post_acquire")


def execute_in_sandbox(
    sandbox: Any,
    profile: KitProfile,
    plan: dict[str, Any],
    command_timeout: int,
) -> ProofEvidence:
    staged_via = stage_fixture(sandbox, profile)
    log(f"[ok] repo staged via {staged_via}")
    before = establish_clean_base(sandbox, profile)
    log(f"[info] clean-base tool state: {before}")

    present = set(plan["present_files"])
    manifest = plan["manifest"]
    if not detect_matches(manifest, present):
        raise RuntimeError(f"kit no longer matches staged fixture files {sorted(present)}")
    demand = plan["demand"]
    log(f"[ok] detected kits: {demand['kits']}")
    log(
        f"[ok] composed demand: toolchain_install="
        f"{len(demand['toolchain_install'])} post_acquire="
        f"{len(demand['post_acquire'])}"
    )

    provision(sandbox, demand, command_timeout)

    version = sb_run(
        sandbox,
        profile.version_command,
        "verify-version",
        timeout=command_timeout,
    )
    require_zero(version, f"{profile.name} version verification")
    version_output = _result_text(version, "stdout") + _result_text(version, "stderr")
    validate_version_output(profile, version_output)

    build = sb_run(
        sandbox,
        profile.build_command,
        "verify-build",
        timeout=command_timeout,
    )
    require_zero(build, f"{profile.name} build")

    test = sb_run(
        sandbox,
        profile.test_command,
        "verify-test",
        timeout=command_timeout,
    )
    require_zero(test, f"{profile.name} test")
    test_output = _result_text(test, "stdout") + _result_text(test, "stderr")
    if profile.test_marker not in test_output:
        raise RuntimeError(f"{profile.name} test marker missing")

    return ProofEvidence(
        kit_reference=demand["kits"][0],
        build_step=profile.build_command.split(";")[-1].strip(),
        test_step=profile.test_command.split(";")[-1].strip(),
    )


def render_success_summary(profile: KitProfile, evidence: ProofEvidence) -> None:
    """Emit definitive proof only after fail-closed sandbox teardown succeeds."""
    log("")
    log("=== REDACTED PROOF SUMMARY (mechanism-level, real e2b) ===")
    log("  sandbox:           created and killed (identifier redacted)")
    log(f"  kit:               {evidence.kit_reference}")
    log("  repo staged:       before toolchain provisioning")
    log("  toolchain_install: PASSED")
    log("  post_acquire:      PASSED")
    log(f"  {profile.name} version:  PASSED")
    log(f"  {evidence.build_step}: PASSED")
    log(f"  {evidence.test_step}: PASSED")
    log(f"[ok] KIT TOOLCHAIN E2B PROOF: {profile.name} PASS")


def bounded_int_env(name: str, default: int, maximum: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        value = default
    else:
        try:
            value = int(raw)
        except ValueError as error:
            raise ValueError(f"{name} must be a positive integer") from error
    if value <= 0:
        raise ValueError(f"{name} must be a positive integer")
    if value > maximum:
        raise ValueError(f"{name} must be at most {maximum} seconds")
    return value


def resolve_timeouts(profile: KitProfile) -> tuple[int, int]:
    sandbox_timeout = bounded_int_env(
        "KIT_E2B_TIMEOUT", profile.default_sandbox_timeout, MAX_SANDBOX_TIMEOUT
    )
    command_timeout = bounded_int_env(
        "KIT_E2B_COMMAND_TIMEOUT",
        profile.default_command_timeout,
        MAX_COMMAND_TIMEOUT,
    )
    if command_timeout > sandbox_timeout:
        raise ValueError(
            "KIT_E2B_COMMAND_TIMEOUT must be no greater than KIT_E2B_TIMEOUT"
        )
    return sandbox_timeout, command_timeout


def default_sandbox_factory(timeout: int) -> Any:
    """Import e2b only at the explicit real-cloud boundary."""
    try:
        from e2b import Sandbox
    except ImportError as error:
        raise RuntimeError(
            "e2b SDK not installed; run through kit-toolchain-e2b/run.sh"
        ) from error
    return Sandbox.create(timeout=timeout)


def run_real(
    profile: KitProfile,
    sandbox_factory: Callable[[int], Any] | None = None,
) -> int:
    if not os.environ.get("E2B_API_KEY") and sandbox_factory is None:
        log("[error] E2B_API_KEY not set")
        return 2

    plan = build_plan(profile)
    sandbox_timeout, command_timeout = resolve_timeouts(profile)
    factory = sandbox_factory or default_sandbox_factory
    sandbox = None
    evidence: ProofEvidence | None = None
    cleanup_succeeded = False
    redaction_token: contextvars.Token[tuple[str, ...]] | None = None
    try:
        started = time.monotonic()
        sandbox = factory(sandbox_timeout)
        sandbox_id = str(getattr(sandbox, "sandbox_id", "") or "").strip()
        if sandbox_id:
            redaction_token = _RUN_REDACTIONS.set((sandbox_id,))
        log(
            f"[ok] e2b sandbox created "
            f"({time.monotonic() - started:.1f}s; identifier redacted)"
        )
        evidence = execute_in_sandbox(sandbox, profile, plan, command_timeout)
    except Exception as error:  # noqa: BLE001 - smoke reports provider failures
        log(f"[error] {type(error).__name__}: {error}")
    finally:
        if sandbox is not None:
            try:
                sandbox.kill()
                cleanup_succeeded = True
                log("[info] sandbox killed (identifier redacted)")
            except Exception as error:  # noqa: BLE001 - cleanup must be visible
                log(f"[error] sandbox cleanup failed: {error}")

    try:
        if evidence is not None and cleanup_succeeded:
            render_success_summary(profile, evidence)
            return 0
        log(f"[error] KIT TOOLCHAIN E2B PROOF: {profile.name} FAIL")
        return 1
    finally:
        if redaction_token is not None:
            _RUN_REDACTIONS.reset(redaction_token)


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--kit",
        default=os.environ.get("KIT_TOOLCHAIN_KIT", "ts-next"),
        choices=sorted(PROFILES),
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="validate and print the command plan without importing e2b",
    )
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    profile = PROFILES[args.kit]
    try:
        if args.dry_run:
            return render_dry_run(profile)
        return run_real(profile)
    except (OSError, ValueError) as error:
        log(f"[error] {type(error).__name__}: {error}")
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
