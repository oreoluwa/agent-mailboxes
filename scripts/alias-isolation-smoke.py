#!/usr/bin/env python3
"""Run an isolated, provider-free smoke test for two alias-scoped agents."""

from __future__ import annotations

import argparse
import atexit
import base64
import contextlib
import email.policy
import imaplib
import importlib.util
import io
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from email.message import EmailMessage
from email.parser import BytesParser
from email.utils import formatdate, make_msgid, parseaddr
from pathlib import Path
from typing import Any
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
MAILBOX_NAME = "primary"
MAILBOX_ADDRESS = "primary@example.com"
MAILBOX_USERNAME = "primary"
MAILBOX_PASSWORD = "app-password"
RESEARCH_ALIAS = "primary+research@example.com"
ASSISTANT_ALIAS = "primary+assistant@example.com"


class SmokeFailure(RuntimeError):
    pass


class APIError(SmokeFailure):
    def __init__(self, status: int, body: bytes, url: str):
        text = body.decode("utf-8", "replace").strip()
        super().__init__(f"API {status} for {url}: {text}")
        self.status = status
        self.body = body


def run(
    args: list[str],
    *,
    cwd: Path = ROOT,
    env: dict[str, str] | None = None,
    capture: bool = False,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        args,
        cwd=cwd,
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.PIPE if capture else None,
    )


def require_commands(*commands: str) -> None:
    missing = [command for command in commands if shutil.which(command) is None]
    if missing:
        raise SmokeFailure("missing required commands: " + ", ".join(missing))


def free_local_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def wait_for_socket(port: int, label: str, timeout: float = 60) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                return
        except OSError:
            time.sleep(0.5)
    raise SmokeFailure(f"{label} did not become ready on port {port}")


def docker_port(container: str, private_port: int) -> int:
    output = run(
        ["docker", "port", container, f"{private_port}/tcp"], capture=True
    ).stdout.strip()
    if not output:
        raise SmokeFailure(f"docker did not publish {container}:{private_port}")
    return int(output.splitlines()[0].rsplit(":", 1)[1])


def api_request(
    base_url: str,
    method: str,
    path: str,
    *,
    token: str | None = None,
    master_key: str | None = None,
    payload: Any | None = None,
    expected: tuple[int, ...] = (200,),
) -> tuple[int, bytes]:
    headers: dict[str, str] = {}
    body = None
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if master_key:
        headers["X-Master-Key"] = master_key
    if payload is not None:
        headers["Content-Type"] = "application/json"
        body = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(base_url + path, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            status = response.status
            response_body = response.read()
    except urllib.error.HTTPError as exc:
        status = exc.code
        response_body = exc.read()
    except urllib.error.URLError as exc:
        raise SmokeFailure(f"API request failed for {path}: {exc}") from exc
    if status not in expected:
        raise APIError(status, response_body, base_url + path)
    return status, response_body


def api_json(*args: Any, **kwargs: Any) -> Any:
    _, body = api_request(*args, **kwargs)
    try:
        return json.loads(body)
    except json.JSONDecodeError as exc:
        raise SmokeFailure(f"API returned invalid JSON: {body[:500]!r}") from exc


def mailbox_path(path: str) -> str:
    separator = "&" if "?" in path else "?"
    return path + separator + urllib.parse.urlencode({"mailbox": MAILBOX_NAME})


def wait_for_http(base_url: str, process: subprocess.Popen[bytes], timeout: float = 60) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise SmokeFailure(f"emaild exited early with status {process.returncode}")
        try:
            api_request(base_url, "GET", "/onboarding")
            return
        except SmokeFailure:
            time.sleep(0.5)
    raise SmokeFailure("emaild did not become ready")


def make_message(subject: str, sender: str, recipient: str, body: str, **headers: str) -> bytes:
    message = EmailMessage(policy=email.policy.SMTP)
    message["From"] = sender
    message["To"] = recipient
    message["Subject"] = subject
    message["Date"] = formatdate(localtime=False)
    message["Message-ID"] = make_msgid(domain="smoke.invalid")
    for name, value in headers.items():
        message[name.replace("_", "-")] = value
    message.set_content(body)
    return message.as_bytes()


def append_messages(imap_port: int, messages: list[bytes]) -> None:
    client = imaplib.IMAP4("127.0.0.1", imap_port)
    try:
        client.login(MAILBOX_USERNAME, MAILBOX_PASSWORD)
        status, _ = client.select("INBOX")
        if status != "OK":
            raise SmokeFailure("could not select GreenMail INBOX")
        internal_date = imaplib.Time2Internaldate(time.time())
        for message in messages:
            status, detail = client.append("INBOX", None, internal_date, message)
            if status != "OK":
                raise SmokeFailure(f"GreenMail APPEND failed: {detail!r}")
    finally:
        try:
            client.logout()
        except (imaplib.IMAP4.error, OSError):
            pass


def greenmail_messages(imap_port: int) -> list[Any]:
    client = imaplib.IMAP4("127.0.0.1", imap_port)
    messages: list[Any] = []
    try:
        client.login(MAILBOX_USERNAME, MAILBOX_PASSWORD)
        status, _ = client.select("INBOX")
        if status != "OK":
            raise SmokeFailure("could not select GreenMail INBOX")
        status, data = client.search(None, "ALL")
        if status != "OK":
            raise SmokeFailure("could not search GreenMail INBOX")
        for message_number in data[0].split():
            status, fetched = client.fetch(message_number, "(RFC822)")
            if status != "OK":
                raise SmokeFailure(f"could not fetch GreenMail message {message_number!r}")
            raw = next(
                (part[1] for part in fetched if isinstance(part, tuple) and isinstance(part[1], bytes)),
                None,
            )
            if raw is not None:
                messages.append(BytesParser(policy=email.policy.default).parsebytes(raw))
    finally:
        try:
            client.logout()
        except (imaplib.IMAP4.error, OSError):
            pass
    return messages


def assert_subjects(items: list[dict[str, Any]], expected: set[str], forbidden: set[str]) -> None:
    subjects = {str(item.get("subject") or "") for item in items}
    if not expected.issubset(subjects):
        raise SmokeFailure(f"missing subjects {sorted(expected - subjects)}; got {sorted(subjects)}")
    leaked = subjects & forbidden
    if leaked:
        raise SmokeFailure(f"cross-alias messages leaked into inbox: {sorted(leaked)}")


def find_item(items: list[dict[str, Any]], subject: str) -> dict[str, Any]:
    for item in items:
        if item.get("subject") == subject:
            return item
    raise SmokeFailure(f"message with subject {subject!r} was not returned")


class Harness:
    def __init__(self, keep: bool):
        self.keep = keep
        self.temp: tempfile.TemporaryDirectory[str] | None
        if keep:
            self.temp = None
            self.temp_path = Path(tempfile.mkdtemp(prefix="agent-email-alias-smoke-"))
        else:
            self.temp = tempfile.TemporaryDirectory(prefix="agent-email-alias-smoke-")
            self.temp_path = Path(self.temp.name)
        suffix = uuid.uuid4().hex[:12]
        self.pg_container = f"agent-email-alias-pg-{suffix}"
        self.greenmail_container = f"agent-email-alias-greenmail-{suffix}"
        self.created_containers: list[str] = []
        self.emaild: subprocess.Popen[bytes] | None = None
        self.log_path = self.temp_path / "emaild.log"
        self.log_file: Any | None = None
        self.binary = self.temp_path / "emaild"
        self.empty_env_file = self.temp_path / "empty.env"
        self.empty_env_file.write_text("", encoding="utf-8")
        self.api_port = free_local_port()
        self.api_url = f"http://127.0.0.1:{self.api_port}"
        self.master_key = "smoke-" + uuid.uuid4().hex
        self.pg_port = 0
        self.imap_port = 0
        self.smtp_port = 0
        self.daemon_env: dict[str, str] = {}

    def start_dependencies(self) -> None:
        print("[1/9] starting isolated pgvector and GreenMail containers")
        run(
            [
                "docker", "run", "--detach", "--name", self.pg_container,
                "--env", "POSTGRES_USER=email", "--env", "POSTGRES_PASSWORD=email",
                "--env", "POSTGRES_DB=email_router", "--publish", "127.0.0.1::5432",
                "pgvector/pgvector:pg16",
            ],
            capture=True,
        )
        self.created_containers.append(self.pg_container)
        run(
            [
                "docker", "run", "--detach", "--name", self.greenmail_container,
                "--env", (
                    "GREENMAIL_OPTS=-Dgreenmail.setup.test.all "
                    "-Dgreenmail.hostname=0.0.0.0 "
                    "-Dgreenmail.users=primary:app-password@example.com"
                ),
                "--publish", "127.0.0.1::3143", "--publish", "127.0.0.1::3025",
                "greenmail/standalone:2.1.3",
            ],
            capture=True,
        )
        self.created_containers.append(self.greenmail_container)
        self.pg_port = docker_port(self.pg_container, 5432)
        self.imap_port = docker_port(self.greenmail_container, 3143)
        self.smtp_port = docker_port(self.greenmail_container, 3025)
        wait_for_socket(self.pg_port, "Postgres")
        wait_for_socket(self.imap_port, "GreenMail IMAP")
        wait_for_socket(self.smtp_port, "GreenMail SMTP")

    def build_and_migrate(self) -> None:
        print("[2/9] building emaild into the temporary harness directory")
        build_env = os.environ.copy()
        build_env["GOCACHE"] = str(self.temp_path / "go-build-cache")
        run(["go", "build", "-o", str(self.binary), "./cmd/emaild"], env=build_env)
        dsn = f"postgres://email:email@127.0.0.1:{self.pg_port}/email_router?sslmode=disable"
        self.daemon_env = os.environ.copy()
        self.daemon_env.update(
            {
                "POSTGRES_DSN": dsn,
                "MASTER_KEY": self.master_key,
                "MAILBOX_CREDENTIAL_KEY": base64.b64encode(os.urandom(32)).decode("ascii"),
                "EMAILD_LISTEN": f"127.0.0.1:{self.api_port}",
                "EMAILD_ENV_FILE": str(self.empty_env_file),
                "SUMMARY_ENABLED": "false",
                "INITIAL_SYNC_MAX_MESSAGES": "200",
                "FETCH_MAX_MESSAGES_PER_SYNC": "200",
                "POLL_INTERVAL_SEC": "300",
            }
        )
        print("[3/9] applying migrations with AI disabled")
        run([str(self.binary), "migrate"], env=self.daemon_env)

    def start_emaild(self) -> None:
        if self.emaild is not None:
            raise SmokeFailure("emaild is already running")
        self.log_file = self.log_path.open("ab")
        self.emaild = subprocess.Popen(
            [str(self.binary), "serve", "--all-mailboxes"],
            cwd=ROOT,
            env=self.daemon_env,
            stdout=self.log_file,
            stderr=subprocess.STDOUT,
        )
        wait_for_http(self.api_url, self.emaild)

    def stop_emaild(self) -> None:
        process = self.emaild
        self.emaild = None
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        if self.log_file is not None:
            self.log_file.close()
            self.log_file = None

    def cleanup(self) -> None:
        self.stop_emaild()
        if self.keep:
            if self.created_containers:
                print("kept smoke containers: " + ", ".join(self.created_containers), file=sys.stderr)
            print(f"kept smoke artifacts: {self.temp_path}", file=sys.stderr)
            return
        for container in reversed(self.created_containers):
            subprocess.run(
                ["docker", "rm", "--force", container],
                cwd=ROOT,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )
        self.created_containers.clear()
        if self.temp is not None:
            self.temp.cleanup()

    def print_diagnostics(self) -> None:
        if self.log_path.exists():
            print("--- emaild log ---", file=sys.stderr)
            lines = self.log_path.read_text(encoding="utf-8", errors="replace").splitlines()
            print("\n".join(lines[-160:]), file=sys.stderr)
        for container in self.created_containers:
            result = subprocess.run(
                ["docker", "logs", "--tail", "80", container],
                cwd=ROOT,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                check=False,
            )
            print(f"--- {container} log ---\n{result.stdout}", file=sys.stderr)

    def credential_fingerprint(self, mailbox_name: str) -> str:
        result = run(
            [
                "docker", "exec", self.pg_container,
                "psql", "-U", "email", "-d", "email_router", "-At",
                "-c", "SELECT md5(password_encrypted) FROM mailboxes WHERE name='" + mailbox_name.replace("'", "''") + "'",
            ],
            capture=True,
        )
        fingerprint = result.stdout.strip()
        if len(fingerprint) != 32:
            raise SmokeFailure("could not verify retained encrypted mailbox credential")
        return fingerprint

    def postgres_exec(self, sql: str) -> None:
        result = subprocess.run(
            ["docker", "exec", "-i", self.pg_container, "psql", "-U", "email", "-d", "email_router", "-v", "ON_ERROR_STOP=1"],
            cwd=ROOT,
            input=sql,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        if result.returncode != 0:
            raise SmokeFailure("fixture token database rotation failed")

    def postgres_scalar(self, sql: str) -> str:
        result = run(
            ["docker", "exec", self.pg_container, "psql", "-U", "email", "-d", "email_router", "-At", "-c", sql],
            capture=True,
        )
        return result.stdout.strip()


def load_e2e_seed_module() -> Any:
    path = ROOT / "scripts" / "e2e-seed.py"
    spec = importlib.util.spec_from_file_location("agent_emails_e2e_seed", path)
    if spec is None or spec.loader is None:
        raise SmokeFailure("could not load E2E seed module")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def verify_repeat_api_bootstrap(harness: Harness, mailbox_id: str) -> None:
    seed = load_e2e_seed_module()
    fixture = {
        "mailbox": {
            "name": MAILBOX_NAME,
            "imap_host": "unused",
            "imap_port": 1,
            "imap_tls": True,
            "smtp_host": "unused",
            "smtp_port": 1,
            "smtp_tls": True,
            "username": MAILBOX_USERNAME,
            "password": "must-not-replace-stored-ciphertext",
        },
        "aliases": [
            {"email": RESEARCH_ALIAS, "token_name": "repeat-research"},
            {"email": ASSISTANT_ALIAS, "token_name": "repeat-assistant"},
        ],
    }
    environment = {
        "EMAIL_API": harness.api_url,
        "MASTER_KEY": harness.master_key,
        "POSTGRES_DSN": "postgres://synthetic.invalid/fixture",
        "FIXTURE_IMAP_HOST": "127.0.0.1",
        "FIXTURE_IMAP_PORT": str(harness.imap_port),
        "FIXTURE_IMAP_TLS": "false",
        "FIXTURE_SMTP_HOST": "127.0.0.1",
        "FIXTURE_SMTP_PORT": str(harness.smtp_port),
        "FIXTURE_SMTP_TLS": "false",
    }
    before = harness.credential_fingerprint(MAILBOX_NAME)
    results = []
    with patch.dict(os.environ, environment, clear=False), patch.object(seed, "psql_exec", side_effect=harness.postgres_exec):
        for _ in range(2):
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                seed.bootstrap_emaild_via_api(fixture)
            results.append(json.loads(output.getvalue()))
    after = harness.credential_fingerprint(MAILBOX_NAME)
    returned_ids = [result.get("mailbox_id") for result in results]
    if returned_ids != [mailbox_id, mailbox_id]:
        raise SmokeFailure(f"repeat API bootstrap changed mailbox identity: {returned_ids!r}")
    if after != before:
        raise SmokeFailure("repeat API bootstrap replaced the encrypted mailbox credential")
    first_aliases = {item["alias"].lower(): item for item in results[0]["aliases"]}
    second_aliases = {item["alias"].lower(): item for item in results[1]["aliases"]}
    for email in (RESEARCH_ALIAS, ASSISTANT_ALIAS):
        if first_aliases[email]["token_id"] != second_aliases[email]["token_id"]:
            raise SmokeFailure(f"repeat API bootstrap changed deterministic token ID for {email}")
        api_request(
            harness.api_url, "GET", mailbox_path("/v1/inbox/list"),
            token=first_aliases[email]["token"], expected=(401,),
        )
    api_request(
        harness.api_url, "GET", mailbox_path("/v1/admin/aliases"),
        token=results[0]["admin_token"], expected=(401,),
    )
    api_json(
        harness.api_url, "GET", mailbox_path("/v1/admin/aliases"),
        token=results[1]["admin_token"],
    )
    for email in (RESEARCH_ALIAS, ASSISTANT_ALIAS):
        api_json(
            harness.api_url, "GET", mailbox_path("/v1/inbox/list"),
            token=second_aliases[email]["token"],
        )

    stable_hashes = harness.postgres_scalar(
        "SELECT string_agg(name || ':' || token_hash, ',' ORDER BY name) FROM tokens "
        "WHERE revoked_at IS NULL AND "
        "(name='fixture-admin:primary' OR name IN ('repeat-research','repeat-assistant'))"
    )
    harness.postgres_exec(
        "UPDATE aliases SET enabled=FALSE WHERE mailbox_id="
        f"{seed.sql_literal(mailbox_id)}::uuid AND email={seed.sql_literal(ASSISTANT_ALIAS)};\n"
    )
    try:
        try:
            with patch.dict(os.environ, environment, clear=False), \
                    patch.object(seed, "psql_exec", side_effect=harness.postgres_exec):
                seed.provision_fixture_identities(
                    mailbox_id=mailbox_id,
                    mailbox_name=MAILBOX_NAME,
                    master_key=harness.master_key,
                    aliases=[
                        {"email": RESEARCH_ALIAS, "token_name": "repeat-research"},
                        {"email": ASSISTANT_ALIAS, "token_name": "repeat-assistant"},
                    ],
                )
        except SmokeFailure:
            pass
        else:
            raise SmokeFailure("disabled retained alias was unexpectedly reused")
    finally:
        harness.postgres_exec(
            "UPDATE aliases SET enabled=TRUE WHERE mailbox_id="
            f"{seed.sql_literal(mailbox_id)}::uuid AND email={seed.sql_literal(ASSISTANT_ALIAS)};\n"
        )
    if harness.postgres_scalar(
        "SELECT string_agg(name || ':' || token_hash, ',' ORDER BY name) FROM tokens "
        "WHERE revoked_at IS NULL AND "
        "(name='fixture-admin:primary' OR name IN ('repeat-research','repeat-assistant'))"
    ) != stable_hashes:
        raise SmokeFailure("disabled-alias rejection changed previously emitted credentials")
    api_json(
        harness.api_url, "GET", mailbox_path("/v1/admin/aliases"),
        token=results[1]["admin_token"],
    )
    api_json(
        harness.api_url, "GET", mailbox_path("/v1/inbox/list"),
        token=second_aliases[ASSISTANT_ALIAS]["token"],
    )
    active_count = harness.postgres_scalar(
        "SELECT count(*) FROM tokens WHERE revoked_at IS NULL AND "
        "(name='fixture-admin:primary' OR name IN ('repeat-research','repeat-assistant'))"
    )
    if active_count != "3":
        raise SmokeFailure(f"repeat bootstrap left unexpected active fixture token count {active_count!r}")

    harness.postgres_exec(
        "CREATE OR REPLACE FUNCTION reject_late_fixture_token() RETURNS trigger LANGUAGE plpgsql AS $$ "
        "BEGIN IF NEW.name='rollback-agent' THEN RAISE EXCEPTION 'synthetic late token failure'; "
        "END IF; RETURN NEW; END $$;\n"
        "CREATE TRIGGER reject_late_fixture_token BEFORE INSERT OR UPDATE ON tokens "
        "FOR EACH ROW EXECUTE FUNCTION reject_late_fixture_token();\n"
    )
    try:
        try:
            with patch.dict(os.environ, environment, clear=False), \
                    patch.object(seed, "psql_exec", side_effect=harness.postgres_exec):
                seed.provision_fixture_identities(
                    mailbox_id=mailbox_id,
                    mailbox_name=MAILBOX_NAME,
                    master_key=harness.master_key,
                    aliases=[
                        {"email": RESEARCH_ALIAS, "token_name": "repeat-research"},
                        {"email": ASSISTANT_ALIAS, "token_name": "rollback-agent"},
                    ],
                )
        except SmokeFailure:
            pass
        else:
            raise SmokeFailure("synthetic late token failure unexpectedly committed")
    finally:
        harness.postgres_exec(
            "DROP TRIGGER IF EXISTS reject_late_fixture_token ON tokens;\n"
            "DROP FUNCTION IF EXISTS reject_late_fixture_token();\n"
        )
    if harness.postgres_scalar(
        "SELECT string_agg(name || ':' || token_hash, ',' ORDER BY name) FROM tokens "
        "WHERE revoked_at IS NULL AND "
        "(name='fixture-admin:primary' OR name IN ('repeat-research','repeat-assistant'))"
    ) != stable_hashes:
        raise SmokeFailure("failed fixture transaction changed previously emitted credentials")
    if harness.postgres_scalar("SELECT count(*) FROM tokens WHERE name='rollback-agent'") != "0":
        raise SmokeFailure("failed fixture transaction left a replacement token row")
    api_json(
        harness.api_url, "GET", mailbox_path("/v1/admin/aliases"),
        token=results[1]["admin_token"],
    )
    for email in (RESEARCH_ALIAS, ASSISTANT_ALIAS):
        api_json(
            harness.api_url, "GET", mailbox_path("/v1/inbox/list"),
            token=second_aliases[email]["token"],
        )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--keep", action="store_true", help="keep temporary containers and artifacts")
    args = parser.parse_args()
    require_commands("docker", "go")
    harness = Harness(args.keep)
    atexit.register(harness.cleanup)

    def stop_for_signal(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt(f"received signal {signum}")

    signal.signal(signal.SIGTERM, stop_for_signal)
    signal.signal(signal.SIGINT, stop_for_signal)

    try:
        harness.start_dependencies()
        harness.build_and_migrate()
        print("[4/9] starting emaild and bootstrapping mailbox")
        harness.start_emaild()

        mailbox = api_json(
            harness.api_url,
            "POST",
            "/v1/admin/mailboxes",
            master_key=harness.master_key,
            payload={
                "name": MAILBOX_NAME,
                "imap_host": "127.0.0.1",
                "imap_port": harness.imap_port,
                "imap_tls": False,
                "smtp_host": "127.0.0.1",
                "smtp_port": harness.smtp_port,
                "smtp_tls": False,
                "username": MAILBOX_USERNAME,
                "password": MAILBOX_PASSWORD,
            },
        )
        if not mailbox.get("mailbox_id"):
            raise SmokeFailure(f"mailbox bootstrap returned no id: {mailbox!r}")
        admin = api_json(
            harness.api_url,
            "POST",
            "/v1/admin/mint-token",
            master_key=harness.master_key,
            payload={"name": "alias-isolation-smoke-admin"},
        )
        admin_token = admin["token"]
        for alias in (RESEARCH_ALIAS, ASSISTANT_ALIAS):
            api_json(
                harness.api_url,
                "POST",
                mailbox_path("/v1/admin/aliases"),
                token=admin_token,
                payload={"email": alias},
            )

        verify_repeat_api_bootstrap(harness, mailbox["mailbox_id"])

        def mint(alias: str, name: str, scopes: list[str]) -> dict[str, str]:
            return api_json(
                harness.api_url,
                "POST",
                mailbox_path("/v1/admin/tokens"),
                token=admin_token,
                payload={"alias_email": alias, "name": name, "scopes": scopes},
            )

        research = mint(RESEARCH_ALIAS, "research-agent", ["read", "send"])
        assistant = mint(ASSISTANT_ALIAS, "assistant-agent", ["read", "send"])
        read_only = mint(RESEARCH_ALIAS, "research-reader", ["read"])

        marker = uuid.uuid4().hex[:12]
        research_subject = f"Research private {marker}"
        assistant_subject = f"Assistant private {marker}"
        sender_trap_subject = f"Personal sender trap {marker}"
        header_trap_subject = f"Personal header trap {marker}"
        append_messages(
            harness.imap_port,
            [
                make_message(research_subject, MAILBOX_ADDRESS, RESEARCH_ALIAS, f"research body {marker}"),
                make_message(assistant_subject, MAILBOX_ADDRESS, ASSISTANT_ALIAS, f"assistant body {marker}"),
                make_message(sender_trap_subject, RESEARCH_ALIAS, MAILBOX_ADDRESS, f"personal body {marker}"),
                make_message(
                    header_trap_subject,
                    MAILBOX_ADDRESS,
                    MAILBOX_ADDRESS,
                    f"custom header body {marker}",
                    X_Agent_Alias=RESEARCH_ALIAS,
                ),
            ],
        )

        print("[5/9] syncing and checking two-agent read isolation")
        api_json(
            harness.api_url,
            "POST",
            mailbox_path("/v1/admin/sync"),
            token=admin_token,
            payload={},
        )
        research_items = api_json(
            harness.api_url, "GET", mailbox_path("/v1/inbox/list?limit=50"), token=research["token"]
        )
        assistant_items = api_json(
            harness.api_url, "GET", mailbox_path("/v1/inbox/list?limit=50"), token=assistant["token"]
        )
        personal_subjects = {sender_trap_subject, header_trap_subject}
        assert_subjects(research_items, {research_subject}, {assistant_subject} | personal_subjects)
        assert_subjects(assistant_items, {assistant_subject}, {research_subject} | personal_subjects)
        research_id = find_item(research_items, research_subject)["msg_id"]
        assistant_id = find_item(assistant_items, assistant_subject)["msg_id"]

        for token, own_id, other_id, own_subject in (
            (research["token"], research_id, assistant_id, research_subject),
            (assistant["token"], assistant_id, research_id, assistant_subject),
        ):
            read = api_json(
                harness.api_url,
                "GET",
                mailbox_path(f"/v1/inbox/read?id={urllib.parse.quote(own_id)}"),
                token=token,
            )
            if read.get("subject") != own_subject:
                raise SmokeFailure(f"read returned wrong message: {read!r}")
            body = api_json(
                harness.api_url,
                "GET",
                mailbox_path(f"/v1/inbox/body?id={urllib.parse.quote(own_id)}"),
                token=token,
            )
            if marker not in str(body.get("extracted_text") or body.get("text_plain") or ""):
                raise SmokeFailure("message body did not contain smoke marker")
            _, raw = api_request(
                harness.api_url,
                "GET",
                mailbox_path(f"/v1/inbox/raw?id={urllib.parse.quote(own_id)}"),
                token=token,
            )
            if own_subject.encode() not in raw:
                raise SmokeFailure("raw message did not contain its subject")
            search = api_json(
                harness.api_url,
                "GET",
                mailbox_path("/v1/search?" + urllib.parse.urlencode({"q": marker, "limit": 50})),
                token=token,
            )
            assert_subjects(search, {own_subject}, {research_subject, assistant_subject, *personal_subjects} - {own_subject})
            folders = api_json(
                harness.api_url, "GET", mailbox_path("/v1/folders"), token=token
            )
            inbox = next((folder for folder in folders if str(folder.get("name", "")).lower() == "inbox"), None)
            if inbox is None or inbox.get("total") != 1:
                raise SmokeFailure(f"folder counts crossed alias boundary: {folders!r}")
            api_json(
                harness.api_url,
                "POST",
                mailbox_path("/v1/inbox/ack"),
                token=token,
                payload={"message_id": own_id},
            )
            unread = api_json(
                harness.api_url,
                "GET",
                mailbox_path("/v1/inbox/list?unread=true&limit=50"),
                token=token,
            )
            if any(item.get("msg_id") == own_id for item in unread):
                raise SmokeFailure("acknowledged message remained unread")
            api_json(
                harness.api_url,
                "POST",
                mailbox_path("/v1/inbox/unack"),
                token=token,
                payload={"message_id": own_id},
            )
            for endpoint in ("read", "body", "raw"):
                api_request(
                    harness.api_url,
                    "GET",
                    mailbox_path(f"/v1/inbox/{endpoint}?id={urllib.parse.quote(other_id)}"),
                    token=token,
                    expected=(403,),
                )
            api_request(
                harness.api_url,
                "POST",
                mailbox_path("/v1/inbox/unack"),
                token=token,
                payload={"message_id": other_id},
                expected=(403,),
            )

        print("[6/9] checking send, reply, scope, and forced-From behavior")
        send_subject = f"Research outbound {marker}"
        api_json(
            harness.api_url,
            "POST",
            mailbox_path("/v1/send"),
            token=research["token"],
            payload={
                "to": [MAILBOX_ADDRESS],
                "subject": send_subject,
                "body": f"outbound body {marker}",
                "idempotency_key": f"send-{marker}",
            },
        )
        api_request(
            harness.api_url,
            "POST",
            mailbox_path("/v1/send"),
            token=read_only["token"],
            payload={"to": [MAILBOX_ADDRESS], "subject": "must fail", "body": "must fail"},
            expected=(403,),
        )
        wrong_raw = make_message(f"Wrong alias {marker}", ASSISTANT_ALIAS, MAILBOX_ADDRESS, "must fail")
        api_request(
            harness.api_url,
            "POST",
            mailbox_path("/v1/send/raw"),
            token=research["token"],
            payload={
                "from": ASSISTANT_ALIAS,
                "to": [MAILBOX_ADDRESS],
                "raw_base64": base64.b64encode(wrong_raw).decode("ascii"),
                "idempotency_key": f"wrong-alias-{marker}",
            },
            expected=(400,),
        )
        api_json(
            harness.api_url,
            "POST",
            mailbox_path("/v1/reply"),
            token=research["token"],
            payload={
                "message_id": research_id,
                "body": f"reply body {marker}",
                "idempotency_key": f"reply-{marker}",
            },
        )
        reply_subject = "Re: " + research_subject
        deadline = time.monotonic() + 30
        delivered: dict[str, Any] = {}
        while time.monotonic() < deadline:
            delivered = {
                str(message.get("Subject", "")): message
                for message in greenmail_messages(harness.imap_port)
                if str(message.get("Subject", "")) in {send_subject, reply_subject}
            }
            if delivered.keys() >= {send_subject, reply_subject}:
                break
            time.sleep(1)
        if delivered.keys() < {send_subject, reply_subject}:
            raise SmokeFailure(f"send/reply were not delivered to GreenMail; got {sorted(delivered)}")
        for subject in (send_subject, reply_subject):
            from_email = parseaddr(str(delivered[subject].get("From", "")))[1].lower()
            if from_email != RESEARCH_ALIAS:
                raise SmokeFailure(f"{subject!r} used unexpected From address {from_email!r}")

        print("[7/9] checking revocation")
        api_json(
            harness.api_url,
            "POST",
            "/v1/admin/tokens/revoke",
            token=admin_token,
            payload={"token_id": assistant["token_id"]},
        )
        api_request(
            harness.api_url,
            "GET",
            mailbox_path("/v1/inbox/list"),
            token=assistant["token"],
            expected=(401,),
        )

        print("[8/9] restarting emaild and checking sync deduplication")
        before = api_json(
            harness.api_url, "GET", mailbox_path("/v1/status"), token=admin_token
        )
        if before.get("MessageCount") != 2:
            raise SmokeFailure(f"expected exactly two routed messages before restart: {before!r}")
        harness.stop_emaild()
        harness.start_emaild()
        api_json(
            harness.api_url,
            "POST",
            mailbox_path("/v1/admin/sync"),
            token=admin_token,
            payload={},
        )
        after = api_json(
            harness.api_url, "GET", mailbox_path("/v1/status"), token=admin_token
        )
        if after.get("MessageCount") != before.get("MessageCount"):
            raise SmokeFailure(f"restart sync duplicated messages: before={before!r} after={after!r}")
        research_after = api_json(
            harness.api_url, "GET", mailbox_path("/v1/inbox/list?limit=50"), token=research["token"]
        )
        assert_subjects(research_after, {research_subject}, {assistant_subject} | personal_subjects)
        if find_item(research_after, research_subject).get("msg_id") != research_id:
            raise SmokeFailure("restart changed the persisted research message identity")

        print("[9/9] SUCCESS: two-agent alias isolation smoke passed")
        return 0
    except BaseException:
        harness.print_diagnostics()
        raise


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("alias isolation smoke interrupted", file=sys.stderr)
        raise SystemExit(130)
    except (SmokeFailure, subprocess.CalledProcessError) as exc:
        print(f"alias isolation smoke failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
