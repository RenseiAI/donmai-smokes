import contextlib
import io
import os
import unittest
from unittest import mock

import kit_toolchain_e2b as harness


SWIFTLY_INSTALL = (
    "curl -fsSL https://download.swift.org/swiftly/linux/"
    "swiftly-$(uname -m).tar.gz -o /tmp/swiftly.tgz && "
    "tar -xzf /tmp/swiftly.tgz -C /tmp && "
    "/tmp/swiftly init --assume-yes --skip-install --no-modify-profile && "
    '. "$HOME/.local/share/swiftly/env.sh" && '
    "swiftly install 6.0.3 && swiftly use 6.0.3"
)


class FakeSandbox:
    def __init__(self, kill_error=None):
        self.kill_calls = 0
        self.kill_error = kill_error

    def kill(self):
        self.kill_calls += 1
        if self.kill_error is not None:
            raise self.kill_error


class FakeResult:
    exit_code = 0
    stdout = ""
    stderr = ""


class RecordingCommands:
    def __init__(self):
        self.calls = []

    def run(self, command, timeout):
        self.calls.append((command, timeout))
        return FakeResult()


class RecordingFiles:
    def __init__(self):
        self.writes = []

    def write(self, path, contents):
        self.writes.append((path, contents))


class RecordingSandbox(FakeSandbox):
    def __init__(self):
        super().__init__()
        self.commands = RecordingCommands()
        self.files = RecordingFiles()


class ManifestPipelineTests(unittest.TestCase):
    def test_swift_manifest_detects_and_composes_published_toolchain(self):
        profile = harness.PROFILES["swift"]
        plan = harness.build_plan(profile)

        self.assertIn("Package.swift", plan["present_files"])
        self.assertEqual(plan["demand"]["kits"], ["default/swift@1.0.0"])
        self.assertEqual(plan["demand"]["toolchain_install"], [SWIFTLY_INSTALL])
        self.assertEqual(len(plan["demand"]["post_acquire"]), 1)
        self.assertIn("swift package resolve", plan["demand"]["post_acquire"][0])

    def test_swift_verify_commands_re_source_swiftly_environment(self):
        profile = harness.PROFILES["swift"]
        commands = [profile.version_command, profile.build_command, profile.test_command]

        for command in commands:
            with self.subTest(command=command):
                self.assertIn(".local/share/swiftly/env.sh", command)
                self.assertIn(".swiftly/env.sh", command)
        self.assertTrue(profile.build_command.endswith("swift build"))
        self.assertTrue(profile.test_command.endswith("swift test"))

    def test_swift_version_must_come_from_swiftly_toolchain(self):
        profile = harness.PROFILES["swift"]
        harness.validate_version_output(
            profile,
            "swiftly=/home/user/.local/share/swiftly/bin/swiftly\n"
            "swift=/home/user/.local/share/swiftly/bin/swift\n"
            "Swift version 6.0.3 (swift-6.0.3-RELEASE)\n",
        )

        with self.assertRaises(RuntimeError):
            harness.validate_version_output(
                profile,
                "swiftly=/home/user/.local/share/swiftly/bin/swiftly\n"
                "swift=/usr/bin/swift\n"
                "Swift version 6.0.3 (swift-6.0.3-RELEASE)\n",
            )

    def test_swift_fixture_is_dependency_free_and_has_real_test_marker(self):
        profile = harness.PROFILES["swift"]
        files = harness.fixture_relative_files(profile)
        package = (harness.fixture_root(profile) / "Package.swift").read_text()
        test = (
            harness.fixture_root(profile)
            / "Tests/SwiftKitFixtureTests/CalcTests.swift"
        ).read_text()

        self.assertEqual(
            files,
            [
                "Package.swift",
                "Sources/SwiftKitFixture/Calc.swift",
                "Tests/SwiftKitFixtureTests/CalcTests.swift",
            ],
        )
        self.assertNotIn(".package(", package)
        self.assertIn("KIT_TOOLCHAIN_SWIFT_TEST_OK", test)

    def test_ts_next_default_still_detects_and_composes(self):
        profile = harness.PROFILES["ts-next"]
        plan = harness.build_plan(profile)

        self.assertIn("package.json", plan["present_files"])
        self.assertEqual(plan["demand"]["kits"], ["ts-next@0.1.0"])
        self.assertEqual(profile.build_command, "npm run build")
        self.assertEqual(profile.test_command, "npm test")

    def test_detect_requires_positive_match_and_honors_exclusion(self):
        manifest = {"detect": {"files": ["Package.swift"], "not_files": ["skip"]}}

        self.assertTrue(harness.detect_matches(manifest, {"Package.swift"}))
        self.assertFalse(harness.detect_matches(manifest, {"Package.swift", "skip"}))
        self.assertFalse(harness.detect_matches({"detect": {}}, {"Package.swift"}))


class DryRunAndSafetyTests(unittest.TestCase):
    def test_dry_run_prints_exact_swift_plan_without_factory(self):
        output = io.StringIO()
        with mock.patch.object(harness, "default_sandbox_factory") as factory:
            with contextlib.redirect_stdout(output):
                result = harness.render_dry_run(harness.PROFILES["swift"])

        self.assertEqual(result, 0)
        factory.assert_not_called()
        rendered = output.getvalue()
        self.assertIn("no sandbox created", rendered)
        self.assertIn("swiftly install 6.0.3", rendered)
        self.assertIn("verify_build=", rendered)
        self.assertIn("swift build", rendered)
        self.assertIn("swift test", rendered)
        self.assertIn("KIT TOOLCHAIN DRY RUN: swift PASS", rendered)

    def test_redaction_removes_known_secret_and_url_userinfo(self):
        rendered = harness.redact_text(
            "key=super-secret https://user:password@example.com/path",
            ["super-secret"],
        )

        self.assertNotIn("super-secret", rendered)
        self.assertNotIn("user:password", rendered)
        self.assertEqual(
            rendered, "key=<redacted> https://<redacted>@example.com/path"
        )

    def test_positive_timeout_override(self):
        with mock.patch.dict(os.environ, {"KIT_E2B_TIMEOUT": "123"}, clear=False):
            self.assertEqual(harness.positive_int_env("KIT_E2B_TIMEOUT", 600), 123)

    def test_invalid_timeout_fails_closed(self):
        for value in ("0", "-1", "not-a-number"):
            with self.subTest(value=value):
                with mock.patch.dict(
                    os.environ, {"KIT_E2B_TIMEOUT": value}, clear=False
                ):
                    with self.assertRaises(ValueError):
                        harness.positive_int_env("KIT_E2B_TIMEOUT", 600)

    def test_swift_fixture_staging_is_local_and_deterministic(self):
        sandbox = RecordingSandbox()
        with mock.patch.dict(
            os.environ,
            {"TS_FIXTURE_REPO": "https://user:password@example.com/private.git"},
            clear=False,
        ):
            staged_via = harness.stage_fixture(sandbox, harness.PROFILES["swift"])

        self.assertEqual(staged_via, "uploaded deterministic local fixture")
        self.assertEqual(len(sandbox.commands.calls), 1)
        self.assertNotIn("git clone", sandbox.commands.calls[0][0])
        self.assertEqual(len(sandbox.files.writes), 3)

    def test_quiet_command_wrapper_is_bounded_and_shell_safe(self):
        sandbox = RecordingSandbox()
        with contextlib.redirect_stdout(io.StringIO()):
            harness.sb_run(
                sandbox,
                "swift build",
                "verify-build",
                timeout=321,
                quiet=True,
            )

        self.assertEqual(len(sandbox.commands.calls), 1)
        command, timeout = sandbox.commands.calls[0]
        self.assertEqual(timeout, 321)
        self.assertIn("cd '/home/user/repo'", command)
        self.assertIn('printf "%s\\n" "--- exit $rc (tail) ---"', command)
        self.assertIn("exit $rc", command)

    def test_provision_runs_toolchain_before_post_acquire(self):
        demand = {
            "toolchain_install": ["install-one", "install-two"],
            "post_acquire": ["post-one"],
        }
        with mock.patch.object(
            harness, "sb_run", return_value=FakeResult()
        ) as run_command:
            harness.provision(RecordingSandbox(), demand, 222)

        self.assertEqual(
            [(call.args[1], call.args[2]) for call in run_command.call_args_list],
            [
                ("install-one", "toolchain_install"),
                ("install-two", "toolchain_install"),
                ("post-one", "post_acquire"),
            ],
        )

    def test_real_runner_always_kills_sandbox_after_failure(self):
        sandbox = FakeSandbox()
        output = io.StringIO()
        with mock.patch.object(
            harness, "execute_in_sandbox", side_effect=RuntimeError("boom")
        ):
            with contextlib.redirect_stdout(output):
                result = harness.run_real(
                    harness.PROFILES["swift"], lambda _timeout: sandbox
                )

        self.assertEqual(result, 1)
        self.assertEqual(sandbox.kill_calls, 1)
        self.assertIn("sandbox killed", output.getvalue())

    def test_cleanup_failure_turns_success_into_failure(self):
        sandbox = FakeSandbox(kill_error=RuntimeError("cleanup boom"))
        output = io.StringIO()
        with mock.patch.object(harness, "execute_in_sandbox", return_value=None):
            with contextlib.redirect_stdout(output):
                result = harness.run_real(
                    harness.PROFILES["swift"], lambda _timeout: sandbox
                )

        self.assertEqual(result, 1)
        self.assertEqual(sandbox.kill_calls, 1)
        self.assertIn("sandbox cleanup failed", output.getvalue())


if __name__ == "__main__":
    unittest.main()
