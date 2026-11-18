CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS message_embeddings (
  message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  segment text NOT NULL,
  chunk_index int NOT NULL DEFAULT 0,
  model_name text NOT NULL,
  source_text text NOT NULL,
  embedding vector NOT NULL,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (message_id, segment, chunk_index)
);

CREATE INDEX IF NOT EXISTS idx_message_embeddings_message_id
  ON message_embeddings(message_id);
