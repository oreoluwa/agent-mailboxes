CREATE TABLE IF NOT EXISTS summarize_jobs (
  id uuid PRIMARY KEY,
  message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  payload jsonb NOT NULL,
  status text NOT NULL,
  attempts int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL,
  lease_until timestamptz NULL,
  last_error text NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  UNIQUE (message_id)
);

CREATE INDEX IF NOT EXISTS idx_summarize_jobs_status_next_attempt
  ON summarize_jobs(status, next_attempt_at);
