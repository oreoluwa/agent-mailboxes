#!/usr/bin/env python3
"""Exercise supported atomic onboarding and a portable agent profile locally."""

from __future__ import annotations

import atexit
import http.client
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import time
from typing import Any
import urllib.parse


ROOT = Path(__file__).resolve().parents[1]
MAILBOX_NAME = "supported-pilot"
MAILBOX_ADDRESS = "owner@example.test"
AGENT_ALIAS = "agent@example.test"
OTHER_ALIAS = "other@example.test"


def load_alias_smoke() -> Any:
    path = ROOT / "scripts" / "alias-isolation-smoke.py"
    spec = importlib.util.spec_from_file_location("agent_emails_alias_smoke", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("could not load disposable smoke harness")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


smoke = load_alias_smoke()


def build_email_client(harness: Any) -> Path:
    binary = harness.temp_path / "email"
    build_env = os.environ.copy()
    build_env["GOCACHE"] = str(harness.temp_path / "go-build-cache")
    smoke.run(["go", "build", "-o", str(binary), "./cmd/email"], env=build_env)
    return binary


def start_tls_proxy(harness: Any) -> tuple[subprocess.Popen[str], dict[str, int]]:
    binary = harness.temp_path / "test-tls-proxy"
    ca_path = harness.temp_path / "test-ca.pem"
    build_env = os.environ.copy()
    build_env["GOCACHE"] = str(harness.temp_path / "go-build-cache")
    smoke.run(["go", "build", "-o", str(binary), "./scripts/test-tls-proxy.go"], env=build_env)
    process = subprocess.Popen(
        [
            str(binary),
            "--imap-target", f"127.0.0.1:{harness.imap_port}",
            "--smtp-target", f"127.0.0.1:{harness.smtp_port}",
            "--ca-out", str(ca_path),
        ],
        cwd=ROOT,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    assert process.stdout is not None
    deadline = time.monotonic() + 10
    line = ""
    while time.monotonic() < deadline and not line:
        if process.poll() is not None:
            raise smoke.SmokeFailure("test TLS proxy exited before becoming ready")
        line = process.stdout.readline().strip()
    try:
        ports = json.loads(line)
    except json.JSONDecodeError as error:
        process.terminate()
        raise smoke.SmokeFailure("test TLS proxy returned invalid readiness data") from error
    harness.daemon_env["SSL_CERT_FILE"] = str(ca_path)
    return process, {"imap_port": int(ports["imap_port"]), "smtp_port": int(ports["smtp_port"])}


def validate_tls_test_provider(harness: Any, ports: dict[str, int]) -> None:
    binary = harness.temp_path / "test-provider-credentials"
    build_env = os.environ.copy()
    build_env["GOCACHE"] = str(harness.temp_path / "go-build-cache")
    smoke.run(["go", "build", "-o", str(binary), "./scripts/test-provider-credentials.go"], env=build_env)
    smoke.run(
        [
            str(binary),
            "--imap-port", str(ports["imap_port"]),
            "--smtp-port", str(ports["smtp_port"]),
            "--username", "primary",
            "--password", "app-password",
        ],
        env=harness.daemon_env,
    )


def write_profile(harness: Any, onboarded: dict[str, Any], alias: str, filename: str) -> Path:
    expected = {
        "mailbox_name": MAILBOX_NAME,
        "alias_email": alias,
    }
    for key, value in expected.items():
        if onboarded.get(key) != value:
            raise smoke.SmokeFailure(f"onboarding returned unexpected {key}")
    token = str(onboarded.get("token", "")).strip()
    if not token:
        raise smoke.SmokeFailure("onboarding returned no agent token")
    path = harness.temp_path / filename
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as destination:
        json.dump(
            {
                "version": 1,
                "api_url": harness.api_url,
                "mailbox": MAILBOX_NAME,
                "alias": alias,
                "token": token,
            },
            destination,
            indent=2,
        )
        destination.write("\n")
    return path


def run_email(binary: Path, profile: Path, *args: str) -> Any:
    result = smoke.run(
        [str(binary), "--profile", str(profile), *args],
        capture=True,
    )
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise smoke.SmokeFailure("native email client returned invalid JSON") from error


def provision_agent(harness: Any, alias: str, token_name: str, replace: bool = False) -> dict[str, Any]:
    return smoke.api_json(
        harness.api_url,
        "POST",
        "/v1/admin/agents",
        master_key=harness.master_key,
        payload={
            "mailbox_name": MAILBOX_NAME,
            "alias_email": alias,
            "token_name": token_name,
            "replace": replace,
        },
    )


def start_api_only(harness: Any) -> None:
    if harness.emaild is not None:
        raise smoke.SmokeFailure("emaild is already running")
    harness.log_file = harness.log_path.open("ab")
    harness.emaild = subprocess.Popen(
        [str(harness.binary), "serve", "--http-only", "--all-mailboxes"],
        cwd=ROOT,
        env=harness.daemon_env,
        stdout=harness.log_file,
        stderr=subprocess.STDOUT,
    )
    smoke.wait_for_http(harness.api_url, harness.emaild)


def wait_for_api_without_process(base_url: str, timeout: float = 90) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            smoke.api_request(base_url, "GET", "/onboarding")
            return
        except (smoke.SmokeFailure, OSError, http.client.HTTPException):
            time.sleep(0.5)
    raise smoke.SmokeFailure("containerized emaild did not become ready")


def build_darwin_emaild_image(harness: Any) -> tuple[str, str]:
    suffix = harness.pg_container.rsplit("-", 1)[-1]
    image = f"agent-emails-supported-smoke:{suffix}"
    container = f"agent-emails-supported-emaild-{suffix}"
    smoke.run([
        "docker", "build", "--build-arg", "TARGET=./cmd/emaild",
        "--tag", image, ".",
    ])
    return image, container


def start_darwin_emaild(harness: Any, image: str, container: str, ca_path: Path) -> None:
    dsn = harness.daemon_env["POSTGRES_DSN"].replace("127.0.0.1", "host.docker.internal")
    command = [
        "docker", "run", "--detach", "--rm", "--name", container,
        "--add-host", "host.docker.internal:host-gateway",
        "--publish", f"127.0.0.1:{harness.api_port}:8787",
        "--volume", f"{ca_path}:/test-ca.pem:ro",
        "--env", f"POSTGRES_DSN={dsn}",
        "--env", f"MASTER_KEY={harness.daemon_env['MASTER_KEY']}",
        "--env", f"MAILBOX_CREDENTIAL_KEY={harness.daemon_env['MAILBOX_CREDENTIAL_KEY']}",
        "--env", "EMAILD_ENV_FILE=/dev/null",
        "--env", "EMAILD_LISTEN=0.0.0.0:8787",
        "--env", "SUMMARY_ENABLED=false",
        "--env", "INITIAL_SYNC_MAX_MESSAGES=0",
        "--env", "SYNC_FOLDERS=INBOX",
        "--env", "SSL_CERT_FILE=/test-ca.pem",
        image, "/usr/local/bin/app", "serve", "--http-only", "--all-mailboxes",
    ]
    smoke.run(command, capture=True)
    harness.created_containers.append(container)
    wait_for_api_without_process(harness.api_url)


def stop_darwin_emaild(harness: Any, container: str) -> None:
    subprocess.run(
        ["docker", "rm", "--force", container],
        cwd=ROOT,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    if container in harness.created_containers:
        harness.created_containers.remove(container)


def main() -> int:
    smoke.require_commands("docker", "go")
    harness = smoke.Harness(keep=False)
    atexit.register(harness.cleanup)
    tls_proxy: subprocess.Popen[str] | None = None
    darwin_image = ""
    darwin_container = ""
    try:
        print("[1/7] starting disposable PostgreSQL and GreenMail")
        harness.start_dependencies()
        harness.build_and_migrate()
        harness.daemon_env["INITIAL_SYNC_MAX_MESSAGES"] = "0"
        email_binary = build_email_client(harness)
        tls_proxy, tls_ports = start_tls_proxy(harness)
        provider_host = "127.0.0.1"
        if sys.platform == "darwin":
            darwin_image, darwin_container = build_darwin_emaild_image(harness)
            provider_host = "host.docker.internal"
        else:
            validate_tls_test_provider(harness, tls_ports)

        print("[2/7] starting API-only service")
        if sys.platform == "darwin":
            start_darwin_emaild(harness, darwin_image, darwin_container, harness.temp_path / "test-ca.pem")
        else:
            start_api_only(harness)
        payload = {
            "name": MAILBOX_NAME,
            "imap_host": provider_host,
            "imap_port": tls_ports["imap_port"],
            "imap_tls": True,
            "smtp_host": provider_host,
            "smtp_port": tls_ports["smtp_port"],
            "smtp_tls": True,
            "username": "primary",
            "password": "app-password",
            "alias_email": AGENT_ALIAS,
            "token_name": "supported-agent",
        }
        onboarded = smoke.api_json(
            harness.api_url,
            "POST",
            "/v1/admin/onboard",
            master_key=harness.master_key,
            payload=payload,
        )
        if onboarded.get("poller_started") is not False:
            raise smoke.SmokeFailure("API-only onboarding unexpectedly started a poller")
        profile = write_profile(harness, onboarded, AGENT_ALIAS, "agent-profile.json")
        queued = smoke.api_json(
            harness.api_url, "POST", f"/v1/send?mailbox={MAILBOX_NAME}", token=onboarded["token"],
            payload={"to": ["recipient@example.test"], "subject": "API-only queue probe", "body": "synthetic", "idempotency_key": "api-only-probe"},
        )
        def assert_queue_stays_pending():
            # Queue workers run every two seconds; observe beyond a full interval.
            time.sleep(3)
            state = harness.postgres_scalar(
                "SELECT status || ':' || attempts::text FROM outbound_messages "
                "WHERE mailbox_id=(SELECT id FROM mailboxes WHERE name='supported-pilot')"
            )
            if state != "pending:0":
                raise smoke.SmokeFailure(f"HTTP-only server processed outbound work: {state!r}")
        if not queued.get("job_id"):
            raise smoke.SmokeFailure("API-only server did not accept an outbound job")
        assert_queue_stays_pending()

        second = provision_agent(harness, OTHER_ALIAS, "supported-other")
        second_profile = write_profile(harness, second, OTHER_ALIAS, "other-agent-profile.json")

        print("[3/7] verifying both agents before first sync")
        initial = run_email(email_binary, profile, "inbox", "list", "--json")
        second_initial = run_email(email_binary, second_profile, "inbox", "list", "--json")
        if initial != [] or second_initial != []:
            raise smoke.SmokeFailure("new-mail-only onboarding imported messages before first sync")
        if harness.postgres_scalar(
            "SELECT count(*) FROM aliases WHERE email IN ('agent@example.test','other@example.test') AND enabled=TRUE"
        ) != "2":
            raise smoke.SmokeFailure("both agent aliases were not committed before first sync")
        admin = smoke.api_json(
            harness.api_url,
            "POST",
            "/v1/admin/mint-token",
            master_key=harness.master_key,
            payload={"name": "supported-smoke-admin"},
        )
        sync_path = "/v1/admin/sync?" + urllib.parse.urlencode({"mailbox": MAILBOX_NAME})
        smoke.api_json(
            harness.api_url, "POST", sync_path,
            token=admin["token"], payload={},
        )
        if harness.postgres_scalar("SELECT count(*) FROM messages") != "0":
            raise smoke.SmokeFailure("initial new-mail-only checkpoint imported history")

        print("[4/7] delivering synthetic alias and unrelated messages")
        marker = os.urandom(6).hex()
        agent_subject = f"Supported agent {marker}"
        other_subject = f"Other alias {marker}"
        personal_subject = f"Primary personal {marker}"
        smoke.append_messages(
            harness.imap_port,
            [
                smoke.make_message(agent_subject, "sender@example.test", AGENT_ALIAS, "agent body"),
                smoke.make_message(other_subject, "sender@example.test", OTHER_ALIAS, "other body"),
                smoke.make_message(personal_subject, "sender@example.test", MAILBOX_ADDRESS, "personal body"),
            ],
        )
        smoke.api_json(
            harness.api_url,
            "POST",
            sync_path,
            token=admin["token"],
            payload={},
        )
        visible = run_email(email_binary, profile, "inbox", "list", "--json")
        subjects = {item.get("subject") for item in visible}
        if subjects != {agent_subject}:
            raise smoke.SmokeFailure(
                f"portable profile visibility mismatch: got={sorted(str(item) for item in subjects)}"
            )
        second_visible = run_email(email_binary, second_profile, "inbox", "list", "--json")
        if {item.get("subject") for item in second_visible} != {other_subject}:
            raise smoke.SmokeFailure("second portable profile did not enforce alias visibility")

        print("[5/7] replacing one agent credential without affecting the other")
        if profile.stat().st_mode & 0o077 or second_profile.stat().st_mode & 0o077:
            raise smoke.SmokeFailure("agent profile permissions are not private")
        result = smoke.run(
            [str(email_binary), "--profile", str(profile), "inbox", "list", "--json"],
            capture=True,
        )
        if onboarded["token"] in result.stdout or onboarded["token"] in result.stderr:
            raise smoke.SmokeFailure("native client output exposed the agent token")
        replacement = provision_agent(harness, AGENT_ALIAS, "supported-agent", replace=True)
        replacement_profile = write_profile(
            harness, replacement, AGENT_ALIAS, "replacement-agent-profile.json"
        )
        old_result = subprocess.run(
            [str(email_binary), "--profile", str(profile), "inbox", "list", "--json"],
            cwd=ROOT, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False,
        )
        if old_result.returncode == 0 or "api status 401" not in old_result.stderr:
            raise smoke.SmokeFailure("explicit replacement did not revoke the old agent profile")
        if onboarded["token"] in old_result.stdout or onboarded["token"] in old_result.stderr:
            raise smoke.SmokeFailure("revoked profile error exposed the old token")
        if {item.get("subject") for item in run_email(email_binary, replacement_profile, "inbox", "list", "--json")} != {agent_subject}:
            raise smoke.SmokeFailure("replacement agent profile is not usable")
        if {item.get("subject") for item in run_email(email_binary, second_profile, "inbox", "list", "--json")} != {other_subject}:
            raise smoke.SmokeFailure("replacing one agent invalidated the other agent")

        print("[6/7] restarting service with the same persistent database")
        checkpoint_before = harness.postgres_scalar(
            "SELECT uidvalidity::text || ':' || last_seen_uid::text FROM folders "
            "WHERE name='INBOX' AND mailbox_id=(SELECT id FROM mailboxes WHERE name='supported-pilot')"
        )
        if sys.platform == "darwin":
            stop_darwin_emaild(harness, darwin_container)
            start_darwin_emaild(harness, darwin_image, darwin_container, harness.temp_path / "test-ca.pem")
        else:
            harness.stop_emaild()
            start_api_only(harness)
        assert_queue_stays_pending()
        after_restart = run_email(email_binary, replacement_profile, "inbox", "list", "--json")
        if {item.get("subject") for item in after_restart} != {agent_subject}:
            raise smoke.SmokeFailure("agent profile or messages did not survive restart")
        if {item.get("subject") for item in run_email(email_binary, second_profile, "inbox", "list", "--json")} != {other_subject}:
            raise smoke.SmokeFailure("second agent profile did not survive restart")
        checkpoint_after = harness.postgres_scalar(
            "SELECT uidvalidity::text || ':' || last_seen_uid::text FROM folders "
            "WHERE name='INBOX' AND mailbox_id=(SELECT id FROM mailboxes WHERE name='supported-pilot')"
        )
        if not checkpoint_before or checkpoint_after != checkpoint_before:
            raise smoke.SmokeFailure("INBOX checkpoint did not survive restart")

        print("[7/7] SUCCESS: supported onboarding and portable access passed")
        return 0
    except BaseException:
        harness.print_diagnostics()
        raise
    finally:
        if tls_proxy is not None and tls_proxy.poll() is None:
            tls_proxy.terminate()
            try:
                tls_proxy.wait(timeout=5)
            except subprocess.TimeoutExpired:
                tls_proxy.kill()
                tls_proxy.wait(timeout=5)
        if darwin_container:
            stop_darwin_emaild(harness, darwin_container)
        if darwin_image:
            subprocess.run(
                ["docker", "image", "rm", "--force", darwin_image],
                cwd=ROOT, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False,
            )


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (smoke.SmokeFailure, subprocess.CalledProcessError) as error:
        print(f"supported onboarding smoke failed: {error}", file=sys.stderr)
        raise SystemExit(1)
