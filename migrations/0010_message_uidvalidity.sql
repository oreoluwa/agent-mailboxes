ALTER TABLE messages
  ADD COLUMN IF NOT EXISTS uidvalidity bigint NOT NULL DEFAULT 0;

UPDATE messages AS m
SET uidvalidity = f.uidvalidity
FROM folders AS f
WHERE f.id = m.folder_id
  AND m.uidvalidity = 0;

ALTER TABLE messages
  DROP CONSTRAINT IF EXISTS messages_folder_id_imap_uid_key;

ALTER TABLE messages
  ADD CONSTRAINT messages_folder_id_uidvalidity_imap_uid_key
  UNIQUE (folder_id, uidvalidity, imap_uid);
