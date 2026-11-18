CREATE TABLE IF NOT EXISTS embedding_jobs (
  id uuid PRIMARY KEY,
  message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  segment text NOT NULL,
  chunk_index int NOT NULL DEFAULT 0,
  model_name text NOT NULL,
  source_text text NOT NULL,
  status text NOT NULL,
  provider_batch_id text NULL,
  attempts int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL,
  lease_until timestamptz NULL,
  last_error text NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  UNIQUE (message_id, segment, chunk_index, model_name)
);

CREATE INDEX IF NOT EXISTS idx_embedding_jobs_status_next_attempt
  ON embedding_jobs(status, next_attempt_at);
