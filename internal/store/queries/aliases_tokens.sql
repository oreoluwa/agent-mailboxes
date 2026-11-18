-- name: ListAliases :many
SELECT id, email, enabled
FROM aliases
WHERE mailbox_id = $1
ORDER BY email;

-- name: AddAlias :exec
INSERT INTO aliases (id, mailbox_id, email, enabled, created_at)
VALUES ($1, $2, $3, true, now())
ON CONFLICT (mailbox_id, email) DO NOTHING;

-- name: MintToken :exec
INSERT INTO tokens (id, alias_id, name, token_hash, scopes, created_at, revoked_at)
VALUES ($1, $2, $3, $4, $5, now(), NULL);

-- name: RevokeToken :execrows
UPDATE tokens
SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL;

-- name: AuthenticateToken :one
SELECT id, alias_id, scopes
FROM tokens
WHERE token_hash = $1 AND revoked_at IS NULL;
