#!/usr/bin/env python3
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location(
    "package_release", Path(__file__).with_name("package-release.py")
)
PACKAGE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PACKAGE)


class PackageReleaseTests(unittest.TestCase):
    def test_version_requires_prerelease_semver(self):
        for value in ("0.1.0-rc.1", "1.2.3-0", "1.2.3-rc.0", "1.2.3-01a"):
            self.assertEqual(PACKAGE.validate_version(value), value)
        for value in ("0.1.0", "latest", "0.1.0/../../bad", "0.1.0-01", "1.2.3-rc.01"):
            with self.subTest(value=value), self.assertRaises(argparse.ArgumentTypeError):
                PACKAGE.validate_version(value)

    def test_packaging_accepts_visibility_and_approval_independently(self):
        with tempfile.TemporaryDirectory() as folder:
            source = Path(folder)
            for visibility in ("private", "public"):
                for approved in (False, True):
                    policy = {"visibility": visibility, "license": "MIT", "publication_allowed": approved}
                    (source / "release-policy.json").write_text(json.dumps(policy))
                    self.assertEqual(PACKAGE.load_release_policy(source), policy)
            for policy in (
                {"visibility": "unknown", "license": "MIT", "publication_allowed": False},
                {"visibility": "public", "license": "MIT", "publication_allowed": "false"},
                {"visibility": "public", "license": "MIT"},
                [],
            ):
                (source / "release-policy.json").write_text(json.dumps(policy))
                with self.assertRaises(PACKAGE.PackageError):
                    PACKAGE.load_release_policy(source)

    def test_runtime_payload_requires_migrations_and_helpers(self):
        with tempfile.TemporaryDirectory() as folder:
            source = Path(folder)
            metadata = {"version": "0.1.0-rc.1"}
            binaries = {"email": b"email", "emaild": b"emaild"}
            with self.assertRaisesRegex(PACKAGE.PackageError, "migrations"):
                PACKAGE.runtime_files(source, metadata, binaries)
            (source / "migrations").mkdir()
            (source / "migrations/001.sql").write_text("SELECT 1;\n")
            (source / "scripts").mkdir()
            for helper in ("mailbox-onboard.py", "onboard-agent.py"):
                with self.assertRaisesRegex(PACKAGE.PackageError, helper):
                    PACKAGE.runtime_files(source, metadata, binaries)
                (source / "scripts" / helper).write_text("# helper\n")
            for name in ("LICENSE", "THIRD_PARTY_NOTICES.md", "third_party/go/example/LICENSE"):
                path = source / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("upstream copyright and terms\n")
            files = PACKAGE.runtime_files(source, metadata, binaries)
            for name in ("LICENSE", "THIRD_PARTY_NOTICES.md", "third_party/go/example/LICENSE"):
                self.assertEqual(files[name][0], b"upstream copyright and terms\n")
            self.assertIn("migrations/001.sql", files)
            self.assertIn("scripts/onboard-agent.py", files)
            self.assertIn(b"Building this bundle does not publish it", files["BUNDLE-README.md"][0])

    def test_normalized_bundle_contains_metadata(self):
        with tempfile.TemporaryDirectory() as folder:
            archive = Path(folder) / "bundle.tar.gz"
            PACKAGE.normalized_tar_gz(archive, "agent-emails-test", {
                "VERSION.json": (b'{"version":"test"}\n', 0o644),
                "bin/email": (b"binary", 0o755),
            }, 1700000000)
            with tarfile.open(archive) as contents:
                names = {item.name: item for item in contents.getmembers()}
                self.assertIn("agent-emails-test/VERSION.json", names)
                self.assertEqual(names["agent-emails-test/bin/email"].mode, 0o755)

    def test_source_archive_uses_git_and_excludes_untracked_files(self):
        with tempfile.TemporaryDirectory() as folder:
            repo = Path(folder) / "repo"
            repo.mkdir()
            subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
            (repo / "tracked.txt").write_text("tracked\n")
            (repo / "LICENSE").write_text("project terms\n")
            (repo / "third_party").mkdir()
            (repo / "third_party/NOTICE").write_text("upstream attribution\n")
            (repo / "private.md").write_text("private planning note\n")
            (repo / "notes/internal").mkdir(parents=True)
            (repo / "notes/internal/note.md").write_text("internal fixture note\n")
            (repo / ".gitattributes").write_text("private.md export-ignore\nnotes/internal export-ignore\n")
            subprocess.run(["git", "add", "tracked.txt", "LICENSE", "third_party/NOTICE", "private.md", ".gitattributes", "notes/internal/note.md"], cwd=repo, check=True)
            subprocess.run([
                "git", "-c", "user.name=Test", "-c", "user.email=test@example.test",
                "-c", "commit.gpgsign=false", "commit", "-qm", "fixture",
            ], cwd=repo, check=True)
            (repo / "untracked.txt").write_text("untracked\n")
            output = Path(folder) / "output"
            output.mkdir()
            with mock.patch.object(PACKAGE, "ROOT", repo):
                archive = PACKAGE.write_source_archive(
                    PACKAGE.resolve_commit("HEAD"), output, "0.1.0-rc.1", 1700000000
                )
            self.assertEqual(archive.name, "agent-emails-0.1.0-rc.1-source.tar.gz")
            with tarfile.open(archive) as contents:
                names = {item.name for item in contents.getmembers()}
            self.assertTrue(any(name.endswith("/tracked.txt") for name in names))
            self.assertTrue(any(name.endswith("/LICENSE") for name in names))
            self.assertTrue(any(name.endswith("/third_party/NOTICE") for name in names))
            self.assertFalse(any(name.endswith("/untracked.txt") for name in names))
            self.assertFalse(any(name.endswith("/private.md") for name in names))
            self.assertFalse(any("/notes/internal" in name for name in names))

    def test_main_failure_leaves_no_completed_output(self):
        with tempfile.TemporaryDirectory() as folder:
            output = Path(folder) / "release"
            args = argparse.Namespace(
                version="0.1.0-rc.1", ref="HEAD", output=output,
                targets=["linux/amd64"], source=False,
            )
            with mock.patch.object(PACKAGE, "parse_args", return_value=args), \
                    mock.patch.object(PACKAGE, "resolve_commit", return_value="a" * 40), \
                    mock.patch.object(PACKAGE, "require_clean_head"), \
                    mock.patch.object(PACKAGE, "commit_epoch", return_value=1700000000), \
                    mock.patch.object(PACKAGE, "archive_bytes", side_effect=PACKAGE.PackageError("synthetic failure")):
                self.assertEqual(PACKAGE.main(), 1)
            self.assertFalse(output.exists())

    def test_main_writes_runtime_archive_and_checksums(self):
        with tempfile.TemporaryDirectory() as folder:
            output = Path(folder) / "release"
            policy = {"visibility": "public", "license": "MIT", "publication_allowed": True}
            args = argparse.Namespace(
                version="0.1.0-rc.1", ref="HEAD", output=output,
                targets=["linux/amd64"], source=False,
            )

            def extract(_data, destination):
                (destination / "release-policy.json").write_text(json.dumps(policy))

            def build(_source, staging, **_kwargs):
                self.assertEqual(_kwargs["policy"], policy)
                artifact = staging / "agent-emails-0.1.0-rc.1-linux-amd64.tar.gz"
                artifact.write_bytes(b"artifact")
                return artifact

            completed = subprocess.CompletedProcess(["go", "version"], 0, stdout="go version go1.test test/arch\n")
            with mock.patch.object(PACKAGE, "parse_args", return_value=args), \
                    mock.patch.object(PACKAGE, "resolve_commit", return_value="a" * 40), \
                    mock.patch.object(PACKAGE, "require_clean_head"), \
                    mock.patch.object(PACKAGE, "commit_epoch", return_value=1700000000), \
                    mock.patch.object(PACKAGE, "archive_bytes", return_value=b"archive"), \
                    mock.patch.object(PACKAGE, "extract_archive", side_effect=extract), \
                    mock.patch.object(PACKAGE, "run", return_value=completed), \
                    mock.patch.object(PACKAGE, "build_target", side_effect=build):
                self.assertEqual(PACKAGE.main(), 0)
            artifact = output / "agent-emails-0.1.0-rc.1-linux-amd64.tar.gz"
            expected = hashlib.sha256(artifact.read_bytes()).hexdigest()
            self.assertEqual((output / "SHA256SUMS").read_text(), f"{expected}  {artifact.name}\n")


if __name__ == "__main__":
    unittest.main()
