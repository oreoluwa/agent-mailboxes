# Changelog

All notable changes will be recorded here once versioned releases begin.

## Unreleased

### Added

- Alias-scoped read/send credentials over existing provider mailboxes.
- AI-optional receive, list, read, keyword search, send, and reply paths.
- UIDVALIDITY-aware synchronization with bounded initial history and new-mail-only mode.
- Encrypted provider credential storage with explicit migration and key rotation.
- Atomic mailbox/agent onboarding, additional-agent provisioning, and scoped credential replacement.
- Portable native CLI agent profiles.
- Persistent local Docker setup with backup/restore and restart verification.
- Optional summarization, embeddings, attachment analysis, and draft-agent features.

### Validated

- Synthetic PostgreSQL/GreenMail isolation, migration, restart, onboarding, and persistence flows.
- Zoho custom-domain IMAP/SMTP app-password flow with configured alias send and reply delivery.

### Known limitations

- No web inbox or IMAP/SMTP bridge is included.
- Gmail, Outlook, OAuth-only providers, Bcc-only delivery, and forwarding have not been validated end to end.
- The schema requires PostgreSQL with `pgvector` even when AI features are disabled.
- OAuth/MCP adapters, public hosting templates, date/byte history budgets, and resumable backfill remain future work.
