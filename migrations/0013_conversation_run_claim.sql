ALTER TABLE agent_conversations
  ADD COLUMN run_token text NULL,
  ADD COLUMN run_lease_until timestamptz NULL;

ALTER TABLE agent_conversations
  ADD CONSTRAINT agent_conversations_run_lease_pair
  CHECK ((run_token IS NULL) = (run_lease_until IS NULL));

CREATE INDEX idx_agent_conversations_run_lease_until
  ON agent_conversations(run_lease_until)
  WHERE run_token IS NOT NULL;

CREATE UNIQUE INDEX idx_agent_conversations_run_token
  ON agent_conversations(run_token)
  WHERE run_token IS NOT NULL;
