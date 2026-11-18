ALTER TABLE agent_conversations
  ADD COLUMN IF NOT EXISTS alias_id uuid NULL REFERENCES aliases(id) ON DELETE CASCADE;

ALTER TABLE drafts
  ADD COLUMN IF NOT EXISTS alias_id uuid NULL REFERENCES aliases(id) ON DELETE CASCADE;

DROP INDEX IF EXISTS idx_agent_conversations_mailbox_message;
DROP INDEX IF EXISTS idx_agent_conversations_mailbox_thread;

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_conversations_alias_message
  ON agent_conversations(mailbox_id, alias_id, message_id)
  WHERE alias_id IS NOT NULL AND message_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_conversations_alias_thread
  ON agent_conversations(mailbox_id, alias_id, thread_rfc_message_id)
  WHERE alias_id IS NOT NULL AND thread_rfc_message_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_conversations_admin_message
  ON agent_conversations(mailbox_id, message_id)
  WHERE alias_id IS NULL AND message_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_conversations_admin_thread
  ON agent_conversations(mailbox_id, thread_rfc_message_id)
  WHERE alias_id IS NULL AND thread_rfc_message_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_agent_conversations_alias_created_at
  ON agent_conversations(mailbox_id, alias_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_drafts_alias_updated_at
  ON drafts(mailbox_id, alias_id, updated_at DESC);

-- Releases before this migration marked a draft sent when it was only queued. A dead
-- outbound row proves delivery did not complete, so make those drafts explicitly
-- recoverable. This does not enqueue anything; a user must review/revise and send again.
UPDATE drafts d
SET status = 'failed', updated_at = now()
FROM outbound_messages o
WHERE d.status = 'sent'
  AND d.send_idempotency_key = o.idempotency_key
  AND d.mailbox_id = o.mailbox_id
  AND o.status = 'dead';

UPDATE drafts d
SET status = 'queued', updated_at = now()
FROM outbound_messages o
WHERE d.status = 'sent'
  AND d.send_idempotency_key = o.idempotency_key
  AND d.mailbox_id = o.mailbox_id
  AND o.status IN ('pending', 'processing');
