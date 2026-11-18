---
name: emaild
description: Use for all emaild daemon and admin CLI tasks, including migrate, serve, dependency checks, install/uninstall, alias and token management, status, and sync.
---

# Emaild Skill

Operate the `emaild` CLI for daemon lifecycle and administration.

## Environment
- `EMAILD_ENV_FILE` optional env file path loaded at startup.
- `EMAIL_API` optional admin API URL. Default: `http://127.0.0.1:8787`.
- `EMAIL_TOKEN` bearer token for admin API commands.
- `EMAIL_MAILBOX` optional mailbox selector for admin API commands.
- `FETCH_MAX_MESSAGES_PER_SYNC` optional cap on IMAP messages fetched per mailbox sync cycle. Default: `200` (`0` = unlimited).

## Startup Checks
1. Verify CLI is available: `emaild --help`.
2. If using admin API commands, verify token is set: `echo "${EMAIL_TOKEN:+set}"`.

## Commands

### Lifecycle
- `emaild migrate`: apply SQL migrations and ensure schema is up to date.
- `emaild migrate`
- `emaild serve`: start HTTP API; pollers/workers run unless `--http-only` is set.
- `emaild serve [--mailbox <name> | --all-mailboxes] [--http-only]`
- `emaild worker`: start mailbox pollers/workers without HTTP server.
- `emaild worker [--mailbox <name> | --all-mailboxes]`
- `emaild doctor`: check local attachment-analysis dependencies and report readiness.
- `emaild doctor`
- `emaild deps install`: install missing local dependencies used by attachment analysis.
- `emaild deps install`

### macOS LaunchAgent
- `emaild install`: install per-user LaunchAgent to auto-run `emaild serve` at login.
- `emaild install [--env-file <path>] [--workdir <path>]`
- `emaild uninstall`: remove installed per-user LaunchAgent.
- `emaild uninstall`

### Alias Management
- `emaild alias add`: register an alias address for routing and auth scoping.
- `emaild alias add <alias_email>`
- `emaild alias list`: view configured aliases.
- `emaild alias list`

### Token Management
- `emaild token mint`: create alias-scoped API token with selected scopes.
- `emaild token mint <alias_email> [--name <agent_name>] [--scopes read,send]`
- `emaild token revoke`: invalidate a previously minted token.
- `emaild token revoke <token_id>`

### Status and Sync
- `emaild status`: inspect server mailbox/runtime status via admin API.
- `emaild status`
- `emaild sync now`: trigger immediate mailbox sync outside normal poll interval.
- `emaild sync now`

## Admin Authentication
- Preferred: set `EMAIL_TOKEN`.
- Interactive mode: pass `--mint` with admin commands when stdin is interactive.
- Example: `emaild --mint status`

## Execution Rules
- Run `emaild migrate` before first `serve` in a new environment.
- `serve` mailbox mode:
  - one enabled mailbox: no flags required.
  - multiple enabled mailboxes: pass `--mailbox <name>` or `--all-mailboxes`.
  - API-only mode: pass `--http-only` to disable pollers/workers.
- `worker` mailbox mode:
  - one enabled mailbox: no flags required.
  - multiple enabled mailboxes: pass `--mailbox <name>` or `--all-mailboxes`.
  - with `--all-mailboxes`, worker reconciles mailbox create/enable/disable changes from DB on a timer.
  - `WORKER_RECONCILE_SEC` controls reconcile interval seconds (default `30`).
  - shard mode: pass `--shard-index N --shard-count K` (with `--all-mailboxes`) for deterministic mailbox partitioning.
- Search behavior served by API:
  - keyword search is always available.
  - setting `EMBEDDING_MODEL_NAME` enables semantic/hybrid search on `/v1/search`.
- Use `doctor` to check attachment-tool dependencies before long-running workloads.
- Use `deps install` only when missing dependencies are reported.
- Treat minted tokens as secrets; token value is printed once.

## Failure Handling
- Missing token for admin commands: set `EMAIL_TOKEN` or run with `--mint` interactively.
- **401 unauthorized** (e.g. after `alias add` or `--mint`): use a minted admin token, not the raw master key. If your `.env` already has `EMAIL_TOKEN`, run the command without overriding it (e.g. `./bin/emaild alias add <email>`). To mint a new token when CLI and server use different config, call the API with the master key: `curl -sS -X POST "$EMAIL_API/v1/admin/mint-token" -H "X-Master-Key: $MASTER_KEY" -H "Content-Type: application/json" -d '{}'` then set `EMAIL_TOKEN` to the returned `token` value.
- `serve` selection errors: pass explicit mailbox flags.
- Dependency failures: run `emaild doctor`, then `emaild deps install`.
- macOS install errors: ensure macOS and ensure `--workdir` contains `migrations/`.

## Common Flows
1. Bring up service:
   - `emaild migrate`
   - `emaild serve --all-mailboxes`
2. Configure alias and token:
   - `emaild alias add x.ai@example.com`
   - `emaild token mint x.ai@example.com --name x-agent --scopes read,send`
3. Recover sync lag:
   - `emaild status`
   - `emaild sync now`
