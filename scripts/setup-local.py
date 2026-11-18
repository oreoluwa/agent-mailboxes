#!/usr/bin/env python3
"""Create and operate a persistent, loopback-only Agent Emails installation."""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import stat
import subprocess
import sys


SCHEMA_VERSION = 1
MANIFEST_NAME = "agent-emails-local.json"
MANAGED_FILES = (MANIFEST_NAME, "compose.yml", "emaild.env", "postgres.env", "master.key")


class SetupError(Exception):
    pass


def default_directory() -> Path:
    if sys.platform == "darwin":
        return Path.home() / "Library" / "Application Support" / "agent-emails"
    config_home = os.environ.get("XDG_CONFIG_HOME")
    return Path(config_home) / "agent-emails" if config_home else Path.home() / ".config" / "agent-emails"


def validate_port(value: str) -> int:
    try:
        port = int(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError("port must be an integer") from error
    if not 1 <= port <= 65535:
        raise argparse.ArgumentTypeError("port must be between 1 and 65535")
    return port


def resolve_checkout(value: Path) -> Path:
    checkout = value.expanduser().resolve()
    if not (checkout / "Dockerfile").is_file() or not (checkout / "migrations").is_dir():
        raise SetupError(f"checkout must contain Dockerfile and migrations: {checkout}")
    return checkout


def project_name(directory: Path) -> str:
    suffix = hashlib.sha256(str(directory).encode()).hexdigest()[:12]
    return f"agent-emails-local-{suffix}"


def absolute_directory(value: Path) -> Path:
    return Path(os.path.abspath(value.expanduser()))


def assert_private_directory(directory: Path) -> None:
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode) or directory.is_symlink():
        raise SetupError(f"setup path must be a real directory: {directory}")
    if hasattr(os, "getuid") and info.st_uid != os.getuid():
        raise SetupError(f"setup directory must be owned by the current user: {directory}")
    if stat.S_IMODE(info.st_mode) & 0o077:
        raise SetupError(f"setup directory must not grant group or other access: {directory}")


def private_read(path: Path, limit: int = 65536) -> bytes:
    if not hasattr(os, "O_NOFOLLOW"):
        raise SetupError("secure managed-file loading requires no-follow file support")
    flags = os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_NONBLOCK", 0)
    fd = os.open(path, flags)
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode):
            raise SetupError(f"managed path must be a regular file: {path}")
        if hasattr(os, "getuid") and info.st_uid != os.getuid():
            raise SetupError(f"managed file must be owned by the current user: {path}")
        if stat.S_IMODE(info.st_mode) & 0o077:
            raise SetupError(f"managed file must not grant group or other access: {path}")
        data = source.read(limit + 1)
    if len(data) > limit:
        raise SetupError(f"managed file is unexpectedly large: {path}")
    return data


def private_create(path: Path, data: str) -> None:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as destination:
        destination.write(data)


def load_manifest(directory: Path) -> dict:
    assert_private_directory(directory)
    try:
        manifest = json.loads(private_read(directory / MANIFEST_NAME))
    except FileNotFoundError as error:
        raise SetupError(f"directory is not an Agent Emails local installation: {directory}") from error
    except json.JSONDecodeError as error:
        raise SetupError(f"installation manifest is invalid: {directory / MANIFEST_NAME}") from error
    if manifest.get("schema_version") != SCHEMA_VERSION:
        raise SetupError("unsupported local installation manifest version")
    if manifest.get("directory") != str(directory):
        raise SetupError("installation manifest does not match its directory")
    if manifest.get("project_name") != project_name(directory):
        raise SetupError("installation manifest has an unexpected Compose project identity")
    port = manifest.get("api_port")
    if type(port) is not int or not 1 <= port <= 65535 or manifest.get("api_url") != f"http://127.0.0.1:{port}":
        raise SetupError("installation manifest has an invalid loopback endpoint")
    if manifest.get("managed_files") != list(MANAGED_FILES):
        raise SetupError("installation manifest has an unexpected managed-file set")
    for name in MANAGED_FILES:
        private_read(directory / name)
    return manifest


def compose_source(directory: Path, checkout: Path, api_port: int, project: str) -> str:
    context = json.dumps(str(checkout))
    volume = json.dumps(project + "-postgres-data")
    return f"""services:
  postgres:
    image: pgvector/pgvector:pg16
    restart: unless-stopped
    env_file:
      - postgres.env
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      test: [\"CMD-SHELL\", \"pg_isready -U email -d agent_emails\"]
      interval: 3s
      timeout: 3s
      retries: 30

  emaild:
    build:
      context: {context}
      dockerfile: Dockerfile
      args:
        TARGET: ./cmd/emaild
    restart: unless-stopped
    env_file:
      - emaild.env
    command: [\"/usr/local/bin/app\", \"serve\", \"--all-mailboxes\"]
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - \"127.0.0.1:{api_port}:8787\"
    healthcheck:
      test: [\"CMD\", \"wget\", \"-q\", \"--spider\", \"http://127.0.0.1:8787/onboarding\"]
      interval: 3s
      timeout: 3s
      retries: 30

volumes:
  postgres-data:
    name: {volume}
"""


def init_installation(directory: Path, checkout: Path, api_port: int) -> dict:
    directory = absolute_directory(directory)
    checkout = resolve_checkout(checkout)
    if directory.resolve().is_relative_to(checkout):
        raise SetupError("installation directory must be outside the build checkout")
    created_directory = False
    if directory.exists():
        if any(directory.iterdir()):
            manifest = load_manifest(directory)
            expected = {
                "checkout": str(checkout),
                "api_port": api_port,
            }
            mismatches = [key for key, value in expected.items() if manifest.get(key) != value]
            if mismatches:
                raise SetupError("existing installation differs in: " + ", ".join(mismatches))
            return manifest
        assert_private_directory(directory)
    else:
        directory.mkdir(mode=0o700, parents=True)
        created_directory = True
    os.chmod(directory, 0o700)

    project = project_name(directory)
    database_password = secrets.token_hex(32)
    master_key = secrets.token_hex(32)
    credential_key = base64.b64encode(secrets.token_bytes(32)).decode("ascii")
    manifest = {
        "schema_version": SCHEMA_VERSION,
        "directory": str(directory),
        "checkout": str(checkout),
        "project_name": project,
        "api_port": api_port,
        "api_url": f"http://127.0.0.1:{api_port}",
        "managed_files": list(MANAGED_FILES),
    }
    files = {
        "compose.yml": compose_source(directory, checkout, api_port, project),
        "postgres.env": (
            "POSTGRES_USER=email\n"
            "POSTGRES_DB=agent_emails\n"
            f"POSTGRES_PASSWORD={database_password}\n"
        ),
        "master.key": master_key + "\n",
        "emaild.env": (
            f"POSTGRES_DSN=postgres://email:{database_password}@postgres:5432/agent_emails?sslmode=disable\n"
            f"MASTER_KEY={master_key}\n"
            f"MAILBOX_CREDENTIAL_KEY={credential_key}\n"
            "EMAILD_ENV_FILE=/dev/null\n"
            "EMAILD_LISTEN=0.0.0.0:8787\n"
            "EMAILD_CONNECT_ENABLED=false\n"
            "SUMMARY_ENABLED=false\n"
            "ATTACH_SUMMARY_ENABLED=false\n"
            "INITIAL_SYNC_MAX_MESSAGES=0\n"
            "SYNC_FOLDERS=INBOX\n"
            "CATCHALL_ADMIN=false\n"
            "POLL_INTERVAL_SEC=30\n"
        ),
        MANIFEST_NAME: json.dumps(manifest, indent=2) + "\n",
    }
    created_files: list[Path] = []
    try:
        for name in ("compose.yml", "postgres.env", "emaild.env", "master.key", MANIFEST_NAME):
            path = directory / name
            private_create(path, files[name])
            created_files.append(path)
    except Exception:
        for path in created_files:
            try:
                path.unlink()
            except OSError:
                pass
        if created_directory:
            try:
                directory.rmdir()
            except OSError:
                pass
        raise
    return manifest


def compose_command(directory: Path, action: str) -> int:
    directory = absolute_directory(directory)
    manifest = load_manifest(directory)
    if action == "start" and directory.resolve().is_relative_to(resolve_checkout(Path(manifest["checkout"]))):
        raise SetupError("installation directory must be outside the build checkout")
    base = [
        "docker", "compose", "--env-file", "/dev/null",
        "--project-name", manifest["project_name"],
        "--file", str(directory / "compose.yml"),
    ]
    commands = {
        "start": ["up", "-d", "--build", "--wait"],
        "status": ["ps"],
        "stop": ["stop"],
    }
    return subprocess.run(base + commands[action], cwd=directory, check=False).returncode


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    sub = result.add_subparsers(dest="command", required=True)
    init = sub.add_parser("init", help="create private persistent configuration")
    init.add_argument("directory", nargs="?", type=Path, default=default_directory())
    init.add_argument("--checkout", type=Path, default=Path(__file__).resolve().parents[1])
    init.add_argument("--port", type=validate_port, default=8787)
    for command in ("start", "status", "stop"):
        item = sub.add_parser(command)
        item.add_argument("directory", nargs="?", type=Path, default=default_directory())
    return result


def main() -> int:
    args = parser().parse_args()
    try:
        if args.command == "init":
            manifest = init_installation(args.directory, args.checkout, args.port)
            print(f"Local installation ready: {manifest['directory']}")
            print(f"API: {manifest['api_url']}")
            print("Run setup-local.py start to build and start it. Docker must start at login for automatic restart.")
            return 0
        return compose_command(args.directory, args.command)
    except (OSError, SetupError) as error:
        print(f"setup-local: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
