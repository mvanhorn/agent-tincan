from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import types
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("session-route.py")
session_route = types.ModuleType("session_route")
sys.modules[session_route.__name__] = session_route
exec(compile(SCRIPT.read_text(encoding="utf-8"), str(SCRIPT), "exec"), session_route.__dict__)


SESSION_A = "00000000-0000-4000-8000-000000000001"
SESSION_B = "00000000-0000-4000-8000-000000000002"


class SessionRouteTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=str(Path(tempfile.gettempdir()).resolve()))
        self.root = Path(self.temp.name)
        self.operator_dir = self.root / "operator"
        self.operator_dir.mkdir(mode=0o700)
        self.operator_dir.chmod(0o700)
        self.registry = self.operator_dir / "claude-session-routes.json"
        self.project = self.root / "project"
        self.project.mkdir()

    def tearDown(self):
        self.temp.cleanup()

    def write_registry(self, value: object, *, raw: bytes | None = None, mode: int = 0o600) -> None:
        data = raw if raw is not None else (json.dumps(value) + "\n").encode()
        self.registry.write_bytes(data)
        self.registry.chmod(mode)

    def test_bind_and_resolve_canonical_project_alias(self):
        route, created = session_route.bind_route(self.registry, self.project, "design", SESSION_A)
        alias = self.project / "." / ".." / "project"
        found = session_route.resolve_route(self.registry, alias, "design")
        symlink_alias = self.root / "project-alias"
        symlink_alias.symlink_to(self.project, target_is_directory=True)
        via_symlink = session_route.resolve_route(self.registry, symlink_alias, "design")

        self.assertTrue(created)
        self.assertEqual(route, found)
        self.assertEqual(route, via_symlink)
        self.assertEqual(route["project"], str(self.project.resolve()))
        self.assertEqual(self.registry.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.operator_dir.stat().st_mode & 0o777, 0o700)
        state = json.loads(self.registry.read_text())
        self.assertEqual(state, {"version": 1, "routes": [route]})

    def test_missing_route_does_not_create_registry_or_lock_or_session(self):
        with self.assertRaisesRegex(session_route.RouteError, "route_not_found"):
            session_route.resolve_route(self.registry, self.project, "missing")
        self.assertFalse(self.registry.exists())
        self.assertFalse(self.registry.with_name(self.registry.name + ".lock").exists())

    def test_bind_keeps_registry_and_lock_private_under_restrictive_umask(self):
        previous_umask = os.umask(0o777)
        try:
            session_route.bind_route(self.registry, self.project, "design", SESSION_A)
        finally:
            os.umask(previous_umask)
        self.assertEqual(self.registry.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.registry.with_name(self.registry.name + ".lock").stat().st_mode & 0o777, 0o600)

    def test_project_and_task_keys_are_isolated(self):
        other_project = self.root / "other-project"
        other_project.mkdir()
        session_route.bind_route(self.registry, self.project, "design", SESSION_A)
        session_route.bind_route(self.registry, self.project, "tests", SESSION_B)
        session_route.bind_route(self.registry, other_project, "design", "00000000-0000-4000-8000-000000000003")

        self.assertEqual(session_route.resolve_route(self.registry, self.project, "design")["session_id"], SESSION_A)
        self.assertEqual(session_route.resolve_route(self.registry, self.project, "tests")["session_id"], SESSION_B)
        self.assertEqual(
            session_route.resolve_route(self.registry, other_project, "design")["project"],
            str(other_project.resolve()),
        )

    def test_uuid_input_is_normalized_but_route_remains_immutable(self):
        uppercase = SESSION_A.upper()
        route, created = session_route.bind_route(self.registry, self.project, "design", uppercase)
        same, created_again = session_route.bind_route(self.registry, self.project, "design", SESSION_A)
        self.assertTrue(created)
        self.assertFalse(created_again)
        self.assertEqual(route, same)
        self.assertEqual(SESSION_A, route["session_id"])
        with self.assertRaisesRegex(session_route.RouteError, "route_conflict_immutable"):
            session_route.bind_route(self.registry, self.project, "design", SESSION_B)
        self.assertEqual(session_route.resolve_route(self.registry, self.project, "design"), route)

    def test_one_session_uuid_cannot_be_bound_to_another_route(self):
        session_route.bind_route(self.registry, self.project, "design", SESSION_A)
        with self.assertRaisesRegex(session_route.RouteError, "session_already_bound"):
            session_route.bind_route(self.registry, self.project, "tests", SESSION_A)

    def test_invalid_session_and_task_keys_fail_closed(self):
        for invalid in ("", "../project", "bad task", "x" * 129, "ümlaut"):
            with self.subTest(value=invalid), self.assertRaises(session_route.RouteError):
                session_route.bind_route(self.registry, self.project, invalid, SESSION_A)
        with self.assertRaisesRegex(session_route.RouteError, "invalid_session_uuid"):
            session_route.bind_route(self.registry, self.project, "design", "request-123")

    def test_unknown_version_fields_and_malformed_routes_are_rejected(self):
        malformed_states = (
            {"version": 2, "routes": []},
            {"version": 1, "routes": [], "extra": True},
            {"version": 1, "routes": [{"project": str(self.project), "task": "design",
                                          "session_id": SESSION_A, "extra": "x"}]},
            {"version": 1, "routes": [{"project": str(self.project / ".."), "task": "design",
                                          "session_id": SESSION_A}]},
            {"version": True, "routes": []},
        )
        for state in malformed_states:
            with self.subTest(state=state):
                self.write_registry(state)
                with self.assertRaises(session_route.RouteError):
                    session_route.resolve_route(self.registry, self.project, "design")
                self.registry.unlink()

    def test_duplicate_json_keys_and_duplicate_routes_are_rejected(self):
        self.write_registry({}, raw=b'{"version":1,"version":1,"routes":[]}')
        with self.assertRaisesRegex(session_route.RouteError, "duplicate_registry_key"):
            session_route.resolve_route(self.registry, self.project, "design")

        duplicate = {"version": 1, "routes": [
            {"project": str(self.project), "task": "design", "session_id": SESSION_A},
            {"project": str(self.project), "task": "design", "session_id": SESSION_B},
        ]}
        self.write_registry(duplicate)
        with self.assertRaisesRegex(session_route.RouteError, "duplicate_registry_route"):
            session_route.resolve_route(self.registry, self.project, "design")

    def test_nonregular_or_oversized_registry_is_rejected(self):
        self.write_registry({"version": 1, "routes": []}, mode=0o644)
        with self.assertRaisesRegex(session_route.RouteError, "unsafe_registry_file"):
            session_route.resolve_route(self.registry, self.project, "design")
        self.write_registry({}, raw=b" " * (session_route.MAX_REGISTRY_BYTES + 1))
        with self.assertRaisesRegex(session_route.RouteError, "registry_too_large"):
            session_route.resolve_route(self.registry, self.project, "design")

    def test_symlink_registry_parent_and_lock_are_rejected(self):
        actual = self.operator_dir / "actual.json"
        actual.write_text(json.dumps({"version": 1, "routes": []}))
        actual.chmod(0o600)
        self.registry.symlink_to(actual)
        with self.assertRaisesRegex(session_route.RouteError, "unsafe_registry_file"):
            session_route.resolve_route(self.registry, self.project, "design")
        self.registry.unlink()

        lock = self.registry.with_name(self.registry.name + ".lock")
        lock.symlink_to(actual)
        with self.assertRaises(session_route.RouteError):
            session_route.bind_route(self.registry, self.project, "design", SESSION_A)
        lock.unlink()

        parent_link = self.root / "operator-link"
        parent_link.symlink_to(self.operator_dir, target_is_directory=True)
        linked_registry = parent_link / self.registry.name
        with self.assertRaisesRegex(session_route.RouteError, "unsafe_registry_directory"):
            session_route.resolve_route(linked_registry, self.project, "design")

    def test_fifo_registry_is_rejected_without_waiting_for_writer(self):
        os.mkfifo(self.registry, 0o600)
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "--registry", str(self.registry),
             "--project", str(self.project), "--task", "design"],
            capture_output=True, text=True, timeout=3, check=False,
        )
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stderr.strip(), "unsafe_registry_file")

    def test_nonprivate_registry_directory_and_lock_are_rejected(self):
        self.operator_dir.chmod(0o755)
        with self.assertRaisesRegex(session_route.RouteError, "unsafe_registry_directory"):
            session_route.bind_route(self.registry, self.project, "design", SESSION_A)
        self.operator_dir.chmod(0o700)
        lock = self.registry.with_name(self.registry.name + ".lock")
        lock.write_text("")
        lock.chmod(0o644)
        with self.assertRaisesRegex(session_route.RouteError, "unsafe_registry_lock"):
            session_route.bind_route(self.registry, self.project, "design", SESSION_A)

    def test_registry_owner_is_checked(self):
        session_route.bind_route(self.registry, self.project, "design", SESSION_A)
        with patch.object(session_route.os, "getuid", return_value=os.getuid() + 1):
            with self.assertRaises(session_route.RouteError):
                session_route.resolve_route(self.registry, self.project, "design")

    def test_concurrent_conflicting_binds_have_one_immutable_winner(self):
        command = [
            sys.executable, str(SCRIPT), "--registry", str(self.registry),
            "--project", str(self.project), "--task", "design",
        ]
        first = subprocess.Popen(command + ["--bind", SESSION_A], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        second = subprocess.Popen(command + ["--bind", SESSION_B], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        first_out, first_err = first.communicate(timeout=10)
        second_out, second_err = second.communicate(timeout=10)

        self.assertEqual(sorted((first.returncode, second.returncode)), [0, 2],
                         (first_out, first_err, second_out, second_err))
        resolved = session_route.resolve_route(self.registry, self.project, "design")
        self.assertIn(resolved["session_id"], {SESSION_A, SESSION_B})
        self.assertEqual(len(json.loads(self.registry.read_text())["routes"]), 1)

    def test_cli_bind_and_resolve_emit_machine_readable_route(self):
        output = subprocess.run(
            [sys.executable, str(SCRIPT), "--registry", str(self.registry), "--project", str(self.project),
             "--task", "design", "--bind", SESSION_A],
            capture_output=True, text=True, timeout=10, check=False,
        )
        self.assertEqual(output.returncode, 0, output.stderr)
        self.assertEqual(json.loads(output.stdout), {
            "action": "bound", "project": str(self.project.resolve()), "task": "design", "session_id": SESSION_A,
        })
        resolved = subprocess.run(
            [sys.executable, str(SCRIPT), "--registry", str(self.registry), "--project", str(self.project),
             "--task", "design"],
            capture_output=True, text=True, timeout=10, check=False,
        )
        self.assertEqual(resolved.returncode, 0, resolved.stderr)
        self.assertEqual(json.loads(resolved.stdout)["action"], "resolved")

    def test_desktop_opener_uses_pty_exact_uuid_and_project_directory(self):
        fake_claude = self.root / "fake-claude"
        capture = self.root / "opener.json"
        fake_claude.write_text(
            "#!/bin/sh\n"
            "printf '{\\\"cwd\\\":\\\"%s\\\",\\\"argc\\\":%s,\\\"a0\\\":\\\"%s\\\",\\\"a1\\\":\\\"%s\\\",\\\"a2\\\":\\\"%s\\\"}\\n' "
            "\"$(pwd -P)\" \"$#\" \"$1\" \"$2\" \"$3\" > \"$ROUTE_TEST_CAPTURE\"\n"
        )
        fake_claude.chmod(0o700)
        result = session_route.open_desktop(
            self.project, SESSION_A, claude_bin=fake_claude, timeout=5,
            env={"PATH": os.defpath, "HOME": str(self.root), "ROUTE_TEST_CAPTURE": str(capture)},
        )
        self.assertEqual(result, 0)
        value = json.loads(capture.read_text())
        self.assertEqual(value, {
            "cwd": str(self.project.resolve()), "argc": 3,
            "a0": "--desktop", "a1": "--resume", "a2": SESSION_A,
        })

    def test_desktop_opener_timeout_is_bounded_and_reported_safely(self):
        fake_claude = self.root / "fake-slow-claude"
        fake_claude.write_text("#!/bin/sh\nsleep 10\n")
        fake_claude.chmod(0o700)
        started = time.monotonic()
        with self.assertRaisesRegex(session_route.RouteError, "desktop_opener_timeout"):
            session_route.open_desktop(self.project, SESSION_A, claude_bin=fake_claude, timeout=1,
                                       env={"PATH": os.defpath, "HOME": str(self.root)})
        self.assertLess(time.monotonic() - started, 4)

    def test_noisy_desktop_opener_cannot_starve_timeout(self):
        fake_claude = self.root / "fake-noisy-claude"
        fake_claude.write_text("#!/bin/sh\nwhile :; do printf xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; done\n")
        fake_claude.chmod(0o700)
        started = time.monotonic()
        with self.assertRaisesRegex(session_route.RouteError, "desktop_opener_timeout"):
            session_route.open_desktop(self.project, SESSION_A, claude_bin=fake_claude, timeout=1,
                                       env={"PATH": os.defpath, "HOME": str(self.root)})
        self.assertLess(time.monotonic() - started, 4)

    def test_desktop_opener_failure_never_returns_terminal_output(self):
        fake_claude = self.root / "fake-failing-claude"
        fake_claude.write_text("#!/bin/sh\nprintf 'PRIVATE_TRANSCRIPT_SHOULD_NOT_ESCAPE'\nexit 23\n")
        fake_claude.chmod(0o700)
        with self.assertRaisesRegex(session_route.RouteError, "desktop_opener_failed") as error:
            session_route.open_desktop(self.project, SESSION_A, claude_bin=fake_claude, timeout=5,
                                       env={"PATH": os.defpath, "HOME": str(self.root)})
        self.assertNotIn("PRIVATE_TRANSCRIPT", str(error.exception))


if __name__ == "__main__":
    unittest.main()
