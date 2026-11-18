# Security policy

## Supported versions

Security fixes are maintained on the default branch. Older revisions may not receive backported fixes.

## Reporting a vulnerability

Report suspected vulnerabilities privately to the repository maintainers. If GitHub private vulnerability reporting is enabled for the repository, use it. Do not open a public issue containing exploit details, credentials, message content, tokens, private URLs, or account identifiers.

Include the affected revision, security boundary, a minimal synthetic reproduction, impact, and any suggested mitigation. Use reserved test domains and disposable data. Do not test against someone else's mailbox or service without explicit authorization.

If a secret may have been exposed, revoke or rotate it through the owning provider or service first. Removing a value from the current tree does not remove it from Git history, caches, logs, images, or previously downloaded release artifacts.

## Security boundaries

- Provider credentials belong only to the service and are encrypted at rest under an operator-controlled key.
- Agents receive alias-scoped service credentials; they must not receive provider passwords, master keys, or other agents' profiles.
- Profiles and injected environment variables do not isolate agents with unrestricted access under the same OS user. Per-agent isolation depends on the runner preventing cross-agent access to secrets, files, and processes. See [shared-host guidance](docs/supported-onboarding.md#multiple-agents-on-one-computer).
- Mail content and headers are untrusted. Alias routing depends only on supported parsed recipient/delivery fields.
- The default API binds to loopback. The profile client requires HTTPS for non-loopback URLs; an SSH tunnel can preserve loopback HTTP access. Agent access uses alias-scoped credentials; administrative operations require operator credentials.
- Development login modes, synthetic mail servers, and test fixtures are not production features.
