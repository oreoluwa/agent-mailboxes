package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func TestDraftThreadMembershipFilteringAndConversationStatus(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(agentStoreIntegrationDSNEnv))
	if dsn == "" {
		t.Skipf("set %s to a dedicated PostgreSQL database", agentStoreIntegrationDSNEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	schema := "draft_query_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	}()
	if _, err := db.ExecContext(ctx, `SET search_path TO `+schema+`, public`); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, db, integrationMigrationsDir(t)); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	const (
		mailboxID  = "20000000-0000-0000-0000-000000000001"
		folderID   = "20000000-0000-0000-0000-000000000002"
		aliasID    = "20000000-0000-0000-0000-000000000003"
		rootID     = "20000000-0000-0000-0000-000000000004"
		childID    = "20000000-0000-0000-0000-000000000005"
		unrelated  = "20000000-0000-0000-0000-000000000006"
		rootRFC    = "<root@example.test>"
		childRFC   = "<child@example.test>"
		otherRFC   = "<other@example.test>"
		mailboxNow = "2026-09-27T00:00:00Z"
	)
	now, _ := time.Parse(time.RFC3339, mailboxNow)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO mailboxes (id,name,enabled,imap_host,imap_port,imap_tls,smtp_host,smtp_port,smtp_tls,username,password_encrypted,created_at) VALUES ($1,'draft-query',true,'imap.invalid',993,true,'smtp.invalid',587,true,'user','password',$2)`, []any{mailboxID, now}},
		{`INSERT INTO folders (id,mailbox_id,name,uidvalidity,last_seen_uid,updated_at) VALUES ($1,$2,'INBOX',1,3,$3)`, []any{folderID, mailboxID, now}},
		{`INSERT INTO aliases (id,mailbox_id,email,enabled,created_at) VALUES ($1,$2,'user+agent@example.test',true,$3)`, []any{aliasID, mailboxID, now}},
		{`INSERT INTO messages (id,mailbox_id,folder_id,imap_uid,uidvalidity,message_id,in_reply_to,internal_date,from_email,subject,raw_headers,has_attachments,spam_flag,body_state,created_at) VALUES
			($1,$4,$5,1,1,$6,NULL,$9,'sender@example.test','Root','',false,false,'fetched',$9),
			($2,$4,$5,2,1,$7,$6,$10,'sender@example.test','Child','',false,false,'fetched',$10),
			($3,$4,$5,3,1,$8,NULL,$11,'sender@example.test','Other','',false,false,'fetched',$11)`, []any{rootID, childID, unrelated, mailboxID, folderID, rootRFC, childRFC, otherRFC, now, now.Add(time.Minute), now.Add(2 * time.Minute)}},
		{`INSERT INTO message_audience (message_id,alias_id) VALUES ($1,$4),($2,$4),($3,$4)`, []any{rootID, childID, unrelated, aliasID}},
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	p := &Postgres{DB: db}
	claims := model.TokenClaims{AliasID: stringPointer(aliasID), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}
	conversation, err := p.CreateAgentConversation(ctx, claims, mailboxID, stringPointer(rootID), stringPointer(rootRFC), nil)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := p.CreateDraft(ctx, claims, CreateDraftInput{
		MailboxID: mailboxID, ConversationID: &conversation.ID, SourceMessageID: stringPointer(childID),
		Subject: "Re: Child", To: []string{"recipient@example.test"}, Body: "first", Provenance: "test",
	})
	if err != nil {
		t.Fatalf("create draft from later visible thread message: %v", err)
	}
	if _, err := p.CreateDraft(ctx, claims, CreateDraftInput{
		MailboxID: mailboxID, ConversationID: &conversation.ID, SourceMessageID: stringPointer(unrelated),
		Subject: "Other", To: []string{"recipient@example.test"}, Body: "other", Provenance: "test",
	}); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("unrelated visible source error = %v", err)
	}
	updated, err := p.ReviseDraft(ctx, claims, ReviseDraftInput{
		MailboxID: mailboxID, DraftID: draft.ID, Subject: "Re: Child revised", To: []string{"recipient@example.test"}, Body: "second", Provenance: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateDraft(ctx, claims, CreateDraftInput{
		MailboxID: mailboxID, SourceMessageID: stringPointer(rootID), Subject: "Root", To: []string{"recipient@example.test"}, Body: "root", Provenance: "test",
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := p.ListDrafts(ctx, claims, mailboxID, DraftListOptions{
		SourceMessageID: stringPointer(childID), Status: model.DraftStatusPendingReview, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != updated.ID || listed[0].LatestRevisionID != updated.LatestRevisionID {
		t.Fatalf("filtered drafts = %+v", listed)
	}
	if len(listed[0].Revisions) != 1 || listed[0].Revisions[0].RevisionNumber != 2 {
		t.Fatalf("list should return latest revision only: %+v", listed[0].Revisions)
	}
	full, err := p.GetDraft(ctx, claims, mailboxID, draft.ID)
	if err != nil || len(full.Revisions) != 2 {
		t.Fatalf("get draft should retain revision history: revisions=%d err=%v", len(full.Revisions), err)
	}
	if err := p.SetAgentConversationStatus(ctx, claims, mailboxID, conversation.ID, model.ConversationStatusCompleted, &draft.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.SetAgentConversationStatus(ctx, claims, mailboxID, conversation.ID, model.ConversationStatusFailed, nil); err != nil {
		t.Fatal(err)
	}
	conversationAfterFailure, err := p.GetAgentConversation(ctx, claims, mailboxID, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conversationAfterFailure.LastDraftID == nil || *conversationAfterFailure.LastDraftID != draft.ID {
		t.Fatalf("failed status cleared last draft: %+v", conversationAfterFailure.LastDraftID)
	}
	if conversationAfterFailure.Status != model.ConversationStatusFailed {
		t.Fatalf("conversation status = %q", conversationAfterFailure.Status)
	}
}
