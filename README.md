# Agent Emails

Self-hosted, alias-scoped mailbox access for agents using an existing IMAP/SMTP provider.

Project code is [MIT licensed](LICENSE); third-party terms are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Start here

Follow [Linux/Docker server setup](docs/supported-onboarding.md) to create a persistent service, connect a provider mailbox, and export a private agent profile. The provider continues to handle delivery, spam filtering, and sender reputation. This project does not provision provider addresses or run an email server.

- `emaild`: mailbox ingest, alias routing, encrypted provider credentials, HTTP/Connect API, and operator CLI.
- `email`: portable agent CLI for inbox, search, send, and reply using alias-scoped credentials.
- `scripts/setup-local.py`: persistent local deployment with generated Compose configuration and private operator files.
- `skills/email` and `skills/emaild`: agent/operator usage guidance.
- `proto/` and [OpenAPI](docs/openapi/emaild.openapi.yaml): API definitions and generated Go bindings.

## Scope and limitations

This repository contains the server, agent CLI, onboarding/deployment tools, API contracts, tests, and optional AI features. It does not include a web inbox, IMAP/SMTP bridge, or OAuth/MCP authorization.

Core receive/read/search/send/reply works with AI disabled. Summarization, embeddings, attachment analysis, and drafting code are optional features. PostgreSQL with pgvector remains required by the schema.

Aliases must already deliver at the provider. Verify outbound send-as permissions separately. Validated provider coverage is limited to a Zoho custom-domain mailbox using implicit TLS and an app password; this is not a general compatibility claim for Gmail, Outlook, Bcc, forwarding, or OAuth-only accounts.

Alias routing uses parsed recipient/delivery headers, not arbitrary message content. The daemon fetches mail before assigning alias audiences, so unrelated provider mail may transit the service but is not exposed to an alias agent. Use a dedicated upstream mailbox if provider-side separation is required.

The setup script binds the API to loopback. Agents can run on Linux or macOS, on the server or another machine. For another machine, use an SSH tunnel or configure an HTTPS endpoint; see the [access options](docs/supported-onboarding.md#4-give-the-agent-portable-access). Choose network exposure and backups for your environment. Native archives can be built with the [packaging guide](docs/release-packaging.md). Build container images locally using the supplied Dockerfile.

For several agents sharing a computer, see [per-agent secret injection and isolation limits](docs/supported-onboarding.md#multiple-agents-on-one-computer). Alias-scoped tokens protect server access; keeping tokens separate depends on the agent runner's isolation.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), and [CHANGELOG.md](CHANGELOG.md). Tests use synthetic credentials and disposable services. Never point test scripts at a live mailbox or production database.

```sh
go test ./...
go vet ./...
go build ./...
python3 scripts/test-setup-local.py
python3 scripts/test-onboard-agent.py
python3 scripts/alias-isolation-smoke.py
python3 scripts/supported-onboarding-smoke.py
```
