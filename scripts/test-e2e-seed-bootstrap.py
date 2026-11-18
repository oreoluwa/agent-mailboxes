#!/usr/bin/env python3
import contextlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import io
import json
import os
from pathlib import Path
import threading
import unittest
from unittest.mock import patch
import urllib.parse


def load_seed_module():
    path = Path(__file__).with_name("e2e-seed.py")
    spec = importlib.util.spec_from_file_location("e2e_seed", path)
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


seed = load_seed_module()


class BootstrapAPI(BaseHTTPRequestHandler):
    mailbox = None
    stored_password = None
    mailbox_posts = 0
    alias_posts = 0
    aliases = {}
    reject_mailbox_create = False

    def log_message(self, _format, *_args):
        pass

    def send_json(self, status, value):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def read_json(self):
        return json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))

    def do_GET(self):
        route = urllib.parse.urlsplit(self.path)
        if route.path == "/v1/admin/mailboxes":
            self.send_json(200, [] if self.mailbox is None else [self.mailbox])
            return
        if route.path == "/v1/admin/aliases":
            self.send_json(200, [self.aliases[email] for email in sorted(self.aliases)])
            return
        self.send_json(404, {"error": "not found"})

    def do_POST(self):
        route = urllib.parse.urlsplit(self.path)
        if route.path == "/v1/admin/mailboxes":
            incoming = self.read_json()
            type(self).mailbox_posts += 1
            if self.mailbox is not None or self.reject_mailbox_create:
                self.send_json(400, {"error": "duplicate"})
                return
            type(self).stored_password = "enc:v1:synthetic-ciphertext"
            type(self).mailbox = {
                "id": "10000000-0000-0000-0000-000000000001",
                **{field: incoming[field] for field in seed.MAILBOX_PUBLIC_CONFIG_FIELDS},
                "enabled": True,
            }
            self.send_json(200, {"status": "ok", "mailbox_id": self.mailbox["id"]})
            return
        if route.path == "/v1/admin/mint-token":
            self.read_json()
            self.send_json(200, {"token": "admin", "token_id": "admin-id"})
            return
        if route.path == "/v1/admin/aliases":
            email = self.read_json()["email"].strip().lower()
            type(self).alias_posts += 1
            if email in self.aliases:
                self.send_json(400, {"error": "duplicate"})
                return
            type(self).aliases[email] = {
                "id": "20000000-0000-0000-0000-" + str(len(self.aliases) + 1).zfill(12),
                "email": email,
                "enabled": True,
            }
            self.send_json(200, {"status": "ok"})
            return
        self.send_json(404, {"error": "not found"})


class SeedBootstrapTests(unittest.TestCase):
    def setUp(self):
        BootstrapAPI.mailbox = None
        BootstrapAPI.stored_password = None
        BootstrapAPI.mailbox_posts = 0
        BootstrapAPI.alias_posts = 0
        BootstrapAPI.aliases = {}
        BootstrapAPI.reject_mailbox_create = False
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), BootstrapAPI)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.fixture = {
            "mailbox": {
                "name": "primary",
                "imap_host": "imap.example.test",
                "imap_port": 993,
                "imap_tls": True,
                "smtp_host": "smtp.example.test",
                "smtp_port": 465,
                "smtp_tls": True,
                "username": "primary@example.test",
                "password": "fixture-password",
            },
            "aliases": [{"email": "agent@example.test"}],
        }
        self.env = {
            "EMAIL_API": f"http://127.0.0.1:{self.server.server_port}",
            "MASTER_KEY": "master",
            "POSTGRES_DSN": "postgres://synthetic.invalid/fixture",
        }
        self.sql_calls = []
        self.committed_sql = []
        self.reject_sql = False

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def run_bootstrap(self):
        output = io.StringIO()
        with patch.dict(os.environ, self.env, clear=False), \
                patch.object(seed, "psql_exec", side_effect=self.execute_sql), \
                contextlib.redirect_stdout(output):
            seed.bootstrap_emaild_via_api(self.fixture)
        return json.loads(output.getvalue())

    def execute_sql(self, sql):
        self.sql_calls.append(sql)
        if self.reject_sql:
            raise SystemExit("synthetic transaction rejected")
        self.committed_sql.append(sql)

    def test_repeat_bootstrap_reuses_mailbox_and_alias_without_rewriting_password(self):
        first = self.run_bootstrap()
        stored = BootstrapAPI.stored_password
        second = self.run_bootstrap()
        self.assertEqual(first["mailbox_id"], second["mailbox_id"])
        self.assertEqual(BootstrapAPI.mailbox_posts, 1)
        self.assertEqual(BootstrapAPI.alias_posts, 0)
        self.assertEqual(BootstrapAPI.stored_password, stored)
        self.assertEqual(first["aliases"][0]["token_id"], second["aliases"][0]["token_id"])
        self.assertNotEqual(first["aliases"][0]["token"], second["aliases"][0]["token"])
        self.assertEqual(len(self.sql_calls), 2)
        self.assertTrue(all("UPDATE tokens SET revoked_at=now()" in sql for sql in self.sql_calls))
        self.assertTrue(all("ON CONFLICT (id) DO UPDATE" in sql for sql in self.sql_calls))
        self.assertTrue(all(sql.count("BEGIN;") == 1 and sql.count("COMMIT;") == 1 for sql in self.sql_calls))
        self.assertTrue(all("FOR UPDATE" in sql for sql in self.sql_calls))
        self.assertTrue(all("INSERT INTO aliases" in sql for sql in self.sql_calls))
        self.assertIn("name='fixture-admin:primary'", self.sql_calls[0])
        self.assertNotIn("fixture-admin:other", "\n".join(self.sql_calls))

    def test_database_boundary_is_required_before_mailbox_mutation(self):
        environment = {"EMAIL_API": self.env["EMAIL_API"], "MASTER_KEY": "master"}
        with patch.dict(os.environ, environment, clear=True):
            with self.assertRaisesRegex(SystemExit, "POSTGRES_DSN"):
                seed.bootstrap_emaild_via_api(self.fixture)
        self.assertEqual(BootstrapAPI.mailbox_posts, 0)

    def test_retained_nonsecret_config_drift_fails_without_posting(self):
        self.run_bootstrap()
        self.fixture["mailbox"]["imap_host"] = "changed.example.test"
        with self.assertRaisesRegex(SystemExit, "imap_host"):
            self.run_bootstrap()
        self.assertEqual(BootstrapAPI.mailbox_posts, 1)

    def test_retained_disabled_mailbox_fails_without_posting(self):
        self.run_bootstrap()
        BootstrapAPI.mailbox["enabled"] = False
        with self.assertRaisesRegex(SystemExit, "is disabled"):
            self.run_bootstrap()
        self.assertEqual(BootstrapAPI.mailbox_posts, 1)

    def test_failed_atomic_identity_transaction_emits_no_replacement_credentials(self):
        previous = self.run_bootstrap()
        committed = list(self.committed_sql)
        self.reject_sql = True
        with self.assertRaisesRegex(SystemExit, "transaction rejected"):
            self.run_bootstrap()
        self.assertEqual(self.committed_sql, committed)
        self.assertEqual(len(self.sql_calls), 2)
        self.assertIn("enabled=TRUE FOR UPDATE", self.sql_calls[-1])
        self.assertTrue(previous["admin_token"])

    def test_direct_bootstrap_is_rejected_before_any_side_effect(self):
        with patch.dict(os.environ, {"BOOTSTRAP_DIRECT": "1", "MASTER_KEY": "synthetic", "MAILBOX_PASSWORD_ENVELOPE": "enc:v1:synthetic", "POSTGRES_DSN": "postgres://synthetic.invalid/fixture"}, clear=True), \
                patch.object(seed, "psql_exec") as sql, \
                patch.object(seed.urllib.request, "urlopen") as http:
            with self.assertRaisesRegex(SystemExit, "direct bootstrap is no longer supported"):
                seed.bootstrap_emaild(self.fixture)
            sql.assert_not_called()
            http.assert_not_called()

    def test_unrelated_create_error_is_not_treated_as_duplicate(self):
        BootstrapAPI.reject_mailbox_create = True
        with self.assertRaisesRegex(SystemExit, "bootstrap mailbox failed: HTTP 400"):
            self.run_bootstrap()
        self.assertEqual(BootstrapAPI.mailbox_posts, 1)

    def test_unrelated_list_error_is_not_treated_as_duplicate(self):
        with patch.object(seed.urllib.request, "urlopen", side_effect=seed.urllib.error.HTTPError(
            "url", 400, "bad request", {}, None
        )):
            with self.assertRaisesRegex(SystemExit, "list mailboxes failed: HTTP 400"):
                self.run_bootstrap()


if __name__ == "__main__":
    unittest.main()
