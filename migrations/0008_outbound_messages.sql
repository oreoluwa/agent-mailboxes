CREATE TABLE IF NOT EXISTS outbound_messages (
  id uuid PRIMARY KEY,
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  alias_id uuid NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
  from_email citext NOT NULL,
  to_emails jsonb NOT NULL,
  raw_rfc822 bytea NOT NULL,
  idempotency_key text NOT NULL,
  status text NOT NULL,
  attempts int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL,
  lease_until timestamptz NULL,
  last_error text NULL,
  accepted_at timestamptz NOT NULL DEFAULT now(),
  sent_at timestamptz NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (mailbox_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_outbound_messages_status_next_attempt
  ON outbound_messages(mailbox_id, status, next_attempt_at);
