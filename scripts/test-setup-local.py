#!/usr/bin/env python3
from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


def load_module():
    path = Path(__file__).with_name("setup-local.py")
    spec = importlib.util.spec_from_file_location("setup_local", path)
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


setup = load_module()
ROOT = Path(__file__).resolve().parents[1]


class LocalSetupTests(unittest.TestCase):
    def test_rejects_checkout_and_nested_installations_before_writing_secrets(self):
        with tempfile.TemporaryDirectory() as parent:
            checkout = Path(parent) / "checkout"
            checkout.mkdir()
            (checkout / "Dockerfile").write_text("FROM scratch\n")
            (checkout / "migrations").mkdir()
            link = Path(parent) / "linked-checkout"
            link.symlink_to(checkout, target_is_directory=True)
            for directory in (checkout, checkout / "install", link / "install"):
                with self.subTest(directory=directory), patch.object(setup.secrets, "token_hex") as generate:
                    with self.assertRaisesRegex(setup.SetupError, "outside the build checkout"):
                        setup.init_installation(directory, checkout, 18787)
                    generate.assert_not_called()
            self.assertEqual({p.name for p in checkout.iterdir()}, {"Dockerfile", "migrations"})

    def test_start_rejects_existing_installation_inside_checkout(self):
        with tempfile.TemporaryDirectory() as parent:
            directory = Path(parent) / "install"
            setup.init_installation(directory, ROOT, 18787)
            # Simulate a valid legacy installation inside its configured checkout.
            (directory / "Dockerfile").write_text("FROM scratch\n")
            (directory / "migrations").mkdir()
            path = directory / setup.MANIFEST_NAME
            manifest = json.loads(path.read_text())
            manifest["checkout"] = str(directory)
            path.write_text(json.dumps(manifest))
            with patch.object(setup.subprocess, "run") as run:
                with self.assertRaisesRegex(setup.SetupError, "outside the build checkout"):
                    setup.compose_command(directory, "start")
                run.assert_not_called()
                self.assertEqual(setup.compose_command(directory, "status"), run.return_value.returncode)

    def test_init_is_private_persistent_and_idempotent(self):
        with tempfile.TemporaryDirectory() as parent:
            directory = Path(parent) / "install"
            first = setup.init_installation(directory, ROOT, 18787)
            before = {path.name: path.read_bytes() for path in directory.iterdir()}
            second = setup.init_installation(directory, ROOT, 18787)
            after = {path.name: path.read_bytes() for path in directory.iterdir()}
            self.assertEqual(first, second)
            self.assertEqual(before, after)
            self.assertEqual(directory.stat().st_mode & 0o777, 0o700)
            for path in directory.iterdir():
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            compose = (directory / "compose.yml").read_text()
            self.assertIn("postgres-data:/var/lib/postgresql/data", compose)
            self.assertIn("restart: unless-stopped", compose)
            self.assertIn('127.0.0.1:18787:8787', compose)
            self.assertIn('command: ["/usr/local/bin/app", "serve", "--all-mailboxes"]', compose)
            self.assertNotIn("tmpfs", compose)
            self.assertNotIn("5432:5432", compose)
            env = (directory / "emaild.env").read_text()
            for setting in ("INITIAL_SYNC_MAX_MESSAGES=0", "SYNC_FOLDERS=INBOX", "SUMMARY_ENABLED=false"):
                self.assertIn(setting, env)
            master_line = next(line for line in env.splitlines() if line.startswith("MASTER_KEY="))
            self.assertEqual((directory / "master.key").read_text().strip(), master_line.split("=", 1)[1])

    def test_tampered_project_identity_cannot_control_other_services(self):
        with tempfile.TemporaryDirectory() as parent:
            directory = Path(parent) / "install"
            manifest = setup.init_installation(directory, ROOT, 18787)
            manifest["project_name"] = "unrelated-project"
            (directory / setup.MANIFEST_NAME).write_text(json.dumps(manifest))
            with patch.object(setup.subprocess, "run") as run:
                with self.assertRaises(setup.SetupError):
                    setup.compose_command(directory, "stop")
                run.assert_not_called()

    def test_init_prints_no_secrets(self):
        with tempfile.TemporaryDirectory() as parent:
            directory = Path(parent) / "install"
            output = io.StringIO()
            with patch("sys.argv", ["setup-local.py", "init", str(directory), "--checkout", str(ROOT)]), contextlib.redirect_stdout(output):
                self.assertEqual(setup.main(), 0)
            rendered = output.getvalue()
            env = (directory / "emaild.env").read_text().splitlines()
            for line in env:
                if any(line.startswith(prefix) for prefix in ("MASTER_KEY=", "MAILBOX_CREDENTIAL_KEY=", "POSTGRES_DSN=")):
                    self.assertNotIn(line.split("=", 1)[1], rendered)

    def test_unknown_or_changed_directory_is_not_modified(self):
        with tempfile.TemporaryDirectory() as parent:
            unknown = Path(parent) / "unknown"
            unknown.mkdir(mode=0o700)
            marker = unknown / "keep.txt"
            marker.write_text("keep")
            with self.assertRaises(setup.SetupError):
                setup.init_installation(unknown, ROOT, 8787)
            self.assertEqual(marker.read_text(), "keep")

            directory = Path(parent) / "install"
            setup.init_installation(directory, ROOT, 8787)
            before = (directory / "emaild.env").read_bytes()
            with self.assertRaisesRegex(setup.SetupError, "api_port"):
                setup.init_installation(directory, ROOT, 8788)
            self.assertEqual((directory / "emaild.env").read_bytes(), before)

    def test_compose_commands_never_delete_volume(self):
        with tempfile.TemporaryDirectory() as parent:
            directory = Path(parent) / "install"
            setup.init_installation(directory, ROOT, 8787)
            calls = []

            def run(args, **kwargs):
                calls.append(args)
                return type("Result", (), {"returncode": 0})()

            with patch.object(setup.subprocess, "run", side_effect=run):
                for action in ("start", "status", "stop"):
                    self.assertEqual(setup.compose_command(directory, action), 0)
            flattened = [part for call in calls for part in call]
            self.assertNotIn("down", flattened)
            self.assertNotIn("-v", flattened)
            self.assertEqual(calls[-1][-1], "stop")

    def test_manifest_must_remain_private(self):
        with tempfile.TemporaryDirectory() as parent:
            directory = Path(parent) / "install"
            setup.init_installation(directory, ROOT, 8787)
            manifest = directory / setup.MANIFEST_NAME
            manifest.chmod(0o644)
            with self.assertRaises(setup.SetupError):
                setup.load_manifest(directory)

    def test_init_rejects_symlink_directory(self):
        with tempfile.TemporaryDirectory() as parent:
            actual = Path(parent) / "actual"
            actual.mkdir(mode=0o700)
            linked = Path(parent) / "linked"
            linked.symlink_to(actual, target_is_directory=True)
            with self.assertRaisesRegex(setup.SetupError, "real directory"):
                setup.init_installation(linked, ROOT, 8787)
            self.assertEqual(list(actual.iterdir()), [])

    @unittest.skipUnless(hasattr(os, "mkfifo"), "FIFO creation unavailable")
    def test_managed_file_reader_rejects_fifo_without_blocking(self):
        with tempfile.TemporaryDirectory() as parent:
            fifo = Path(parent) / "credential.pipe"
            os.mkfifo(fifo, 0o600)
            with self.assertRaisesRegex(setup.SetupError, "regular file"):
                setup.private_read(fifo)


if __name__ == "__main__":
    unittest.main()
