-- name: GetFolderCheckpoint :one
SELECT id, uidvalidity, last_seen_uid
FROM folders
WHERE mailbox_id = $1 AND name = $2;

-- name: UpsertFolderCheckpoint :one
INSERT INTO folders (id, mailbox_id, name, uidvalidity, last_seen_uid, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (mailbox_id, name) DO UPDATE
SET uidvalidity = EXCLUDED.uidvalidity,
    last_seen_uid = GREATEST(folders.last_seen_uid, EXCLUDED.last_seen_uid),
    updated_at = now()
RETURNING id;
