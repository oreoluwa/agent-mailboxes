#!/usr/bin/env python3
"""Build local release bundles from a committed Git revision; never publish them."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parents[1]
TARGETS = ("darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64")
VERSION_RE = re.compile(
    r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-"
    r"[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*$"
)


class PackageError(Exception):
    pass


def run(args: list[str], *, cwd: Path = ROOT, env: dict[str, str] | None = None,
        capture: bool = False) -> subprocess.CompletedProcess:
    return subprocess.run(
        args, cwd=cwd, env=env, check=True,
        stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.PIPE if capture else None,
        text=capture,
    )


def git_output(*args: str) -> str:
    return run(["git", *args], cwd=ROOT, capture=True).stdout.strip()


def validate_version(value: str) -> str:
    if not VERSION_RE.fullmatch(value) or any(
        part.isdigit() and len(part) > 1 and part.startswith("0")
        for part in value.split("-", 1)[-1].split(".")
    ):
        raise argparse.ArgumentTypeError(
            "version must be a prerelease version, for example 0.1.0-rc.1"
        )
    return value


def resolve_commit(ref: str) -> str:
    try:
        return git_output("rev-parse", "--verify", ref + "^{commit}")
    except subprocess.CalledProcessError as error:
        raise PackageError(f"Git ref does not resolve to a commit: {ref}") from error


def require_clean_head(ref: str, commit: str) -> None:
    if ref not in {"HEAD", "@", git_output("branch", "--show-current")}:
        return
    if commit != git_output("rev-parse", "HEAD"):
        return
    for diff_args in (("diff", "--quiet"), ("diff", "--cached", "--quiet")):
        if subprocess.run(["git", *diff_args], cwd=ROOT, check=False).returncode:
            raise PackageError("tracked worktree changes must be committed before packaging HEAD")


def commit_epoch(commit: str) -> int:
    return int(git_output("show", "-s", "--format=%ct", commit))


def archive_bytes(commit: str) -> bytes:
    return subprocess.run(
        ["git", "archive", "--format=tar", commit], cwd=ROOT,
        stdout=subprocess.PIPE, check=True,
    ).stdout


def extract_archive(data: bytes, destination: Path) -> None:
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as archive:
        for member in archive.getmembers():
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts:
                raise PackageError(f"unsafe path in Git archive: {member.name}")
            if member.isdir():
                destination.joinpath(*path.parts).mkdir(parents=True, exist_ok=True)
                continue
            if not member.isfile():
                raise PackageError(f"unsupported entry in Git archive: {member.name}")
            source = archive.extractfile(member)
            if source is None:
                raise PackageError(f"cannot read Git archive entry: {member.name}")
            target = destination.joinpath(*path.parts)
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(source.read())
            target.chmod(member.mode & 0o777)


def load_release_policy(source: Path) -> dict:
    try:
        policy = json.loads((source / "release-policy.json").read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise PackageError("selected revision has no valid release-policy.json") from error
    if (
        not isinstance(policy, dict)
        or set(policy) != {"visibility", "license", "publication_allowed"}
        or policy["visibility"] not in ("private", "public")
        or policy["license"] != "MIT"
        or type(policy["publication_allowed"]) is not bool
    ):
        raise PackageError("release policy requires private/public visibility, MIT license, and boolean publication_allowed")
    return policy


def runtime_files(source: Path, metadata: dict, binaries: dict[str, bytes]) -> dict[str, tuple[bytes, int]]:
    files: dict[str, tuple[bytes, int]] = {
        "BUNDLE-README.md": ((
            "# Agent Emails release bundle\n\n"
            "Project code is MIT licensed. Building this bundle does not publish it. "
            "See VERSION.json for the recorded release policy, and LICENSE and THIRD_PARTY_NOTICES.md for licensing.\n\n"
            "Requires PostgreSQL with pgvector and Python 3 for the onboarding helpers. "
            "Run commands from this extracted bundle root.\n\n"
            "Set POSTGRES_DSN, MASTER_KEY and MAILBOX_CREDENTIAL_KEY in a private environment "
            "file. Set SUMMARY_ENABLED=false to run without AI and INITIAL_SYNC_MAX_MESSAGES=0 "
            "to start with new mail only. Then run `EMAILD_ENV_FILE=/private/emaild.env bin/emaild migrate` followed by "
            "`EMAILD_ENV_FILE=/private/emaild.env bin/emaild serve --all-mailboxes`. Use "
            "`python3 scripts/onboard-agent.py --help` for the hidden-password onboarding options. "
            "After creating a profile, run `bin/email --profile /path/to/agent.json inbox list --json`. "
            "Keep provider passwords and operator keys out of agent profiles.\n"
        ).encode(), 0o644),
        "VERSION.json": ((json.dumps(metadata, sort_keys=True, indent=2) + "\n").encode(), 0o644),
        "bin/email": (binaries["email"], 0o755),
        "bin/emaild": (binaries["emaild"], 0o755),
    }
    migrations = sorted((source / "migrations").glob("*.sql"))
    if not migrations:
        raise PackageError("native bundle requires SQL migrations")
    for migration in migrations:
        files[f"migrations/{migration.name}"] = (migration.read_bytes(), 0o644)
    for helper in ("mailbox-onboard.py", "onboard-agent.py"):
        path = source / "scripts" / helper
        if not path.is_file():
            raise PackageError(f"native bundle requires scripts/{helper}")
        files[f"scripts/{helper}"] = (path.read_bytes(), 0o755)
    for name in ("LICENSE", "THIRD_PARTY_NOTICES.md"):
        files[name] = ((source / name).read_bytes(), 0o644)
    for path in sorted((source / "third_party").rglob("*")):
        if path.is_file():
            files[path.relative_to(source).as_posix()] = (path.read_bytes(), 0o644)
    return files


def normalized_tar_gz(output: Path, root_name: str,
                      files: dict[str, tuple[bytes, int]], epoch: int) -> None:
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.PAX_FORMAT) as archive:
        for name in sorted(files):
            data, mode = files[name]
            info = tarfile.TarInfo(f"{root_name}/{name}")
            info.size = len(data)
            info.mode = mode
            info.mtime = epoch
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            archive.addfile(info, io.BytesIO(data))
    with output.open("wb") as destination:
        with gzip.GzipFile(filename="", mode="wb", fileobj=destination, mtime=epoch) as compressed:
            compressed.write(raw.getvalue())


def build_target(source: Path, output: Path, *, version: str, commit: str,
                 epoch: int, target: str, policy: dict, go_version: str) -> Path:
    goos, goarch = target.split("/", 1)
    environment = os.environ.copy()
    environment.update({"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch, "GOFLAGS": "", "GOWORK": "off"})
    binaries: dict[str, bytes] = {}
    with tempfile.TemporaryDirectory(prefix="agent-emails-binaries-") as folder:
        build = Path(folder)
        for command in ("email", "emaild"):
            binary = build / command
            run([
                "go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
                "-ldflags=-s -w -buildid=", "-o", str(binary), f"./cmd/{command}",
            ], cwd=source, env=environment)
            binaries[command] = binary.read_bytes()
    metadata = {
        "name": "agent-emails", "version": version, "revision": commit,
        "target": target, "go_version": go_version, **policy,
    }
    root_name = f"agent-emails-{version}-{goos}-{goarch}"
    archive = output / f"{root_name}.tar.gz"
    normalized_tar_gz(archive, root_name, runtime_files(source, metadata, binaries), epoch)
    return archive


def write_source_archive(commit: str, output: Path, version: str, epoch: int) -> Path:
    root_name = f"agent-emails-{version}-source"
    tar_data = subprocess.run(
        ["git", "archive", "--format=tar", f"--prefix={root_name}/", commit],
        cwd=ROOT, stdout=subprocess.PIPE, check=True,
    ).stdout
    archive = output / f"{root_name}.tar.gz"
    with archive.open("wb") as destination:
        with gzip.GzipFile(filename="", mode="wb", fileobj=destination, mtime=epoch) as compressed:
            compressed.write(tar_data)
    return archive


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, type=validate_version)
    parser.add_argument("--ref", required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--target", action="append", choices=TARGETS, dest="targets")
    parser.add_argument("--source", action="store_true")
    return parser.parse_args()


def main() -> int:
    try:
        args = parse_args()
        commit = resolve_commit(args.ref)
        require_clean_head(args.ref, commit)
        output = args.output.expanduser().absolute()
        if output.exists() or output.is_symlink():
            raise PackageError(f"output directory already exists: {output}")
        output.parent.mkdir(parents=True, exist_ok=True)
        epoch = commit_epoch(commit)
        with tempfile.TemporaryDirectory(prefix=f".{output.name}.staging-", dir=output.parent) as staging_name:
            staging = Path(staging_name)
            source = staging / "source"
            source.mkdir()
            extract_archive(archive_bytes(commit), source)
            policy = load_release_policy(source)
            go_version = run(["go", "version"], cwd=source, capture=True).stdout.split()[2]
            artifacts = [
                build_target(source, staging, version=args.version, commit=commit, epoch=epoch,
                             target=target, policy=policy, go_version=go_version)
                for target in (args.targets or TARGETS)
            ]
            if args.source:
                artifacts.append(write_source_archive(commit, staging, args.version, epoch))
            checksums = "".join(
                f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n"
                for path in sorted(artifacts, key=lambda item: item.name)
            )
            (staging / "SHA256SUMS").write_text(checksums, encoding="utf-8")
            shutil.rmtree(source)
            shutil.move(str(staging), str(output))
        print(f"Packaged release candidate {args.version} from {commit}")
        print(f"Artifacts: {output}")
        return 0
    except (PackageError, OSError, subprocess.CalledProcessError, tarfile.TarError) as error:
        print(f"package-release: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
