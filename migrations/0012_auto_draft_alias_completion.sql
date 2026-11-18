CREATE TABLE auto_draft_alias_completions (
  job_id uuid NOT NULL REFERENCES auto_draft_jobs(id) ON DELETE CASCADE,
  alias_id uuid NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
  completed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (job_id, alias_id)
);
