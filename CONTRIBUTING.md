# Contributing

Project code is MIT licensed. Contributions must be compatible with that license and retain third-party attribution.

## Development principles

- Keep provider credentials, agent profiles, tokens, mailbox databases, logs, and real mail out of Git, issues, pull requests, and test output.
- Use synthetic addresses under reserved domains such as `example.test`; never commit captured production headers or message bodies.
- Keep alias authorization fail closed. A sender, subject, body, or unrelated header must never grant message visibility.
- Treat message content and provider headers as untrusted input.
- Keep model-backed behavior optional. Core receive/read/search/send/reply paths must work with AI disabled.
- Add dependencies only when their maintenance, license, and security implications are understood.

## Local checks

```sh
go test ./...
go vet ./...
go build ./...
python3 scripts/alias-isolation-smoke.py
./scripts/uidvalidity-integration.sh
./scripts/agent-store-integration.sh
python3 scripts/supported-onboarding-smoke.py
python3 scripts/test-setup-local.py
python3 scripts/test-onboard-agent.py
```


Some tests use Docker and loopback listeners. They must create unique disposable resources and clean up only what they created. Do not point test scripts at a real mailbox, production database, or operator configuration directory.

## Pull requests

- Keep changes focused and explain user-visible behavior and migration impact.
- Include regression tests for authorization, persistence, and failure paths affected by the change.
- Document new environment variables, API routes, schemas, and upgrade requirements.
- List the checks you ran and any checks you skipped.
- Preserve unrelated working-tree changes and private operator artifacts.

Generated protobuf/Connect output should be regenerated with the Go protobuf generation script (scripts/proto-generate.sh) and reviewed with its source definitions. Dependency changes must update lockfiles and document relevant license obligations for release review. Do not add copied code, assets, or fixtures without recording provenance and compatible licensing.
