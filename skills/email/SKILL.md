---
name: email
description: Use for all agent mailbox CLI tasks with the email command, including inbox/junk/sent listing, reading, body extraction, ack, send, reply, and search.
---

# Email Skill

Operate the `email` CLI for mailbox workflows.

## Configuration

Use the credential method configured by the operator: a private agent profile or a runner-injected token. For profile-based access, use the profile exported by supported onboarding. It contains the API URL, mailbox, alias, and scoped credential; no separate `EMAIL_TOKEN` is needed.

Set the profile path once to use the commands below unchanged:

```sh
export EMAIL_PROFILE=/path/to/agent-profile.json
```

Or pass it explicitly before the command:

```sh
email --profile /path/to/agent-profile.json inbox list --json
```

`--profile` takes precedence over `EMAIL_PROFILE`. A selected profile takes precedence over legacy `EMAIL_API` and `EMAIL_TOKEN` settings. Keep the profile private; do not print its contents or paste credentials into chat.

For runner-managed secret injection, leave `EMAIL_PROFILE` unset and omit `--profile`; use the assigned `EMAIL_TOKEN` and `EMAIL_API` (default `http://127.0.0.1:8787`). Do not print these credentials, request their values in chat, or read another agent's secret/profile. Injection alone does not isolate same-user agents; the runner must enforce that boundary. See [shared-host setup](../../docs/supported-onboarding.md#multiple-agents-on-one-computer).

## Startup Checks

1. Verify CLI availability with `email --help`.
2. Use the supplied `--profile` path or `EMAIL_PROFILE`. Check that the file is readable without displaying it, for example `test -r "$EMAIL_PROFILE"` when using the environment variable.
3. Only when neither profile option is configured, check whether the legacy token is set with `test -n "$EMAIL_TOKEN"`. If configuration is missing, ask the operator to supply a profile path or configure per-agent token injection; never request the token value in chat.

## Commands

### Inbox
- `email inbox list`: list inbox messages for triage; use `--unread` for pending items.
- `email inbox list [--unread] [--limit N] [--json]`
- `email inbox read`: inspect message metadata, summary, and attachment metadata.
- `email inbox read <msg_id> [--json]`
- `email inbox body`: print extracted plaintext/markdown body content.
- `email inbox body <msg_id> [--json]`
- `email inbox ack`: mark message as acknowledged after handling.
- `email inbox ack <msg_id>`

### Junk
- `email junk list`: review spam/junk queue.
- `email junk list [--unread] [--limit N] [--json]`
- `email junk read`: inspect junk message metadata and summary.
- `email junk read <msg_id> [--json]`
- `email junk body`: print extracted junk message body content.
- `email junk body <msg_id> [--json]`
- `email junk ack`: mark junk item as acknowledged.
- `email junk ack <msg_id>`

### Sent
- `email sent list`: list sent messages for confirmation/audit.
- `email sent list [--limit N] [--json]`
- `email sent read`: inspect sent message metadata and summary.
- `email sent read <msg_id> [--json]`
- `email sent body`: print extracted sent body content.
- `email sent body <msg_id> [--json]`

### Send
- `email send`: create a new outbound message.
- `email send --to <email[,email]> [--subject <text>] --body-file <path> [--json]`

### Reply
- `email reply`: send reply using thread headers from the original message.
- `email reply <msg_id> --body-file <path> [--json]`

### Search
- `email search`: unified search command.
  - Use it to find relevant messages by terms, phrases, or intent.
  - `--folder` is optional; when omitted, the command can infer a folder from the query text.
- `email search <query> [--folder all|inbox|junk|spam|sent] [--limit N] [--json]`

## Execution Rules
- Prefer `--json` when output is consumed by tools or automation.
- Validate required flags before running:
  - `send` needs `--to` and `--body-file`.
  - `reply` needs `<msg_id>` and `--body-file`.
- Keep filters explicit with `--unread`, `--folder`, and `--limit`.
- Treat `msg_id` as opaque values copied from list/search output.

## Failure Handling
- Missing configuration: ask the operator to configure a profile or runner-injected token. Do not switch credential sources without the operator's direction.
- Profile loading or validation errors: report the path/configuration problem without displaying file contents. Have the operator correct permissions or re-export the profile; do not silently fall back to a different credential.
- Access denied responses: token is invalid or does not allow this action.
- Connection errors: retry once; if it still fails, stop and return the error.

## Common Flows
1. Triage unread inbox:
   - `email inbox list --unread --limit 20`
   - `email inbox read <msg_id>`
   - `email inbox ack <msg_id>`
2. Search and inspect:
   - `email search "invoice overdue" --folder junk --limit 50`
   - `email junk body <msg_id>`
3. Respond:
   - `email reply <msg_id> --body-file /tmp/reply.txt`
