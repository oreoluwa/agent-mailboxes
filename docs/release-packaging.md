# Release packaging

Use this helper to build source and native-binary archives from a Git commit. It requires a local Git checkout and the Go toolchain.

`release-policy.json` records the intended repository visibility, project license, and publication approval. Packaging works with either private or public visibility and either approval state. The policy is recorded in native `VERSION.json`; it is metadata, not a command to publish.

The helper writes local files only. It does not create GitHub releases, push images, upload artifacts, change visibility, or grant publication approval. Public distribution remains a separate explicit owner action under the [release checklist](release-checklist.md).

## Build

```sh
python3 scripts/package-release.py \
  --version 0.1.0-rc.1 \
  --ref HEAD \
  --output /tmp/agent-emails-0.1.0-rc.1 \
  --source
```

The output directory must not already exist. The helper uses `git archive` to export the selected commit into a temporary directory, then runs ordinary Go builds. Untracked and ignored worktree files are not included. Use a clean reviewed checkout and avoid editing the build inputs during packaging.

By default, it builds Linux and macOS binaries for amd64 and arm64. Repeat `--target` to select fewer targets, for example `--target linux/amd64`.

Each native archive contains:

- `bin/email` and `bin/emaild`;
- SQL migrations;
- `LICENSE`, `THIRD_PARTY_NOTICES.md`, and the `third_party/` license texts;
- `scripts/onboard-agent.py` and `scripts/mailbox-onboard.py` for hidden-password onboarding and profile export;
- a standalone `BUNDLE-README.md` and revision/target metadata.

`SHA256SUMS` covers the archives. This is build identification and transfer verification, not a complete supply-chain attestation. No third-party licensing or secret-clearance claim is produced by the helper.

## Source export

`--source` also writes a standard Git source archive named `agent-emails-VERSION-source.tar.gz`. It includes tracked code, deployment files, skills, and public documentation. Standard Git `export-ignore` attributes apply if configured.

Inspect the archive before publication. Adding a sensitive file to Git can put it into a source export. Scan the extracted candidate with an established secret scanner and complete the release checklist.

## Deployment

For Docker deployment, extract the source archive and follow [Linux/Docker server setup](supported-onboarding.md). For native binaries, extract the target archive and follow its standalone README from the archive root so migrations and helper paths resolve correctly. Installation tests must use disposable services and synthetic credentials, never a live installation.

## Optional GitHub workflow

`Release Package` is manually dispatched and works in private or public repositories. It uploads a short-lived Actions artifact; it does not create releases, push images, merge branches, or change visibility. It has only `contents: read` permission and no release/tag/image publication steps. In a public repository, treat workflow artifacts as potentially accessible to repository readers; dispatch only with source suitable for that audience. Changing visibility does not run this workflow automatically.

For an approved public release, update the policy in a reviewed commit to match the intended visibility and approval, build that exact commit, verify its artifacts, and publish through a separate explicitly authorized release action. Do not interpret a successful build as permission to publish.

The same packaging command can be run locally without GitHub Actions.
