-- name: CreateMailbox :exec
INSERT INTO mailboxes (
  id,
  name,
  enabled,
  imap_host,
  imap_port,
  imap_tls,
  smtp_host,
  smtp_port,
  smtp_tls,
  username,
  password_encrypted,
  created_at
) VALUES ($1, $2, true, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: ListMailboxes :many
SELECT
  id,
  name,
  enabled,
  imap_host,
  imap_port,
  imap_tls,
  smtp_host,
  smtp_port,
  smtp_tls,
  username,
  password_encrypted,
  created_at
FROM mailboxes
ORDER BY created_at, name;

-- name: GetMailboxByName :one
SELECT
  id,
  name,
  enabled,
  imap_host,
  imap_port,
  imap_tls,
  smtp_host,
  smtp_port,
  smtp_tls,
  username,
  password_encrypted,
  created_at
FROM mailboxes
WHERE name = $1;

-- name: SetMailboxEnabled :execrows
UPDATE mailboxes
SET enabled = $2
WHERE name = $1;
