#!/usr/bin/env python3
"""Resolve an operator-bound Claude Code session by project and task key.

This helper is local operator tooling, not an automatic Tincan router. A
trusted operator binds an already-existing Claude conversation UUID once; the
helper never discovers or creates conversations, reads prompts, claims inbox
items, or sends a task to Claude. Do not pass peer-selected paths or task keys.
"""
from __future__ import annotations

import argparse
from contextlib import contextmanager
import fcntl
import json
import os
from pathlib import Path
import pty
import secrets
import select
import shutil
import signal
import stat
import sys
import time
import uuid


DEFAULT_REGISTRY = Path.home() / ".config" / "tincan" / "claude-session-routes.json"
REGISTRY_VERSION = 1
MAX_REGISTRY_BYTES = 1_048_576
MAX_ROUTES = 512
MAX_PROJECT_LENGTH = 4096
MAX_REGISTRY_NAME_LENGTH = 200
DEFAULT_OPENER_TIMEOUT = 15
MAX_OPENER_TIMEOUT = 60


class RouteError(RuntimeError):
    """A safe, non-content-bearing failure code for registry operations."""


def _task_key(value: object) -> str:
    if (not isinstance(value, str) or not value or len(value) > 128
            or not value[0].isascii() or not value[0].isalnum()
            or not value.isascii()
            or any(not (char.isalnum() or char in "._:-") for char in value)):
        raise RouteError("invalid_task_key")
    return value


def _session_uuid(value: object) -> str:
    if not isinstance(value, str) or not 1 <= len(value) <= 64:
        raise RouteError("invalid_session_uuid")
    try:
        return str(uuid.UUID(value))
    except (ValueError, AttributeError, TypeError):
        raise RouteError("invalid_session_uuid") from None


def _project_directory(value: Path) -> Path:
    try:
        project = value.expanduser().resolve(strict=True)
        info = project.stat()
    except (OSError, RuntimeError, ValueError):
        raise RouteError("project_directory_unavailable") from None
    if not stat.S_ISDIR(info.st_mode) or len(str(project)) > MAX_PROJECT_LENGTH:
        raise RouteError("project_directory_unavailable")
    return project


def _duplicate_rejecting_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise RouteError("duplicate_registry_key")
        result[key] = value
    return result


def _registry_directory(registry: Path) -> tuple[Path, str]:
    try:
        expanded = registry.expanduser()
        if not expanded.is_absolute():
            raise RouteError("registry_path_must_be_absolute")
        parent = Path(os.path.abspath(expanded.parent))
        name = expanded.name
        if (not name or name in {".", ".."}
                or len(name) > MAX_REGISTRY_NAME_LENGTH
                or "/" in name or "\x00" in name):
            raise RouteError("unsafe_registry_path")
        resolved_parent = parent.resolve(strict=True)
        if resolved_parent != parent:
            raise RouteError("unsafe_registry_directory")
        info = parent.lstat()
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) & 0o077
                or not os.access(parent, os.W_OK | os.X_OK)):
            raise RouteError("unsafe_registry_directory")
        return parent, name
    except RouteError:
        raise
    except (OSError, RuntimeError, ValueError):
        raise RouteError("unsafe_registry_directory") from None


@contextmanager
def _open_registry_directory(registry: Path):
    parent, name = _registry_directory(registry)
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_CLOEXEC", 0)
    if not hasattr(os, "O_NOFOLLOW"):
        raise RouteError("safe_open_unavailable")
    try:
        directory_fd = os.open(parent, flags | os.O_NOFOLLOW)
    except OSError:
        raise RouteError("unsafe_registry_directory") from None
    try:
        info = os.fstat(directory_fd)
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) & 0o077):
            raise RouteError("unsafe_registry_directory")
        yield directory_fd, name
    finally:
        os.close(directory_fd)


def _open_private_regular(directory_fd: int, name: str, flags: int,
                          error_code: str = "unsafe_registry_file") -> int:
    try:
        # Reject FIFOs after open without blocking on a writer. O_NONBLOCK does
        # not change normal regular-file reads, and fstat still checks the type.
        fd = os.open(name, flags | os.O_NOFOLLOW | os.O_NONBLOCK
                     | getattr(os, "O_CLOEXEC", 0), dir_fd=directory_fd)
    except OSError:
        raise RouteError(error_code) from None
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1):
            raise RouteError(error_code)
        return fd
    except BaseException:
        os.close(fd)
        raise


def _registry_state(directory_fd: int, name: str, *, missing_ok: bool) -> dict[str, object] | None:
    try:
        fd = _open_private_regular(directory_fd, name, os.O_RDONLY)
    except RouteError:
        try:
            os.stat(name, dir_fd=directory_fd, follow_symlinks=False)
        except FileNotFoundError:
            if missing_ok:
                return None
        except OSError:
            pass
        raise
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if info.st_size > MAX_REGISTRY_BYTES:
            raise RouteError("registry_too_large")
        raw = stream.read(MAX_REGISTRY_BYTES + 1)
    if len(raw) > MAX_REGISTRY_BYTES:
        raise RouteError("registry_too_large")
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_duplicate_rejecting_object)
    except RouteError:
        raise
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise RouteError("malformed_registry") from None
    return _validate_state(value)


def _validate_state(value: object) -> dict[str, object]:
    if (not isinstance(value, dict) or set(value) != {"version", "routes"}
            or type(value.get("version")) is not int
            or value["version"] != REGISTRY_VERSION
            or not isinstance(value.get("routes"), list)
            or len(value["routes"]) > MAX_ROUTES):
        raise RouteError("unsupported_registry_format")

    routes: list[dict[str, str]] = []
    identities: set[tuple[str, str]] = set()
    sessions: set[str] = set()
    for route in value["routes"]:
        if not isinstance(route, dict) or set(route) != {"project", "task", "session_id"}:
            raise RouteError("malformed_registry_route")
        project = route["project"]
        task = _task_key(route["task"])
        session_id = route["session_id"]
        if (not isinstance(project, str) or not project or len(project) > MAX_PROJECT_LENGTH
                or "\x00" in project
                or not Path(project).is_absolute() or os.path.normpath(project) != project):
            raise RouteError("malformed_registry_route")
        normalized_session = _session_uuid(session_id)
        if normalized_session != session_id:
            raise RouteError("malformed_registry_route")
        identity = (project, task)
        if identity in identities or session_id in sessions:
            raise RouteError("duplicate_registry_route")
        identities.add(identity)
        sessions.add(session_id)
        routes.append({"project": project, "task": task, "session_id": session_id})
    return {"version": REGISTRY_VERSION, "routes": routes}


@contextmanager
def _registry_lock(directory_fd: int, name: str):
    lock_name = name + ".lock"
    flags = os.O_RDWR | os.O_CREAT | os.O_EXCL | getattr(os, "O_CLOEXEC", 0) | os.O_NOFOLLOW
    fd: int | None = None
    try:
        try:
            fd = os.open(lock_name, flags, 0o600, dir_fd=directory_fd)
        except FileExistsError:
            fd = _open_private_regular(directory_fd, lock_name, os.O_RDWR, "unsafe_registry_lock")
        else:
            os.fchmod(fd, 0o600)
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                    or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1):
                raise RouteError("unsafe_registry_lock")
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1):
            raise RouteError("unsafe_registry_lock")
        fcntl.flock(fd, fcntl.LOCK_EX)
    except RouteError:
        if fd is not None:
            os.close(fd)
        raise
    except OSError:
        if fd is not None:
            os.close(fd)
        raise RouteError("registry_lock_unavailable") from None
    assert fd is not None
    try:
        yield
    finally:
        try:
            fcntl.flock(fd, fcntl.LOCK_UN)
        finally:
            os.close(fd)


def _atomic_write(directory_fd: int, name: str, state: dict[str, object]) -> None:
    payload = (json.dumps(state, ensure_ascii=True, sort_keys=True, indent=2) + "\n").encode("utf-8")
    if len(payload) > MAX_REGISTRY_BYTES:
        raise RouteError("registry_too_large")
    temp_name = f".{name}.{os.getpid()}.{secrets.token_hex(8)}.tmp"
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(temp_name, flags, 0o600, dir_fd=directory_fd)
    except OSError:
        raise RouteError("registry_write_unavailable") from None
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp_name, name, src_dir_fd=directory_fd, dst_dir_fd=directory_fd)
        os.fsync(directory_fd)
    except OSError:
        try:
            os.unlink(temp_name, dir_fd=directory_fd)
        except OSError:
            pass
        raise RouteError("registry_write_unavailable") from None


def bind_route(registry: Path, project: Path, task: str, session_id: str) -> tuple[dict[str, str], bool]:
    """Bind an existing UUID once; return (route, created)."""
    canonical_project = str(_project_directory(project))
    task = _task_key(task)
    session_id = _session_uuid(session_id)
    route = {"project": canonical_project, "task": task, "session_id": session_id}
    with _open_registry_directory(registry) as (directory_fd, name):
        with _registry_lock(directory_fd, name):
            state = _registry_state(directory_fd, name, missing_ok=True)
            if state is None:
                state = {"version": REGISTRY_VERSION, "routes": []}
            routes = state["routes"]
            for existing in routes:
                if existing["project"] == canonical_project and existing["task"] == task:
                    if existing["session_id"] == session_id:
                        return existing, False
                    raise RouteError("route_conflict_immutable")
                if existing["session_id"] == session_id:
                    raise RouteError("session_already_bound")
            if len(routes) >= MAX_ROUTES:
                raise RouteError("registry_route_limit")
            routes.append(route)
            state = _validate_state(state)
            _atomic_write(directory_fd, name, state)
    return route, True


def resolve_route(registry: Path, project: Path, task: str) -> dict[str, str]:
    canonical_project = str(_project_directory(project))
    task = _task_key(task)
    with _open_registry_directory(registry) as (directory_fd, name):
        state = _registry_state(directory_fd, name, missing_ok=True)
    if state is None:
        raise RouteError("route_not_found")
    for route in state["routes"]:
        if route["project"] == canonical_project and route["task"] == task:
            return route
    raise RouteError("route_not_found")


def _desktop_environment() -> dict[str, str]:
    allowed = {
        "HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM",
    }
    env = {key: value for key, value in os.environ.items() if key in allowed}
    env.setdefault("HOME", str(Path.home()))
    env.setdefault("PATH", os.defpath)
    env.setdefault("TERM", "xterm-256color")
    return env


def _executable(path: Path | None) -> Path:
    value = shutil.which("claude") if path is None else str(path.expanduser())
    if not value:
        raise RouteError("claude_cli_unavailable")
    try:
        resolved = Path(value).resolve(strict=True)
        info = resolved.stat()
    except (OSError, RuntimeError):
        raise RouteError("claude_cli_unavailable") from None
    if not stat.S_ISREG(info.st_mode) or not os.access(resolved, os.X_OK):
        raise RouteError("claude_cli_unavailable")
    return resolved


def _drain_pty(master_fd: int) -> None:
    """Discard terminal output; Claude transcript text must never be captured."""
    # A child that continuously writes must not keep this loop from returning
    # to the outer deadline check.
    for _ in range(64):
        try:
            chunk = os.read(master_fd, 4096)
        except (BlockingIOError, InterruptedError):
            return
        except OSError:
            return
        if not chunk:
            return


def _stop_child(child_pid: int) -> None:
    try:
        os.killpg(child_pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    except OSError:
        try:
            os.kill(child_pid, signal.SIGTERM)
        except OSError:
            pass
    deadline = time.monotonic() + 0.5
    while time.monotonic() < deadline:
        try:
            waited, _ = os.waitpid(child_pid, os.WNOHANG)
        except ChildProcessError:
            return
        if waited:
            return
        time.sleep(0.02)
    try:
        os.killpg(child_pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    except OSError:
        try:
            os.kill(child_pid, signal.SIGKILL)
        except OSError:
            pass
    try:
        os.waitpid(child_pid, 0)
    except ChildProcessError:
        pass


def open_desktop(project: Path, session_id: str, *, claude_bin: Path | None = None,
                 timeout: float = DEFAULT_OPENER_TIMEOUT,
                 env: dict[str, str] | None = None) -> int:
    """Open one already-bound session via Claude's interactive PTY handoff."""
    project = _project_directory(project)
    session_id = _session_uuid(session_id)
    if (not isinstance(timeout, (int, float)) or isinstance(timeout, bool)
            or not 1 <= timeout <= MAX_OPENER_TIMEOUT):
        raise RouteError("invalid_opener_timeout")
    executable = _executable(claude_bin)
    child_env = _desktop_environment() if env is None else dict(env)
    try:
        child_pid, master_fd = pty.fork()
    except OSError:
        raise RouteError("desktop_opener_unavailable") from None
    if child_pid == 0:
        try:
            os.chdir(project)
            os.execve(executable, [str(executable), "--desktop", "--resume", session_id], child_env)
        except BaseException:
            os._exit(127)
    os.set_blocking(master_fd, False)
    deadline = time.monotonic() + timeout
    status: int | None = None
    try:
        while time.monotonic() < deadline:
            try:
                waited, child_status = os.waitpid(child_pid, os.WNOHANG)
            except ChildProcessError:
                raise RouteError("desktop_opener_failed") from None
            if waited:
                status = child_status
                _drain_pty(master_fd)
                break
            try:
                ready, _, _ = select.select(
                    [master_fd], [], [], min(0.1, max(0, deadline - time.monotonic()))
                )
            except InterruptedError:
                continue
            if ready:
                _drain_pty(master_fd)
        if status is None:
            _stop_child(child_pid)
            raise RouteError("desktop_opener_timeout")
    finally:
        os.close(master_fd)
    if not os.WIFEXITED(status) or os.WEXITSTATUS(status) != 0:
        raise RouteError("desktop_opener_failed")
    return os.WEXITSTATUS(status)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Resolve an existing Claude Code session by a locally bound project/task route.",
        epilog=("Operator-only: project paths, task keys, and UUID bindings must be selected by a trusted "
                "local operator, never supplied by peer request text. This is not an automatic router "
                "or Tincan wake dispatcher. --desktop only opens the exact resolved session; it sends "
                "no prompt and does not claim inbox work."),
    )
    parser.add_argument("--registry", type=Path, default=DEFAULT_REGISTRY,
                        help="route JSON in an existing owner-private 0700 directory")
    parser.add_argument("--project", type=Path, required=True,
                        help="existing project directory; canonicalized before lookup")
    parser.add_argument("--task", required=True, help="explicit bounded operator task key, not a prompt")
    parser.add_argument("--bind", metavar="UUID",
                        help="bind this already-existing Claude conversation UUID once; never rebind")
    parser.add_argument("--desktop", action="store_true",
                        help="open the exact resolved UUID with claude --desktop --resume via a PTY")
    parser.add_argument("--claude-bin", type=Path,
                        help=argparse.SUPPRESS)
    parser.add_argument("--opener-timeout", type=float, default=DEFAULT_OPENER_TIMEOUT,
                        help=f"bound the Desktop opener to 1-{MAX_OPENER_TIMEOUT} seconds (default {DEFAULT_OPENER_TIMEOUT})")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.bind is not None:
            route, created = bind_route(args.registry, args.project, args.task, args.bind)
            action = "bound" if created else "already_bound"
        else:
            route = resolve_route(args.registry, args.project, args.task)
            action = "resolved"
        if args.desktop:
            open_desktop(args.project, route["session_id"], claude_bin=args.claude_bin,
                         timeout=args.opener_timeout)
            action = "desktop_open_requested"
        print(json.dumps({"action": action, **route}, ensure_ascii=True, sort_keys=True))
        return 0
    except RouteError as exc:
        print(str(exc), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
