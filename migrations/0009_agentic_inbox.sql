CREATE TABLE IF NOT EXISTS actor_request_replays (
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  request_id text NOT NULL,
  nonce text NOT NULL,
  action text NOT NULL,
  target_id text NOT NULL,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (mailbox_id, request_id, nonce)
);

CREATE INDEX IF NOT EXISTS idx_actor_request_replays_expires_at
  ON actor_request_replays(expires_at);

CREATE TABLE IF NOT EXISTS mailbox_agent_settings (
  mailbox_id uuid PRIMARY KEY REFERENCES mailboxes(id) ON DELETE CASCADE,
  system_prompt text NOT NULL DEFAULT '',
  enabled_tools text[] NOT NULL DEFAULT ARRAY[
    'search_messages',
    'read_message',
    'read_body',
    'get_thread_context',
    'list_recent_messages',
    'create_draft',
    'revise_draft'
  ],
  model_override text NULL,
  auto_draft_enabled bool NOT NULL DEFAULT false,
  max_tool_steps int NOT NULL DEFAULT 6,
  tool_timeout_sec int NOT NULL DEFAULT 45,
  policy_version int NOT NULL DEFAULT 1,
  updated_by text NULL,
  last_request_id text NULL,
  permission_intent text NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS agent_conversations (
  id uuid PRIMARY KEY,
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  message_id uuid NULL REFERENCES messages(id) ON DELETE SET NULL,
  thread_rfc_message_id text NULL,
  status text NOT NULL,
  created_by text NULL,
  last_draft_id uuid NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_conversations_mailbox_created_at
  ON agent_conversations(mailbox_id, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_conversations_mailbox_message
  ON agent_conversations(mailbox_id, message_id)
  WHERE message_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_conversations_mailbox_thread
  ON agent_conversations(mailbox_id, thread_rfc_message_id)
  WHERE thread_rfc_message_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS agent_messages (
  id uuid PRIMARY KEY,
  conversation_id uuid NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
  role text NOT NULL,
  content text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_messages_conversation_created_at
  ON agent_messages(conversation_id, created_at ASC);

CREATE TABLE IF NOT EXISTS agent_tool_calls (
  id uuid PRIMARY KEY,
  conversation_id uuid NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
  tool_name text NOT NULL,
  status text NOT NULL,
  args_summary jsonb NOT NULL DEFAULT '{}'::jsonb,
  result_summary jsonb NOT NULL DEFAULT '{}'::jsonb,
  policy_decision text NOT NULL DEFAULT '',
  error_text text NULL,
  started_at timestamptz NOT NULL,
  completed_at timestamptz NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_tool_calls_conversation_started_at
  ON agent_tool_calls(conversation_id, started_at ASC);

CREATE TABLE IF NOT EXISTS drafts (
  id uuid PRIMARY KEY,
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  conversation_id uuid NULL REFERENCES agent_conversations(id) ON DELETE SET NULL,
  source_message_id uuid NULL REFERENCES messages(id) ON DELETE SET NULL,
  thread_rfc_message_id text NULL,
  status text NOT NULL,
  latest_revision_id uuid NULL,
  actor_user_id text NULL,
  last_request_id text NULL,
  permission_intent text NULL,
  send_idempotency_key text NULL,
  sent_revision_id uuid NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_drafts_mailbox_updated_at
  ON drafts(mailbox_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS draft_revisions (
  id uuid PRIMARY KEY,
  draft_id uuid NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
  revision_number int NOT NULL,
  provenance text NOT NULL,
  subject text NOT NULL,
  to_emails jsonb NOT NULL,
  body text NOT NULL,
  in_reply_to text NULL,
  "references" text NULL,
  actor_user_id text NULL,
  request_id text NULL,
  permission_intent text NULL,
  created_at timestamptz NOT NULL,
  UNIQUE (draft_id, revision_number)
);

CREATE INDEX IF NOT EXISTS idx_draft_revisions_draft_created_at
  ON draft_revisions(draft_id, created_at ASC);

ALTER TABLE drafts
  ADD CONSTRAINT drafts_latest_revision_fk
  FOREIGN KEY (latest_revision_id) REFERENCES draft_revisions(id) ON DELETE SET NULL;

ALTER TABLE drafts
  ADD CONSTRAINT drafts_sent_revision_fk
  FOREIGN KEY (sent_revision_id) REFERENCES draft_revisions(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS auto_draft_jobs (
  id uuid PRIMARY KEY,
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  policy_version int NOT NULL,
  status text NOT NULL,
  attempts int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL,
  lease_until timestamptz NULL,
  last_error text NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  UNIQUE (mailbox_id, message_id, policy_version)
);

CREATE INDEX IF NOT EXISTS idx_auto_draft_jobs_status_next_attempt
  ON auto_draft_jobs(mailbox_id, status, next_attempt_at);

CREATE TABLE IF NOT EXISTS auto_draft_runs (
  id uuid PRIMARY KEY,
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  job_id uuid NOT NULL REFERENCES auto_draft_jobs(id) ON DELETE CASCADE,
  policy_version int NOT NULL,
  status text NOT NULL,
  failure_reason text NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_auto_draft_runs_mailbox_created_at
  ON auto_draft_runs(mailbox_id, created_at DESC);
