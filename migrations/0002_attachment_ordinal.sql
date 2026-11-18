ALTER TABLE attachments ADD COLUMN IF NOT EXISTS ordinal int;

WITH ranked AS (
  SELECT
    id,
    ROW_NUMBER() OVER (PARTITION BY message_id ORDER BY created_at, id) - 1 AS rn
  FROM attachments
)
UPDATE attachments a
SET ordinal = ranked.rn
FROM ranked
WHERE a.id = ranked.id
  AND (a.ordinal IS NULL OR a.ordinal <> ranked.rn);

ALTER TABLE attachments ALTER COLUMN ordinal SET DEFAULT 0;
UPDATE attachments SET ordinal = 0 WHERE ordinal IS NULL;
ALTER TABLE attachments ALTER COLUMN ordinal SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_attachments_message_ordinal
  ON attachments(message_id, ordinal);
