CREATE TABLE IF NOT EXISTS message_raw (
  message_id uuid PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  raw_rfc822 bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
