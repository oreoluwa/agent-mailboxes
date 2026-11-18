CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE IF NOT EXISTS mailboxes (
  id uuid PRIMARY KEY,
  name text NOT NULL UNIQUE,
  enabled bool NOT NULL DEFAULT true,
  imap_host text NOT NULL,
  imap_port int NOT NULL,
  imap_tls bool NOT NULL,
  smtp_host text NOT NULL,
  smtp_port int NOT NULL,
  smtp_tls bool NOT NULL,
  username text NOT NULL,
  password_encrypted text NOT NULL,
  created_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS aliases (
  id uuid PRIMARY KEY,
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  email citext NOT NULL,
  enabled bool NOT NULL,
  created_at timestamptz NOT NULL,
  UNIQUE (mailbox_id, email)
);

CREATE TABLE IF NOT EXISTS tokens (
  id uuid PRIMARY KEY,
  alias_id uuid NULL REFERENCES aliases(id) ON DELETE CASCADE,
  name text NOT NULL,
  token_hash text NOT NULL UNIQUE,
  scopes text[] NOT NULL,
  created_at timestamptz NOT NULL,
  revoked_at timestamptz NULL
);

CREATE TABLE IF NOT EXISTS folders (
  id uuid PRIMARY KEY,
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  name text NOT NULL,
  uidvalidity bigint NOT NULL,
  last_seen_uid bigint NOT NULL,
  updated_at timestamptz NOT NULL,
  UNIQUE (mailbox_id, name)
);

CREATE TABLE IF NOT EXISTS messages (
  id uuid PRIMARY KEY,
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  folder_id uuid NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  imap_uid bigint NOT NULL,
  message_id text,
  internal_date timestamptz NOT NULL,
  sent_date timestamptz NULL,
  from_email citext,
  from_name text NULL,
  subject text NULL,
  snippet text NULL,
  raw_headers text NOT NULL,
  has_attachments bool NOT NULL,
  spam_flag bool NOT NULL,
  body_state text NOT NULL,
  in_reply_to text NULL,
  "references" text NULL,
  created_at timestamptz NOT NULL,
  UNIQUE (folder_id, imap_uid)
);

CREATE TABLE IF NOT EXISTS message_recipients (
  message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  type text NOT NULL,
  email citext NOT NULL
);

CREATE TABLE IF NOT EXISTS message_audience (
  message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  alias_id uuid NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
  PRIMARY KEY (message_id, alias_id)
);

CREATE TABLE IF NOT EXISTS message_bodies (
  message_id uuid PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  text_plain text NULL,
  text_html text NULL,
  extracted_text text NULL,
  fetched_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS message_digest (
  message_id uuid PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  model_name text NOT NULL,
  summary_short text NOT NULL,
  summary_bullets text[] NOT NULL,
  intent text NOT NULL,
  priority text NOT NULL,
  required_action text NOT NULL,
  suggested_next_step text NOT NULL,
  extracted_entities jsonb NOT NULL,
  confidence double precision NOT NULL,
  created_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS attachments (
  id uuid PRIMARY KEY,
  message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  filename text,
  mime text,
  size_bytes bigint NOT NULL,
  storage_path text NULL,
  created_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS message_ack (
  message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  alias_id uuid NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (message_id, alias_id)
);

CREATE TABLE IF NOT EXISTS audit_log (
  id uuid PRIMARY KEY,
  token_id uuid NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
  action text NOT NULL,
  message_id uuid NULL REFERENCES messages(id) ON DELETE SET NULL,
  target text NULL,
  created_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_mailbox_date ON messages(mailbox_id, internal_date DESC);
CREATE INDEX IF NOT EXISTS idx_message_audience_alias ON message_audience(alias_id, message_id);
