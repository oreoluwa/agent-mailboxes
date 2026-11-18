#!/usr/bin/env python3
from __future__ import annotations
import argparse
import json
import os
import smtplib
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid
import hashlib
import base64
import secrets
from datetime import datetime, timezone
from email.message import EmailMessage
from email.utils import format_datetime


def load_fixture(path: str) -> dict:
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def env(name: str, default: str | None = None) -> str:
    value = os.environ.get(name, default)
    if value is None or value == "":
        raise SystemExit(f"missing required environment variable: {name}")
    return value


def iso_to_dt(value: str | None) -> datetime | None:
    if not value:
        return None
    return datetime.fromisoformat(value.replace("Z", "+00:00")).astimezone(timezone.utc)


def folder_id(mailbox_name: str, folder_name: str) -> str:
    return str(uuid.uuid5(uuid.NAMESPACE_URL, f"agent-emails-folder:{mailbox_name.lower()}:{folder_name.lower()}"))


def build_raw_message(message: dict) -> bytes:
    msg = EmailMessage()
    from_name = message.get("from_name")
    from_email = message["from_email"]
    msg["From"] = f"{from_name} <{from_email}>" if from_name else from_email
    smtp_to = message.get("smtp_to") or ["primary@example.com"]
    msg["To"] = ", ".join(smtp_to)
    if message.get("delivered_to"):
        msg["Delivered-To"] = message["delivered_to"]
    msg["Subject"] = message.get("subject", "")
    msg["Message-ID"] = message["rfc_message_id"]
    sent_dt = iso_to_dt(message.get("sent_date")) or datetime.now(timezone.utc)
    msg["Date"] = format_datetime(sent_dt)
    if message.get("in_reply_to"):
        msg["In-Reply-To"] = message["in_reply_to"]
    if message.get("references"):
        msg["References"] = message["references"]
    text_plain = message.get("text_plain") or ""
    text_html = message.get("text_html")
    if text_html:
        msg.set_content(text_plain)
        msg.add_alternative(text_html, subtype="html")
    else:
        msg.set_content(text_plain)
    return msg.as_bytes()


def node_encrypt_token(token: str, key_b64: str) -> dict:
    script = r"""
const { createCipheriv, randomBytes } = require("crypto");
const token = process.argv[1];
const key = Buffer.from(process.argv[2], "base64");
const iv = randomBytes(12);
const cipher = createCipheriv("aes-256-gcm", key, iv);
const ciphertext = Buffer.concat([cipher.update(token, "utf8"), cipher.final()]);
const authTag = cipher.getAuthTag();
process.stdout.write(JSON.stringify({
  tokenCiphertext: ciphertext.toString("base64"),
  iv: iv.toString("base64"),
  authTag: authTag.toString("base64"),
  keyVersion: 1
}));
"""
    proc = subprocess.run(
        ["node", "-e", script, token, key_b64],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr.decode("utf-8", errors="replace"))
        raise SystemExit(proc.returncode)
    return json.loads(proc.stdout.decode("utf-8"))


def hash_app_password(password: str, pepper: str) -> str:
    salt = secrets.token_bytes(16)
    derived = hashlib.scrypt((password.strip() + pepper).encode("utf-8"), salt=salt, n=16384, r=8, p=1, dklen=32)
    return f"s1${base64.b64encode(salt).decode('ascii')}${base64.b64encode(derived).decode('ascii')}"


def hash_token(master_key: str, token: str) -> str:
    return hashlib.sha256(f"{master_key}:{token}".encode("utf-8")).hexdigest()


def bootstrap_emaild(fixture: dict) -> None:
    if os.environ.get("BOOTSTRAP_DIRECT", "0") == "1":
        raise SystemExit("direct bootstrap is no longer supported; unset BOOTSTRAP_DIRECT and use EMAIL_API with the matching disposable POSTGRES_DSN")
    bootstrap_emaild_via_api(fixture)


def bootstrap_emaild_via_api(fixture: dict) -> None:
    email_api = env("EMAIL_API")
    master_key = env("MASTER_KEY")
    # API bootstrap deliberately keeps mailbox credentials on the encrypted API
    # path, while fixture-only tokens are rotated under deterministic IDs in the
    # disposable E2E database. Refuse to mutate the mailbox without that database
    # boundary because otherwise prior fixture credentials would remain active.
    env("POSTGRES_DSN")
    mailbox = dict(fixture["mailbox"])
    mailbox["imap_host"] = os.environ.get("FIXTURE_IMAP_HOST", mailbox["imap_host"])
    mailbox["imap_port"] = int(os.environ.get("FIXTURE_IMAP_PORT", str(mailbox["imap_port"])))
    mailbox["imap_tls"] = os.environ.get("FIXTURE_IMAP_TLS", str(mailbox["imap_tls"])).lower() in ("1", "true")
    mailbox["smtp_host"] = os.environ.get("FIXTURE_SMTP_HOST", mailbox["smtp_host"])
    mailbox["smtp_port"] = int(os.environ.get("FIXTURE_SMTP_PORT", str(mailbox["smtp_port"])))
    mailbox["smtp_tls"] = os.environ.get("FIXTURE_SMTP_TLS", str(mailbox["smtp_tls"])).lower() in ("1", "true")

    mailbox_url = urllib.parse.urljoin(email_api.rstrip("/") + "/", "v1/admin/mailboxes")
    mailbox_headers = {"X-Master-Key": master_key}
    mailbox_resp = find_existing_mailbox(mailbox_url, mailbox_headers, mailbox)
    if mailbox_resp is None:
        req = urllib.request.Request(
            mailbox_url,
            data=json.dumps(mailbox).encode("utf-8"),
            headers={"Content-Type": "application/json", **mailbox_headers},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                mailbox_resp = json.load(resp)
        except urllib.error.HTTPError as exc:
            exc.close()
            # A concurrent bootstrap may create the mailbox after our GET. Re-read
            # and accept only an exact non-secret configuration match.
            mailbox_resp = find_existing_mailbox(mailbox_url, mailbox_headers, mailbox)
            if mailbox_resp is None:
                raise SystemExit(f"bootstrap mailbox failed: HTTP {exc.code}; response withheld") from exc

    mailbox_name = mailbox["name"]
    mailbox_id = str(mailbox_resp.get("mailbox_id", "")).strip()
    if not mailbox_id:
        raise SystemExit(f"mailbox {mailbox_name!r} has no ID")
    admin, minted = provision_fixture_identities(
        mailbox_id=mailbox_id,
        mailbox_name=mailbox_name,
        master_key=master_key,
        aliases=fixture.get("aliases", []),
    )

    out = {
        "mailbox": mailbox_name,
        "mailbox_id": mailbox_resp.get("mailbox_id"),
        "admin_token": admin["token"],
        "aliases": minted,
    }
    print(json.dumps(out, indent=2))


def plpgsql_block(body: str) -> str:
    while True:
        delimiter = "$fixture_" + uuid.uuid4().hex + "$"
        if delimiter not in body:
            return f"DO {delimiter} BEGIN {body} END {delimiter};\n"


def fixture_token_plan(
    *, mailbox_name: str, master_key: str, alias_email: str | None,
    token_name: str, scopes: list[str], alias_id_sql: str,
) -> tuple[dict, str]:
    token = secrets.token_urlsafe(24)
    if alias_email is None:
        token_id = str(uuid.uuid5(uuid.NAMESPACE_URL, f"agent-emails-admin-token:{mailbox_name.lower()}"))
        identity = "alias_id IS NULL"
        revoke_sql = plpgsql_block(
                "IF EXISTS (SELECT 1 FROM tokens WHERE revoked_at IS NULL AND alias_id IS NULL AND name='fixture-admin') "
                "AND (SELECT count(*) FROM mailboxes) <> 1 THEN "
                "RAISE EXCEPTION 'ambiguous legacy fixture-admin token ownership'; END IF;"
            ) + (
            "UPDATE tokens SET revoked_at=now() WHERE revoked_at IS NULL AND alias_id IS NULL "
            "AND name='fixture-admin' AND (SELECT count(*) FROM mailboxes)=1;\n"
            "UPDATE tokens SET revoked_at=now() "
            f"WHERE revoked_at IS NULL AND {identity} AND name={sql_literal(token_name)} "
            f"AND id<>{sql_literal(token_id)}::uuid;\n"
        )
    else:
        normalized_email = alias_email.strip().lower()
        token_id = str(uuid.uuid5(
            uuid.NAMESPACE_URL,
            f"agent-emails-alias-token:{mailbox_name.lower()}:{normalized_email}:{token_name}",
        ))
        identity = f"alias_id={alias_id_sql}"
        revoke_sql = (
            "UPDATE tokens SET revoked_at=now() "
            f"WHERE revoked_at IS NULL AND {identity} AND name={sql_literal(token_name)} "
            f"AND id<>{sql_literal(token_id)}::uuid;\n"
        )
    scope_values = ",".join(sql_literal(scope) for scope in scopes)
    token_hash = hash_token(master_key, token)
    sql = (
        revoke_sql
        + "INSERT INTO tokens (id,alias_id,name,token_hash,scopes,created_at,revoked_at) VALUES "
        f"({sql_literal(token_id)}::uuid, {alias_id_sql}, {sql_literal(token_name)}, "
        f"{sql_literal(token_hash)}, ARRAY[{scope_values}], now(), NULL) "
        "ON CONFLICT (id) DO UPDATE SET alias_id=EXCLUDED.alias_id,name=EXCLUDED.name,"
        "token_hash=EXCLUDED.token_hash,scopes=EXCLUDED.scopes,created_at=now(),revoked_at=NULL;\n"
    )
    out = {"token": token, "token_id": token_id}
    if alias_email is not None:
        out["alias"] = alias_email
    return out, sql


def provision_fixture_identities(
    *, mailbox_id: str, mailbox_name: str, master_key: str, aliases: list[dict],
) -> tuple[dict, list[dict]]:
    admin, admin_sql = fixture_token_plan(
        mailbox_name=mailbox_name,
        master_key=master_key,
        alias_email=None,
        token_name=f"fixture-admin:{mailbox_name.lower()}",
        scopes=["admin"],
        alias_id_sql="NULL::uuid",
    )
    preflight = "BEGIN;\n" + plpgsql_block(
        f"PERFORM 1 FROM mailboxes WHERE id={sql_literal(mailbox_id)}::uuid "
        f"AND name={sql_literal(mailbox_name)} AND enabled=TRUE FOR UPDATE; "
        "IF NOT FOUND THEN RAISE EXCEPTION 'fixture mailbox identity is unavailable'; END IF;"
    )
    alias_sql = ""
    token_sql = admin_sql
    minted: list[dict] = []
    seen: set[str] = set()
    for alias in aliases:
        alias_email = str(alias["email"]).strip().lower()
        if not alias_email or alias_email in seen:
            raise SystemExit(f"duplicate or empty fixture alias {alias_email!r}")
        seen.add(alias_email)
        generated_alias_id = str(uuid.uuid5(
            uuid.NAMESPACE_URL,
            f"agent-emails-alias:{mailbox_name.lower()}:{alias_email}",
        ))
        alias_sql += (
            "INSERT INTO aliases (id,mailbox_id,email,enabled,created_at) VALUES "
            f"({sql_literal(generated_alias_id)}::uuid,{sql_literal(mailbox_id)}::uuid,"
            f"{sql_literal(alias_email)},TRUE,now()) ON CONFLICT (mailbox_id,email) DO NOTHING;\n"
        )
        alias_sql += plpgsql_block(
            f"PERFORM 1 FROM aliases WHERE mailbox_id={sql_literal(mailbox_id)}::uuid "
            f"AND lower(email)={sql_literal(alias_email)} AND enabled=TRUE FOR UPDATE; "
            "IF NOT FOUND THEN RAISE EXCEPTION 'retained fixture alias is disabled or unavailable'; END IF;"
        )
        alias_id_sql = (
            "(SELECT id FROM aliases WHERE mailbox_id="
            f"{sql_literal(mailbox_id)}::uuid AND lower(email)={sql_literal(alias_email)} AND enabled=TRUE)"
        )
        planned, planned_sql = fixture_token_plan(
            mailbox_name=mailbox_name,
            master_key=master_key,
            alias_email=alias_email,
            token_name=alias.get("token_name") or f"fixture-{alias_email}",
            scopes=alias.get("token_scopes", ["read", "send"]),
            alias_id_sql=alias_id_sql,
        )
        minted.append(planned)
        token_sql += planned_sql
    # Alias validation/creation and every stable token rotation are one
    # transaction, so a failure cannot invalidate a previously emitted token.
    psql_exec(preflight + alias_sql + token_sql + "COMMIT;\n")
    return admin, minted


MAILBOX_PUBLIC_CONFIG_FIELDS = (
    "name", "imap_host", "imap_port", "imap_tls", "smtp_host", "smtp_port", "smtp_tls", "username",
)


def find_existing_mailbox(url: str, headers: dict[str, str], requested: dict) -> dict | None:
    req = urllib.request.Request(url, headers=headers, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            items = json.load(resp)
    except urllib.error.HTTPError as exc:
        exc.close()
        raise SystemExit(f"list mailboxes failed: HTTP {exc.code}; response withheld") from exc
    if not isinstance(items, list):
        raise SystemExit("list mailboxes returned an invalid response")
    name = str(requested.get("name", "")).strip()
    matches = [item for item in items if isinstance(item, dict) and str(item.get("name", "")).strip() == name]
    if len(matches) > 1:
        raise SystemExit(f"multiple mailboxes returned for fixture name {name!r}")
    if not matches:
        return None
    existing = matches[0]
    if existing.get("enabled") is not True:
        raise SystemExit(f"retained mailbox {name!r} is disabled")
    mismatches = [field for field in MAILBOX_PUBLIC_CONFIG_FIELDS if existing.get(field) != requested.get(field)]
    if mismatches:
        raise SystemExit(
            f"retained mailbox {name!r} configuration differs in non-secret field(s): {', '.join(mismatches)}"
        )
    mailbox_id = str(existing.get("id", "")).strip()
    if not mailbox_id:
        raise SystemExit(f"retained mailbox {name!r} has no ID")
    return {"mailbox_id": mailbox_id, "mailbox_name": name, "status": "existing"}


def seed_greenmail(fixture: dict) -> None:
    smtp_host = os.environ.get("GREENMAIL_SMTP_HOST", fixture["mailbox"]["smtp_host"])
    smtp_port = int(os.environ.get("GREENMAIL_SMTP_PORT", str(fixture["mailbox"]["smtp_port"])))
    smtp = smtplib.SMTP(smtp_host, smtp_port, timeout=20)
    try:
        for message in fixture.get("messages", []):
            msg = EmailMessage()
            from_name = message.get("from_name")
            from_email = message["from_email"]
            msg["From"] = f"{from_name} <{from_email}>" if from_name else from_email
            smtp_to = message.get("smtp_to") or ["primary@example.com"]
            msg["To"] = ", ".join(smtp_to)
            if message.get("delivered_to"):
                msg["Delivered-To"] = message["delivered_to"]
            msg["Subject"] = message.get("subject", "")
            msg["Message-ID"] = message["rfc_message_id"]
            sent_dt = iso_to_dt(message.get("sent_date")) or datetime.now(timezone.utc)
            msg["Date"] = format_datetime(sent_dt)
            text_plain = message.get("text_plain") or ""
            text_html = message.get("text_html")
            if text_html:
                msg.set_content(text_plain)
                msg.add_alternative(text_html, subtype="html")
            else:
                msg.set_content(text_plain)
            smtp.send_message(msg)
        print(json.dumps({"status": "ok", "seeded_greenmail_messages": len(fixture.get("messages", []))}, indent=2))
    finally:
        smtp.quit()


def sql_literal(value) -> str:
    if value is None:
        return "NULL"
    if isinstance(value, bool):
        return "TRUE" if value else "FALSE"
    if isinstance(value, (int, float)):
        return str(value)
    text = str(value).replace("'", "''")
    return f"'{text}'"


def psql_exec(sql: str) -> None:
    dsn = env("POSTGRES_DSN")
    proc = subprocess.run(
        ["psql", dsn, "-v", "ON_ERROR_STOP=1"],
        input=sql.encode("utf-8"),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr.decode("utf-8", errors="replace"))
        raise SystemExit(proc.returncode)


def seed_emaild_direct(fixture: dict) -> None:
    mailbox_name = fixture["mailbox"]["name"]
    query_sql = f"SELECT id FROM mailboxes WHERE name={sql_literal(mailbox_name)} LIMIT 1;"
    proc = subprocess.run(
        ["psql", env("POSTGRES_DSN"), "-At", "-c", query_sql],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr.decode("utf-8", errors="replace"))
        raise SystemExit(proc.returncode)
    mailbox_id = proc.stdout.decode("utf-8").strip()
    if not mailbox_id:
        raise SystemExit(f"mailbox not found for direct seed: {mailbox_name}. Run bootstrap first.")

    alias_emails = [alias["email"] for alias in fixture.get("aliases", [])]
    alias_lookup_sql = (
        "SELECT id, email FROM aliases WHERE mailbox_id="
        + sql_literal(mailbox_id)
        + " AND lower(email) = ANY(ARRAY["
        + ",".join(sql_literal(a.lower()) for a in alias_emails)
        + "]);"
    )
    alias_proc = subprocess.run(
        ["psql", env("POSTGRES_DSN"), "-At", "-F", "\t", "-c", alias_lookup_sql],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if alias_proc.returncode != 0:
        sys.stderr.write(alias_proc.stderr.decode("utf-8", errors="replace"))
        raise SystemExit(alias_proc.returncode)
    aliases = {}
    for line in alias_proc.stdout.decode("utf-8").splitlines():
        if not line.strip():
            continue
        alias_id, email = line.split("\t", 1)
        aliases[email.lower()] = alias_id

    missing = [email for email in alias_emails if email.lower() not in aliases]
    if missing:
        raise SystemExit(f"aliases missing for direct seed: {', '.join(missing)}. Run bootstrap first.")

    statements: list[str] = []
    for message in fixture.get("messages", []):
        folder_name = message.get("folder", "inbox")
        fid = folder_id(mailbox_name, folder_name)
        internal_date = iso_to_dt(message.get("internal_date")) or datetime.now(timezone.utc)
        sent_date = iso_to_dt(message.get("sent_date")) or internal_date
        raw = build_raw_message(message).decode("utf-8", errors="replace")
        raw_headers = (
            f"From: {message.get('from_name', '')} <{message['from_email']}>\n"
            f"Subject: {message.get('subject', '')}\n"
            f"Message-ID: {message['rfc_message_id']}"
        )
        text_plain = message.get("text_plain") or ""
        text_html = message.get("text_html")
        extracted_text = message.get("extracted_text") or text_plain
        snippet = message.get("snippet") or (text_plain[:160] if text_plain else None)
        digest = message.get("digest")

        statements.append(
            "INSERT INTO folders (id, mailbox_id, name, uidvalidity, last_seen_uid, updated_at) "
            f"VALUES ({sql_literal(fid)}, {sql_literal(mailbox_id)}, {sql_literal(folder_name)}, 1, {sql_literal(message.get('imap_uid', 1))}, now()) "
            "ON CONFLICT (mailbox_id, name) DO UPDATE SET last_seen_uid = GREATEST(folders.last_seen_uid, EXCLUDED.last_seen_uid), updated_at = now();"
        )
        statements.append(
            "INSERT INTO messages (id, mailbox_id, folder_id, imap_uid, uidvalidity, message_id, internal_date, sent_date, from_email, from_name, subject, snippet, raw_headers, has_attachments, spam_flag, body_state, in_reply_to, \"references\", created_at) "
            "VALUES ("
            f"{sql_literal(message['id'])}, {sql_literal(mailbox_id)}, {sql_literal(fid)}, {sql_literal(message.get('imap_uid', 1))}, (SELECT uidvalidity FROM folders WHERE id={sql_literal(fid)}), {sql_literal(message['rfc_message_id'])}, "
            f"{sql_literal(internal_date.isoformat())}, {sql_literal(sent_date.isoformat())}, {sql_literal(message['from_email'])}, {sql_literal(message.get('from_name'))}, "
            f"{sql_literal(message.get('subject'))}, {sql_literal(snippet)}, {sql_literal(raw_headers)}, FALSE, FALSE, 'fetched', {sql_literal(message.get('in_reply_to'))}, {sql_literal(message.get('references'))}, now()) "
            "ON CONFLICT (folder_id, uidvalidity, imap_uid) DO UPDATE SET "
            "message_id = EXCLUDED.message_id, internal_date = EXCLUDED.internal_date, sent_date = EXCLUDED.sent_date, from_email = EXCLUDED.from_email, from_name = EXCLUDED.from_name, "
            "subject = EXCLUDED.subject, snippet = EXCLUDED.snippet, raw_headers = EXCLUDED.raw_headers, body_state = EXCLUDED.body_state;"
        )
        for alias_email in message.get("audience_aliases", []):
            alias_id = aliases[alias_email.lower()]
            statements.append(
                "INSERT INTO message_audience (message_id, alias_id) VALUES "
                f"({sql_literal(message['id'])}, {sql_literal(alias_id)}) ON CONFLICT DO NOTHING;"
            )
        statements.append(
            "INSERT INTO message_bodies (message_id, text_plain, text_html, extracted_text, fetched_at) VALUES "
            f"({sql_literal(message['id'])}, {sql_literal(text_plain)}, {sql_literal(text_html)}, {sql_literal(extracted_text)}, now()) "
            "ON CONFLICT (message_id) DO UPDATE SET text_plain = EXCLUDED.text_plain, text_html = EXCLUDED.text_html, extracted_text = EXCLUDED.extracted_text, fetched_at = now();"
        )
        statements.append(
            "INSERT INTO message_raw (message_id, raw_rfc822, created_at) VALUES "
            f"({sql_literal(message['id'])}, convert_to({sql_literal(raw)}, 'UTF8'), now()) "
            "ON CONFLICT (message_id) DO UPDATE SET raw_rfc822 = EXCLUDED.raw_rfc822;"
        )
        if digest:
            digest_json = json.dumps(digest.get("extracted_entities", {}), separators=(",", ":"))
            bullets = ",".join(sql_literal(b) for b in digest.get("summary_bullets", []))
            statements.append(
                "INSERT INTO message_digest (message_id, model_name, summary_short, summary_bullets, intent, priority, required_action, suggested_next_step, extracted_entities, confidence, created_at) VALUES "
                f"({sql_literal(message['id'])}, {sql_literal(digest.get('model_name', 'seed-model'))}, {sql_literal(digest.get('summary_short', ''))}, ARRAY[{bullets}], "
                f"{sql_literal(digest.get('intent', 'inform'))}, {sql_literal(digest.get('priority', 'low'))}, {sql_literal(digest.get('required_action', 'none'))}, "
                f"{sql_literal(digest.get('suggested_next_step', 'archive'))}, {sql_literal(digest_json)}::jsonb, {sql_literal(digest.get('confidence', 1.0))}, now()) "
                "ON CONFLICT (message_id) DO UPDATE SET model_name = EXCLUDED.model_name, summary_short = EXCLUDED.summary_short, summary_bullets = EXCLUDED.summary_bullets, "
                "intent = EXCLUDED.intent, priority = EXCLUDED.priority, required_action = EXCLUDED.required_action, suggested_next_step = EXCLUDED.suggested_next_step, extracted_entities = EXCLUDED.extracted_entities, confidence = EXCLUDED.confidence, created_at = now();"
            )
        for alias_email in message.get("seen_aliases", []):
            alias_id = aliases[alias_email.lower()]
            statements.append(
                "INSERT INTO message_ack (message_id, alias_id, created_at) VALUES "
                f"({sql_literal(message['id'])}, {sql_literal(alias_id)}, now()) ON CONFLICT DO NOTHING;"
            )
        for emb in message.get("embeddings", []):
            vec = emb.get("vector") or []
            if not vec:
                continue
            vector_literal = "[" + ",".join(f"{float(v):.8f}" for v in vec) + "]"
            statements.append(
                "INSERT INTO message_embeddings (message_id,segment,chunk_index,model_name,source_text,embedding,created_at) VALUES "
                f"({sql_literal(message['id'])}, {sql_literal(emb.get('segment', 'body'))}, {sql_literal(int(emb.get('chunk_index', 0)))}, "
                f"{sql_literal(emb.get('model_name', 'fixture-embed-v1'))}, {sql_literal(emb.get('source_text', extracted_text))}, "
                f"{sql_literal(vector_literal)}::vector, now()) "
                "ON CONFLICT (message_id,segment,chunk_index) DO UPDATE SET model_name=EXCLUDED.model_name, source_text=EXCLUDED.source_text, embedding=EXCLUDED.embedding, created_at=now();"
            )

    psql_exec("\n".join(statements) + "\n")
    print(json.dumps({"status": "ok", "seeded_emaild_messages": len(fixture.get("messages", [])), "mailbox": mailbox_name}, indent=2))


def bootstrap_gateway(fixture: dict) -> None:
    gateway = fixture.get("gateway")
    if not gateway:
        raise SystemExit("fixture does not define gateway setup")
    master_key = env("MASTER_KEY")
    postgres_dsn = env("POSTGRES_DSN")
    encryption_key = env("UIGW_TOKEN_ENCRYPTION_KEY")
    app_password_pepper = env("UIGW_APP_PASSWORD_PEPPER")
    session_cookie_name = os.environ.get("UIGW_SESSION_COOKIE", "UIGW_SESSION")
    session_ttl_hours = int(os.environ.get("UIGW_SESSION_TTL_HOURS", "336"))

    mailbox_name = fixture["mailbox"]["name"]
    alias_email = gateway["shared_mailbox"]["alias_email"].lower()
    proc = subprocess.run(
        ["psql", postgres_dsn, "-At", "-F", "\t", "-c", f"SELECT m.id, a.id FROM mailboxes m JOIN aliases a ON a.mailbox_id=m.id WHERE m.name={sql_literal(mailbox_name)} AND lower(a.email)={sql_literal(alias_email)} LIMIT 1;"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr.decode("utf-8", errors="replace"))
        raise SystemExit(proc.returncode)
    row = proc.stdout.decode("utf-8").strip()
    if not row:
        raise SystemExit(f"gateway bootstrap requires mailbox={mailbox_name} alias={alias_email}; run bootstrap first")
    mailbox_id, alias_id = row.split("\t", 1)

    alias_token = secrets.token_urlsafe(24)
    alias_token_id = str(uuid.uuid5(uuid.NAMESPACE_URL, f"agent-emails-gateway-credential-token:{mailbox_name.lower()}:{alias_email}"))
    alias_token_hash = hash_token(master_key, alias_token)

    encrypted = node_encrypt_token(alias_token, encryption_key)
    user = gateway["user"]
    team = gateway["team"]
    shared_mailbox = gateway["shared_mailbox"]
    session = gateway["session"]
    app_password_cfg = gateway["app_password"]
    credential_id = str(uuid.uuid4())
    app_password_secret = secrets.token_urlsafe(18)
    app_password_hash = hash_app_password(app_password_secret, app_password_pepper)

    expires_at = datetime.now(timezone.utc).timestamp() + session_ttl_hours * 3600
    expires_iso = datetime.fromtimestamp(expires_at, tz=timezone.utc).isoformat()

    sql = f"""
INSERT INTO tokens (id,alias_id,name,token_hash,scopes,created_at,revoked_at)
VALUES ({sql_literal(alias_token_id)}, {sql_literal(alias_id)}, 'fixture-gateway-credential', {sql_literal(alias_token_hash)}, ARRAY['read','send'], now(), NULL)
ON CONFLICT (id) DO UPDATE SET token_hash=EXCLUDED.token_hash, scopes=EXCLUDED.scopes, revoked_at=NULL;

INSERT INTO gateway_users (id,email,status,created_at,updated_at)
VALUES ({sql_literal(user['id'])}, {sql_literal(user['email'].lower())}, 'active', now(), now())
ON CONFLICT (id) DO UPDATE SET email=EXCLUDED.email, status='active', updated_at=now();

INSERT INTO teams (id,name,created_at)
VALUES ({sql_literal(team['id'])}, {sql_literal(team['name'])}, now())
ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name;

INSERT INTO team_members (team_id,user_id,role,created_at)
VALUES ({sql_literal(team['id'])}, {sql_literal(user['id'])}, 'admin', now())
ON CONFLICT (team_id,user_id) DO UPDATE SET role='admin';

INSERT INTO gateway_mailbox_credentials (id,token_ciphertext,iv,auth_tag,key_version,created_by,created_at,last_validated_at)
VALUES ({sql_literal(credential_id)}, {sql_literal(encrypted['tokenCiphertext'])}, {sql_literal(encrypted['iv'])}, {sql_literal(encrypted['authTag'])}, 1, {sql_literal(user['id'])}, now(), now())
ON CONFLICT (id) DO UPDATE SET token_ciphertext=EXCLUDED.token_ciphertext, iv=EXCLUDED.iv, auth_tag=EXCLUDED.auth_tag, key_version=EXCLUDED.key_version, created_by=EXCLUDED.created_by, last_validated_at=now();

INSERT INTO shared_mailboxes (id,team_id,mailbox_name,alias_email,credential_id,can_read,can_send,can_ack,enabled,created_at)
VALUES ({sql_literal(shared_mailbox['id'])}, {sql_literal(team['id'])}, {sql_literal(shared_mailbox['mailbox_name'])}, {sql_literal(shared_mailbox['alias_email'].lower())}, {sql_literal(credential_id)}, {sql_literal(shared_mailbox.get('can_read', True))}, {sql_literal(shared_mailbox.get('can_send', True))}, {sql_literal(shared_mailbox.get('can_ack', True))}, TRUE, now())
ON CONFLICT (id) DO UPDATE SET credential_id=EXCLUDED.credential_id, can_read=EXCLUDED.can_read, can_send=EXCLUDED.can_send, can_ack=EXCLUDED.can_ack, enabled=TRUE;

INSERT INTO gateway_sessions (id,user_id,csrf_token,expires_at,revoked_at,created_at,last_seen_at)
VALUES ({sql_literal(session['id'])}, {sql_literal(user['id'])}, {sql_literal(session['csrf_token'])}, {sql_literal(expires_iso)}, NULL, now(), now())
ON CONFLICT (id) DO UPDATE SET user_id=EXCLUDED.user_id, csrf_token=EXCLUDED.csrf_token, expires_at=EXCLUDED.expires_at, revoked_at=NULL, last_seen_at=now();

INSERT INTO gateway_app_passwords (id,user_id,shared_mailbox_id,label,password_hash,created_at,last_used_at,revoked_at)
VALUES ({sql_literal(app_password_cfg['id'])}, {sql_literal(user['id'])}, {sql_literal(shared_mailbox['id'])}, {sql_literal(app_password_cfg['label'])}, {sql_literal(app_password_hash)}, now(), NULL, NULL)
ON CONFLICT (id) DO UPDATE SET label=EXCLUDED.label, password_hash=EXCLUDED.password_hash, revoked_at=NULL, last_used_at=NULL;
"""
    psql_exec(sql)
    print(
        json.dumps(
            {
                "status": "ok",
                "gateway": {
                    "user_id": user["id"],
                    "user_email": user["email"].lower(),
                    "team_id": team["id"],
                    "shared_mailbox_id": shared_mailbox["id"],
                    "credential_id": credential_id,
                    "session_cookie_name": session_cookie_name,
                    "session_cookie_value": session["id"],
                    "csrf_token": session["csrf_token"],
                    "app_password_id": app_password_cfg["id"],
                    "app_password_secret": app_password_secret,
                    "app_password_label": app_password_cfg["label"],
                    "mail_client_username": app_password_cfg["id"],
                    "mail_client_password": app_password_secret,
                    "mail_client_alias": alias_email,
                    "emaild_alias_token": alias_token,
                },
            },
            indent=2,
        )
    )


def embed_text(text: str) -> list[float]:
    base_url = env("OPENAI_BASE_URL").rstrip("/")
    model = env("EMBEDDING_MODEL_NAME")
    api_key = os.environ.get("OPENAI_API_KEY", "").strip()
    payload = json.dumps({"model": model, "input": [text]}).encode("utf-8")
    req = urllib.request.Request(
        base_url + "/v1/embeddings",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    if api_key:
        req.add_header("Authorization", f"Bearer {api_key}")
    with urllib.request.urlopen(req, timeout=60) as resp:
        out = json.load(resp)
    data = out.get("data") or []
    if not data or not data[0].get("embedding"):
        raise SystemExit("embedding response missing vector")
    return data[0]["embedding"]


def seed_embeddings(fixture: dict) -> None:
    postgres_dsn = env("POSTGRES_DSN")
    model = env("EMBEDDING_MODEL_NAME")
    mailbox_name = fixture["mailbox"]["name"]
    proc = subprocess.run(
        [
            "psql",
            postgres_dsn,
            "-At",
            "-F",
            "\t",
            "-c",
            (
                "SELECT m.id, COALESCE(b.extracted_text,''), COALESCE(md.summary_short,''), COALESCE(m.subject,'') "
                "FROM messages m "
                "JOIN mailboxes mb ON mb.id=m.mailbox_id "
                "LEFT JOIN message_bodies b ON b.message_id=m.id "
                "LEFT JOIN message_digest md ON md.message_id=m.id "
                f"WHERE mb.name={sql_literal(mailbox_name)} ORDER BY m.internal_date DESC, m.id DESC;"
            ),
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr.decode("utf-8", errors="replace"))
        raise SystemExit(proc.returncode)
    rows = [line.split("\t", 3) for line in proc.stdout.decode("utf-8").splitlines() if line.strip()]
    statements = []
    count = 0
    for message_id, extracted_text, summary_short, subject in rows:
        text = "\n\n".join(part for part in [subject.strip(), summary_short.strip(), extracted_text.strip()] if part)
        if not text:
            continue
        vector = embed_text(text)
        vector_literal = "[" + ",".join(f"{float(v):.8f}" for v in vector) + "]"
        statements.append(
            "INSERT INTO message_embeddings (message_id,segment,chunk_index,model_name,source_text,embedding,created_at) VALUES "
            f"({sql_literal(message_id)}, 'body', 0, {sql_literal(model)}, {sql_literal(text)}, {sql_literal(vector_literal)}::vector, now()) "
            "ON CONFLICT (message_id,segment,chunk_index) DO UPDATE SET model_name=EXCLUDED.model_name, source_text=EXCLUDED.source_text, embedding=EXCLUDED.embedding, created_at=now();"
        )
        count += 1
    if statements:
        psql_exec("\n".join(statements) + "\n")
    print(json.dumps({"status": "ok", "embedded_messages": count, "embedding_model": model}, indent=2))


def main() -> None:
    parser = argparse.ArgumentParser(description="Seed reusable E2E fixtures into GreenMail and/or emaild.")
    parser.add_argument("mode", choices=["bootstrap", "greenmail", "emaild", "gateway", "embeddings", "all"])
    parser.add_argument(
        "--fixture",
        default=os.environ.get("E2E_FIXTURE_PATH", "scripts/fixtures/e2e-seed.synthetic.json"),
        help="Path to fixture JSON",
    )
    args = parser.parse_args()

    fixture = load_fixture(args.fixture)

    if args.mode in ("bootstrap", "all"):
        bootstrap_emaild(fixture)
    if args.mode in ("greenmail", "all"):
        seed_greenmail(fixture)
    if args.mode in ("emaild", "all"):
        seed_emaild_direct(fixture)
    if args.mode in ("gateway", "all"):
        bootstrap_gateway(fixture)
    if args.mode in ("embeddings", "all"):
        seed_embeddings(fixture)


if __name__ == "__main__":
    main()
