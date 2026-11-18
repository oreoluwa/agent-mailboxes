package store

import "context"

func (p *Postgres) CompletedAutoDraftAliases(ctx context.Context, jobID string) ([]string, error) {
	rows, err := p.DB.QueryContext(ctx, `SELECT alias_id::text FROM auto_draft_alias_completions WHERE job_id=$1`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	aliases := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		aliases = append(aliases, id)
	}
	return aliases, rows.Err()
}

func (p *Postgres) CompleteAutoDraftAlias(ctx context.Context, jobID, aliasID, claimToken string) (bool, error) {
	result, err := p.DB.ExecContext(ctx, `WITH owned_job AS (
  SELECT id FROM auto_draft_jobs
  WHERE id=$1 AND status='processing' AND claim_token=$3 AND lease_until>now()
  FOR UPDATE
)
INSERT INTO auto_draft_alias_completions(job_id,alias_id)
SELECT id,$2 FROM owned_job
ON CONFLICT(job_id,alias_id) DO UPDATE SET completed_at=auto_draft_alias_completions.completed_at
`, jobID, aliasID, claimToken)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
