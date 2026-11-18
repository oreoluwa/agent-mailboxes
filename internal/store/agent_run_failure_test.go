package store

import (
	"context"
	"errors"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func TestFailAgentConversationRunAtomicallyFailsStartedToolsAndPreservesDraft(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := &Postgres{DB: db}
	claims := model.TokenClaims{Scopes: map[model.Scope]bool{model.ScopeAdmin: true}}
	draftID := "draft-1"

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT alias_id::text, run_token, status\n\t\tFROM agent_conversations WHERE id=$1 AND mailbox_id=$2 FOR UPDATE")).
		WithArgs("conv-1", "m1").
		WillReturnRows(sqlmock.NewRows([]string{"alias_id", "run_token", "status"}).AddRow(nil, "owner-token", string(model.ConversationStatusRunning)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT mailbox_id::text, alias_id::text, conversation_id::text\n\t\t\tFROM drafts WHERE id=$1 FOR UPDATE")).
		WithArgs(draftID).
		WillReturnRows(sqlmock.NewRows([]string{"mailbox_id", "alias_id", "conversation_id"}).AddRow("m1", nil, "conv-1"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tool_calls\n\t\tSET status=$1, result_summary='{}'::jsonb, error_text=$2, completed_at=$3\n\t\tWHERE conversation_id=$4 AND status=$5")).
		WithArgs(string(model.ToolCallStatusFailed), "planner failed", sqlmock.AnyArg(), "conv-1", string(model.ToolCallStatusStarted)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_conversations SET status=$1, updated_at=$2, last_draft_id=$3 WHERE id=$4 AND mailbox_id=$5 AND run_token=$6 AND status<>$7")).
		WithArgs(string(model.ConversationStatusFailed), sqlmock.AnyArg(), draftID, "conv-1", "m1", "owner-token", string(model.ConversationStatusCompleted)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := p.FailAgentConversationRun(context.Background(), claims, "m1", "conv-1", "owner-token", &draftID, "planner failed"); err != nil {
		t.Fatalf("FailAgentConversationRun() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFailAgentConversationRunCannotOverwriteSuccessorOrCompletedRun(t *testing.T) {
	for _, tc := range []struct {
		name        string
		storedToken any
		status      model.ConversationStatus
	}{
		{name: "successor owns expired run", storedToken: "successor-token", status: model.ConversationStatusRunning},
		{name: "completion already persisted", storedToken: "old-token", status: model.ConversationStatusCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			p := &Postgres{DB: db}
			claims := model.TokenClaims{Scopes: map[model.Scope]bool{model.ScopeAdmin: true}}

			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT alias_id::text, run_token, status\n\t\tFROM agent_conversations WHERE id=$1 AND mailbox_id=$2 FOR UPDATE")).
				WithArgs("conv-1", "m1").
				WillReturnRows(sqlmock.NewRows([]string{"alias_id", "run_token", "status"}).AddRow(nil, tc.storedToken, string(tc.status)))
			mock.ExpectRollback()

			err = p.FailAgentConversationRun(context.Background(), claims, "m1", "conv-1", "old-token", nil, "timeout")
			if !errors.Is(err, ErrAgentRunConflict) {
				t.Fatalf("FailAgentConversationRun() error = %v, want run conflict", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
