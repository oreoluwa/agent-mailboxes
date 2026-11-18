package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

const agentStoreIntegrationDSNEnv = "AGENT_EMAILS_AGENT_STORE_TEST_DSN"

func TestAgentStoreAliasOwnershipAndDraftDeliveryLifecycle(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(agentStoreIntegrationDSNEnv))
	if dsn == "" {
		t.Skipf("set %s to a dedicated empty PostgreSQL database", agentStoreIntegrationDSNEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("connect dedicated PostgreSQL database: %v", err)
	}
	var publicTables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&publicTables); err != nil {
		t.Fatal(err)
	}
	if publicTables != 0 {
		t.Fatalf("%s must reference a dedicated empty database; found %d public tables", agentStoreIntegrationDSNEnv, publicTables)
	}

	migrationsDir := integrationMigrationsDir(t)
	if err := ApplyMigrations(ctx, db, copyMigrationsBefore(t, migrationsDir, "0011_agent_alias_ownership.sql")); err != nil {
		t.Fatalf("apply schema through 0010: %v", err)
	}

	const (
		mailboxID       = "10000000-0000-0000-0000-000000000001"
		folderID        = "10000000-0000-0000-0000-000000000002"
		aliasA          = "10000000-0000-0000-0000-000000000003"
		aliasB          = "10000000-0000-0000-0000-000000000004"
		messageID       = "10000000-0000-0000-0000-000000000005"
		messageAID      = "10000000-0000-0000-0000-000000000013"
		messageBID      = "10000000-0000-0000-0000-000000000014"
		legacyConvID    = "10000000-0000-0000-0000-000000000006"
		legacyDraftID   = "10000000-0000-0000-0000-000000000007"
		legacyRevision  = "10000000-0000-0000-0000-000000000008"
		legacyOutbound  = "10000000-0000-0000-0000-000000000009"
		queuedDraftID   = "10000000-0000-0000-0000-000000000010"
		queuedRevision  = "10000000-0000-0000-0000-000000000011"
		queuedOutbound  = "10000000-0000-0000-0000-000000000012"
		threadMessageID = "<shared-thread@example.test>"
	)
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `INSERT INTO mailboxes
(id,name,enabled,imap_host,imap_port,imap_tls,smtp_host,smtp_port,smtp_tls,username,password_encrypted,created_at)
VALUES ($1,'agent-store',true,'imap.invalid',993,true,'smtp.invalid',587,true,'user','password',$2)`, mailboxID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO folders (id,mailbox_id,name,uidvalidity,last_seen_uid,updated_at) VALUES ($1,$2,'INBOX',1,1,$3)`, folderID, mailboxID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO aliases (id,mailbox_id,email,enabled,created_at) VALUES
($1,$3,'user+alpha@example.test',true,$4),($2,$3,'user+beta@example.test',true,$4)`, aliasA, aliasB, mailboxID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages
(id,mailbox_id,folder_id,imap_uid,uidvalidity,message_id,internal_date,from_email,subject,raw_headers,has_attachments,spam_flag,body_state,created_at)
VALUES ($1,$2,$3,1,1,$4,$5,'sender@example.test','Shared thread','',false,false,'fetched',$5)`, messageID, mailboxID, folderID, threadMessageID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages
(id,mailbox_id,folder_id,imap_uid,uidvalidity,message_id,in_reply_to,internal_date,from_email,subject,raw_headers,has_attachments,spam_flag,body_state,created_at)
VALUES ($1,$3,$4,2,1,'<alpha-child@example.test>',$5,$6,'alpha@example.test','Alpha child','',false,false,'fetched',$6),
       ($2,$3,$4,3,1,'<beta-child@example.test>',$5,$7,'beta@example.test','Beta child','',false,false,'fetched',$7)`, messageAID, messageBID, mailboxID, folderID, threadMessageID, now.Add(time.Second), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO message_audience (message_id,alias_id) VALUES ($1,$2),($1,$3),($4,$2),($5,$3)`, messageID, aliasA, aliasB, messageAID, messageBID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_conversations (id,mailbox_id,message_id,thread_rfc_message_id,status,created_at,updated_at)
VALUES ($1,$2,$3,$4,'completed',$5,$5)`, legacyConvID, mailboxID, messageID, "<legacy@example.test>", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO drafts
(id,mailbox_id,conversation_id,source_message_id,status,send_idempotency_key,created_at,updated_at)
VALUES ($1,$2,$3,$4,'sent','legacy-dead',$5,$5),($6,$2,$3,$4,'sent','legacy-pending',$5,$5)`, legacyDraftID, mailboxID, legacyConvID, messageID, now, queuedDraftID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO draft_revisions
(id,draft_id,revision_number,provenance,subject,to_emails,body,created_at)
VALUES ($1,$2,1,'legacy','legacy','["recipient@example.test"]','legacy',$5),
       ($3,$4,1,'legacy','legacy','["recipient@example.test"]','legacy',$5)`, legacyRevision, legacyDraftID, queuedRevision, queuedDraftID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE drafts AS d SET latest_revision_id=v.revision_id::uuid,sent_revision_id=v.revision_id::uuid
		FROM (VALUES ($1,$2),($3,$4)) AS v(draft_id,revision_id) WHERE d.id=v.draft_id::uuid`, legacyDraftID, legacyRevision, queuedDraftID, queuedRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO outbound_messages
(id,mailbox_id,alias_id,from_email,to_emails,raw_rfc822,idempotency_key,status,attempts,next_attempt_at,accepted_at,created_at,updated_at)
VALUES ($1,$2,$3,'user+alpha@example.test','["recipient@example.test"]','legacy','legacy-dead','dead',4,$5,$5,$5,$5),
       ($4,$2,$3,'user+alpha@example.test','["recipient@example.test"]','legacy','legacy-pending','pending',0,$5,$5,$5,$5)`, legacyOutbound, mailboxID, aliasA, queuedOutbound, now); err != nil {
		t.Fatal(err)
	}

	if err := ApplyMigrations(ctx, db, migrationsDir); err != nil {
		t.Fatalf("upgrade through 0011: %v", err)
	}
	var legacyStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM drafts WHERE id=$1`, legacyDraftID).Scan(&legacyStatus); err != nil {
		t.Fatal(err)
	}
	if legacyStatus != string(model.DraftStatusFailed) {
		t.Fatalf("legacy dead draft status = %q, want failed", legacyStatus)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM drafts WHERE id=$1`, queuedDraftID).Scan(&legacyStatus); err != nil {
		t.Fatal(err)
	}
	if legacyStatus != string(model.DraftStatusQueued) {
		t.Fatalf("legacy pending draft status = %q, want queued", legacyStatus)
	}

	p := &Postgres{DB: db}
	settings, err := p.UpdateMailboxAgentSettings(ctx, mailboxID, UpdateMailboxAgentSettingsInput{SystemPrompt: "initial"})
	if err != nil || settings.MaxToolSteps != 6 || settings.ToolTimeoutSec != 45 {
		t.Fatalf("initial omitted limits = %+v err=%v, want 6/45", settings, err)
	}
	settings, err = p.UpdateMailboxAgentSettings(ctx, mailboxID, UpdateMailboxAgentSettingsInput{SystemPrompt: "explicit", MaxToolSteps: 9, ToolTimeoutSec: 77})
	if err != nil || settings.MaxToolSteps != 9 || settings.ToolTimeoutSec != 77 {
		t.Fatalf("explicit limits = %+v err=%v, want 9/77", settings, err)
	}
	settings, err = p.UpdateMailboxAgentSettings(ctx, mailboxID, UpdateMailboxAgentSettingsInput{SystemPrompt: "prompt only"})
	if err != nil || settings.SystemPrompt != "prompt only" || settings.MaxToolSteps != 9 || settings.ToolTimeoutSec != 77 {
		t.Fatalf("prompt-only update = %+v err=%v, want preserved 9/77", settings, err)
	}
	if _, err := p.UpdateMailboxAgentSettings(ctx, mailboxID, UpdateMailboxAgentSettingsInput{MaxToolSteps: -1}); err == nil {
		t.Fatal("negative max tool steps unexpectedly accepted")
	}
	if _, err := p.UpdateMailboxAgentSettings(ctx, mailboxID, UpdateMailboxAgentSettingsInput{ToolTimeoutSec: -1}); err == nil {
		t.Fatal("negative tool timeout unexpectedly accepted")
	}

	if _, err := p.UpdateMailboxAgentSettings(ctx, mailboxID, UpdateMailboxAgentSettingsInput{EnabledTools: []string{"read_message"}}); err != nil {
		t.Fatal(err)
	}
	settings, err = p.UpdateMailboxAgentSettings(ctx, mailboxID, UpdateMailboxAgentSettingsInput{SystemPrompt: "preserve restrictive tools"})
	if err != nil || len(settings.EnabledTools) != 1 || settings.EnabledTools[0] != "read_message" {
		t.Fatalf("omission widened tools: %+v %v", settings, err)
	}
	settings, err = p.UpdateMailboxAgentSettings(ctx, mailboxID, UpdateMailboxAgentSettingsInput{EnabledTools: []string{}})
	if err != nil || len(settings.EnabledTools) != 0 {
		t.Fatalf("explicit empty tools not honored: %+v %v", settings, err)
	}
	settings, err = p.UpdateMailboxAgentSettings(ctx, mailboxID, UpdateMailboxAgentSettingsInput{SystemPrompt: "keep all tools disabled"})
	if err != nil || len(settings.EnabledTools) != 0 {
		t.Fatalf("omission restored disabled tools: %+v %v", settings, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO actor_request_replays (mailbox_id,request_id,nonce,action,target_id,expires_at)
		VALUES ($1,'expired','expired','send','draft',now()-interval '1 minute'),
		       ($1,'live','live','send','draft',now()+interval '1 hour')`, mailboxID); err != nil {
		t.Fatal(err)
	}
	if err := p.RecordActorReplay(ctx, mailboxID, "fresh", "fresh", "send", "draft", now.Add(time.Hour)); err != nil {
		t.Fatalf("record replay marker: %v", err)
	}
	var expiredCount, liveCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE request_id='expired'), count(*) FILTER (WHERE request_id IN ('live','fresh')) FROM actor_request_replays`).Scan(&expiredCount, &liveCount); err != nil {
		t.Fatal(err)
	}
	if expiredCount != 0 || liveCount != 2 {
		t.Fatalf("replay cleanup counts expired=%d live=%d, want 0/2", expiredCount, liveCount)
	}
	if err := p.RecordActorReplay(ctx, mailboxID, "live", "live", "send", "draft", now.Add(time.Hour)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("duplicate live replay err = %v, want unauthorized", err)
	}
	claimsA := model.TokenClaims{AliasID: stringPointer(aliasA), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}
	claimsB := model.TokenClaims{AliasID: stringPointer(aliasB), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true, model.ScopeAdmin: true}}
	admin := model.TokenClaims{Scopes: map[model.Scope]bool{model.ScopeAdmin: true}}
	threadA, err := p.GetThreadContextForMessage(ctx, claimsA, mailboxID, messageID)
	if err != nil || len(threadA) != 2 || threadA[0].RFCMessageID != threadMessageID || threadA[1].MessageID != messageAID {
		t.Fatalf("alias A thread = %+v err=%v", threadA, err)
	}
	threadB, err := p.GetThreadContextForMessage(ctx, claimsB, mailboxID, messageID)
	if err != nil || len(threadB) != 2 || threadB[0].RFCMessageID != threadMessageID || threadB[1].MessageID != messageBID {
		t.Fatalf("alias B thread = %+v err=%v", threadB, err)
	}
	sharedClaims := model.TokenClaims{MailboxID: stringPointer(mailboxID), Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	sharedThread, err := p.GetThreadContextForMessage(ctx, sharedClaims, mailboxID, messageID)
	if err != nil || len(sharedThread) != 1 || sharedThread[0].MessageID != messageID {
		t.Fatalf("shared summary thread = %+v err=%v", sharedThread, err)
	}
	if _, err := p.GetThreadContextForMessage(ctx, claimsA, mailboxID, messageBID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("alias A target B-only thread err = %v, want unauthorized", err)
	}
	if _, err := p.GetDraft(ctx, claimsA, mailboxID, legacyDraftID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("alias read legacy unowned draft err = %v, want unauthorized", err)
	}
	if _, err := p.GetDraft(ctx, admin, mailboxID, legacyDraftID); err != nil {
		t.Fatalf("admin read legacy unowned draft: %v", err)
	}

	convA, err := p.CreateAgentConversation(ctx, claimsA, mailboxID, stringPointer(messageID), stringPointer(threadMessageID), nil)
	if err != nil {
		t.Fatal(err)
	}
	convB, err := p.CreateAgentConversation(ctx, claimsB, mailboxID, stringPointer(messageID), stringPointer(threadMessageID), nil)
	if err != nil {
		t.Fatal(err)
	}
	if convA.ID == convB.ID || convA.AliasID == nil || *convA.AliasID != aliasA || convB.AliasID == nil || *convB.AliasID != aliasB {
		t.Fatalf("colliding thread ownership incorrect: A=%+v B=%+v", convA, convB)
	}
	claimed, err := p.ClaimAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "run-1", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("initial conversation run claim = %v err=%v", claimed, err)
	}
	claimed, err = p.ClaimAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "run-2", time.Minute)
	if err != nil || claimed {
		t.Fatalf("overlapping conversation run claim = %v err=%v, want false", claimed, err)
	}
	if released, err := p.ReleaseAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "stale-run"); err != nil || released {
		t.Fatalf("stale run release = %v err=%v, want false", released, err)
	}
	if released, err := p.ReleaseAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "run-1"); err != nil || !released {
		t.Fatalf("owned run release = %v err=%v, want true", released, err)
	}
	type claimResult struct {
		token   string
		claimed bool
		err     error
	}
	startClaims := make(chan struct{})
	claimResults := make(chan claimResult, 8)
	for i := 0; i < cap(claimResults); i++ {
		token := fmt.Sprintf("concurrent-%d", i)
		go func() {
			<-startClaims
			got, claimErr := p.ClaimAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, token, time.Minute)
			claimResults <- claimResult{token: token, claimed: got, err: claimErr}
		}()
	}
	close(startClaims)
	concurrentWinner := ""
	for i := 0; i < cap(claimResults); i++ {
		result := <-claimResults
		if result.err != nil {
			t.Fatalf("concurrent run claim %s: %v", result.token, result.err)
		}
		if result.claimed {
			if concurrentWinner != "" {
				t.Fatalf("multiple concurrent run claims won: %s and %s", concurrentWinner, result.token)
			}
			concurrentWinner = result.token
		}
	}
	if concurrentWinner == "" {
		t.Fatal("no concurrent run claim won")
	}
	if released, err := p.ReleaseAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, concurrentWinner); err != nil || !released {
		t.Fatalf("concurrent winner release = %v err=%v", released, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE agent_conversations SET run_token='expired', run_lease_until=CURRENT_TIMESTAMP-interval '1 second' WHERE id=$1`, convA.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = p.ClaimAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "recovered", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("expired conversation run recovery = %v err=%v", claimed, err)
	}
	if released, err := p.ReleaseAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "recovered"); err != nil || !released {
		t.Fatalf("recovered run release = %v err=%v", released, err)
	}
	if _, err := p.ClaimAgentConversationRun(ctx, claimsB, mailboxID, convA.ID, "cross-alias", time.Minute); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-alias run claim err = %v, want unauthorized", err)
	}
	convAAgain, err := p.CreateAgentConversation(ctx, claimsA, mailboxID, stringPointer(messageID), stringPointer(threadMessageID), nil)
	if err != nil || convAAgain.ID != convA.ID {
		t.Fatalf("same-alias thread dedupe = %+v err=%v, want %s", convAAgain, err, convA.ID)
	}
	if _, err := p.GetAgentConversation(ctx, claimsB, mailboxID, convA.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-alias conversation get err = %v, want unauthorized", err)
	}
	if _, err := p.AddAgentConversationMessage(ctx, claimsB, mailboxID, convA.ID, "user", "cross-alias", nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-alias conversation mutation err = %v, want unauthorized", err)
	}
	if _, err := p.AddAgentConversationMessage(ctx, claimsA, mailboxID, convA.ID, "assistant", "full history", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.StartAgentToolCall(ctx, claimsA, mailboxID, convA.ID, "read_message", nil, "allow"); err != nil {
		t.Fatal(err)
	}
	listedA, err := p.ListAgentConversations(ctx, claimsA, mailboxID, AgentConversationListOptions{})
	if err != nil || len(listedA) != 1 || listedA[0].ID != convA.ID {
		t.Fatalf("alias A conversations = %+v err=%v", listedA, err)
	}
	if listedA[0].Messages != nil || listedA[0].ToolCalls != nil {
		t.Fatalf("conversation list loaded history: messages=%+v tools=%+v", listedA[0].Messages, listedA[0].ToolCalls)
	}
	listedB, err := p.ListAgentConversations(ctx, claimsB, mailboxID, AgentConversationListOptions{})
	if err != nil || len(listedB) != 1 || listedB[0].ID != convB.ID {
		t.Fatalf("alias B conversations = %+v err=%v", listedB, err)
	}
	bySource, err := p.ListAgentConversations(ctx, claimsA, mailboxID, AgentConversationListOptions{SourceMessageID: stringPointer(messageID)})
	if err != nil || len(bySource) != 1 || bySource[0].ID != convA.ID {
		t.Fatalf("source-filtered conversations = %+v err=%v", bySource, err)
	}
	byThread, err := p.ListAgentConversations(ctx, claimsA, mailboxID, AgentConversationListOptions{ThreadRFCMessageID: stringPointer(threadMessageID)})
	if err != nil || len(byThread) != 1 || byThread[0].ID != convA.ID {
		t.Fatalf("thread-filtered conversations = %+v err=%v", byThread, err)
	}
	detailA, err := p.GetAgentConversation(ctx, claimsA, mailboxID, convA.ID)
	if err != nil || len(detailA.Messages) != 1 || len(detailA.ToolCalls) != 1 {
		t.Fatalf("conversation detail = %+v err=%v, want full history", detailA, err)
	}

	bulkRows := make([]agentConversationRow, 0, 204)
	for i := 0; i < 204; i++ {
		createdAt := now.Add(time.Duration(i+10) * time.Second)
		bulkRows = append(bulkRows, agentConversationRow{
			ID: fmt.Sprintf("20000000-0000-0000-0000-%012d", i), MailboxID: mailboxID, AliasID: stringPointer(aliasA),
			Status: string(model.ConversationStatusCompleted), CreatedAt: createdAt, UpdatedAt: createdAt,
		})
	}
	if _, err := p.bun().NewInsert().Model(&bulkRows).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	defaultList, err := p.ListAgentConversations(ctx, claimsA, mailboxID, AgentConversationListOptions{})
	if err != nil || len(defaultList) != 100 {
		t.Fatalf("default conversation list count = %d err=%v, want 100", len(defaultList), err)
	}
	maxList, err := p.ListAgentConversations(ctx, claimsA, mailboxID, AgentConversationListOptions{Limit: 999})
	if err != nil || len(maxList) != 200 {
		t.Fatalf("capped conversation list count = %d err=%v, want 200", len(maxList), err)
	}
	shortList, err := p.ListAgentConversations(ctx, claimsA, mailboxID, AgentConversationListOptions{Limit: 3})
	if err != nil || len(shortList) != 3 {
		t.Fatalf("short conversation list count = %d err=%v, want 3", len(shortList), err)
	}

	inReplyTo := "<parent@example.test>"
	references := "<root@example.test> <parent@example.test>"
	draft, err := p.CreateDraft(ctx, claimsA, CreateDraftInput{
		MailboxID: mailboxID, ConversationID: &convA.ID, SourceMessageID: stringPointer(messageID),
		Subject: "Re: Shared", To: []string{"recipient@example.test"}, Body: "first",
		InReplyTo: &inReplyTo, References: &references, Provenance: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetDraft(ctx, claimsB, mailboxID, draft.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-alias draft get err = %v, want unauthorized", err)
	}
	if _, err := p.CreateDraft(ctx, claimsA, CreateDraftInput{MailboxID: mailboxID, ConversationID: &convB.ID, Subject: "bad", To: []string{"recipient@example.test"}}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-alias conversation draft err = %v, want unauthorized", err)
	}
	firstRevisionID := draft.LatestRevisionID
	draft, err = p.ReviseDraft(ctx, claimsA, ReviseDraftInput{
		MailboxID: mailboxID, DraftID: draft.ID, Subject: "Re: Shared edited", To: []string{"recipient@example.test"}, Body: "second", Provenance: "human",
	})
	if err != nil {
		t.Fatal(err)
	}
	if draft.LatestRevision.InReplyTo == nil || *draft.LatestRevision.InReplyTo != inReplyTo || draft.LatestRevision.References == nil || *draft.LatestRevision.References != references {
		t.Fatalf("reply headers not inherited: %+v", draft.LatestRevision)
	}
	discardedDraft, err := p.CreateDraft(ctx, claimsA, CreateDraftInput{
		MailboxID: mailboxID, ConversationID: &convA.ID, SourceMessageID: stringPointer(messageID),
		Subject: "discarded", To: []string{"recipient@example.test"}, Body: "discarded", Provenance: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	queuedFinalizeDraft, err := p.CreateDraft(ctx, claimsA, CreateDraftInput{
		MailboxID: mailboxID, ConversationID: &convA.ID, SourceMessageID: stringPointer(messageID),
		Subject: "queued", To: []string{"recipient@example.test"}, Body: "queued", Provenance: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE drafts SET status=$1 WHERE id=$2`, string(model.DraftStatusDiscarded), discardedDraft.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE drafts SET status=$1 WHERE id=$2`, string(model.DraftStatusQueued), queuedFinalizeDraft.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = p.ClaimAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "finalize-run", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim finalize run = %v err=%v", claimed, err)
	}
	if err := p.CompleteAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "stale-finalize", draft.ID, stringPointer(messageID), "stale"); !errors.Is(err, ErrAgentRunConflict) {
		t.Fatalf("stale-token finalize err = %v, want conflict", err)
	}
	if err := p.CompleteAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "finalize-run", draft.ID, stringPointer(messageAID), "wrong source"); err == nil || !strings.Contains(err.Error(), "source message") {
		t.Fatalf("wrong-source finalize err = %v", err)
	}
	if err := p.CompleteAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "finalize-run", discardedDraft.ID, stringPointer(messageID), "discarded"); err == nil || !strings.Contains(err.Error(), "not editable") {
		t.Fatalf("discarded finalize err = %v", err)
	}
	if err := p.CompleteAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "finalize-run", queuedFinalizeDraft.ID, stringPointer(messageID), "queued"); err == nil || !strings.Contains(err.Error(), "not editable") {
		t.Fatalf("queued finalize err = %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE agent_conversations SET run_lease_until=CURRENT_TIMESTAMP-interval '1 second' WHERE id=$1`, convA.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.CompleteAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "finalize-run", draft.ID, stringPointer(messageID), "expired"); !errors.Is(err, ErrAgentRunConflict) {
		t.Fatalf("expired-lease finalize err = %v, want conflict", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE agent_conversations SET run_lease_until=CURRENT_TIMESTAMP+interval '1 minute' WHERE id=$1`, convA.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.CompleteAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "finalize-run", draft.ID, stringPointer(messageID), "atomic completion"); err != nil {
		t.Fatalf("valid finalize: %v", err)
	}
	var finalizedStatus string
	var finalizedDraftID string
	var finalizedRunToken sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT status,last_draft_id::text,run_token FROM agent_conversations WHERE id=$1`, convA.ID).
		Scan(&finalizedStatus, &finalizedDraftID, &finalizedRunToken); err != nil {
		t.Fatal(err)
	}
	if finalizedStatus != string(model.ConversationStatusCompleted) || finalizedDraftID != draft.ID || !finalizedRunToken.Valid || finalizedRunToken.String != "finalize-run" {
		t.Fatalf("finalized conversation status=%q draft=%q token=%+v", finalizedStatus, finalizedDraftID, finalizedRunToken)
	}
	var completionTurns int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM agent_messages WHERE conversation_id=$1 AND role='assistant' AND content='atomic completion'`, convA.ID).Scan(&completionTurns); err != nil {
		t.Fatal(err)
	}
	if completionTurns != 1 {
		t.Fatalf("completion turns = %d, want 1", completionTurns)
	}
	claimed, err = p.ClaimAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "successor-before-release", time.Minute)
	if err != nil || claimed {
		t.Fatalf("successor claim before owner release = %v err=%v, want false", claimed, err)
	}
	if released, err := p.ReleaseAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "finalize-run"); err != nil || !released {
		t.Fatalf("release finalized run = %v err=%v", released, err)
	}
	claimed, err = p.ClaimAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "successor-after-release", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("successor claim after owner release = %v err=%v, want true", claimed, err)
	}
	if released, err := p.ReleaseAgentConversationRun(ctx, claimsA, mailboxID, convA.ID, "successor-after-release"); err != nil || !released {
		t.Fatalf("release successor run = %v err=%v", released, err)
	}

	writeErrorDraft, err := p.CreateDraft(ctx, claimsB, CreateDraftInput{
		MailboxID: mailboxID, ConversationID: &convB.ID, SourceMessageID: stringPointer(messageID),
		Subject: "write error", To: []string{"recipient@example.test"}, Body: "write error", Provenance: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = p.ClaimAgentConversationRun(ctx, claimsB, mailboxID, convB.ID, "write-error-run", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim write-error run = %v err=%v", claimed, err)
	}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION reject_agent_completion() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.content = 'force-write-error' THEN RAISE EXCEPTION 'forced completion write failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_agent_completion BEFORE INSERT ON agent_messages FOR EACH ROW EXECUTE FUNCTION reject_agent_completion()`); err != nil {
		t.Fatal(err)
	}
	if err := p.CompleteAgentConversationRun(ctx, claimsB, mailboxID, convB.ID, "write-error-run", writeErrorDraft.ID, stringPointer(messageID), "force-write-error"); err == nil {
		t.Fatal("forced completion write error unexpectedly succeeded")
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER reject_agent_completion ON agent_messages; DROP FUNCTION reject_agent_completion()`); err != nil {
		t.Fatal(err)
	}
	var writeErrorStatus string
	var writeErrorLastDraft sql.NullString
	var writeErrorRunToken sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT status,last_draft_id::text,run_token FROM agent_conversations WHERE id=$1`, convB.ID).
		Scan(&writeErrorStatus, &writeErrorLastDraft, &writeErrorRunToken); err != nil {
		t.Fatal(err)
	}
	if writeErrorStatus == string(model.ConversationStatusCompleted) || writeErrorLastDraft.Valid || !writeErrorRunToken.Valid || writeErrorRunToken.String != "write-error-run" {
		t.Fatalf("write-error finalize partially committed: status=%q draft=%+v token=%+v", writeErrorStatus, writeErrorLastDraft, writeErrorRunToken)
	}
	if released, err := p.ReleaseAgentConversationRun(ctx, claimsB, mailboxID, convB.ID, "write-error-run"); err != nil || !released {
		t.Fatalf("release write-error run = %v err=%v", released, err)
	}
	if _, err := p.SendDraft(ctx, claimsA, SendDraftInput{MailboxID: mailboxID, DraftID: draft.ID, AliasID: aliasA, FromEmail: "user+alpha@example.test", ReviewedRevisionID: firstRevisionID}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale revision send err = %v", err)
	}
	queued, err := p.SendDraft(ctx, claimsA, SendDraftInput{MailboxID: mailboxID, DraftID: draft.ID, AliasID: aliasA, FromEmail: "user+alpha@example.test", ReviewedRevisionID: draft.LatestRevisionID})
	if err != nil || queued.Draft.Status != model.DraftStatusQueued {
		t.Fatalf("queue draft = %+v err=%v", queued, err)
	}
	if _, err := p.ReviseDraft(ctx, claimsA, ReviseDraftInput{MailboxID: mailboxID, DraftID: draft.ID, Subject: "blocked", To: []string{"recipient@example.test"}, Body: "blocked"}); err == nil {
		t.Fatal("queued draft revision unexpectedly succeeded")
	}
	if err := p.MarkOutboundMessagesDead(ctx, []string{queued.Outbound.ID}, "terminal"); err != nil {
		t.Fatal(err)
	}
	failed, err := p.GetDraft(ctx, claimsA, mailboxID, draft.ID)
	if err != nil || failed.Status != model.DraftStatusFailed {
		t.Fatalf("failed draft = %+v err=%v", failed, err)
	}
	requeued, err := p.SendDraft(ctx, claimsA, SendDraftInput{MailboxID: mailboxID, DraftID: draft.ID, AliasID: aliasA, FromEmail: "user+alpha@example.test", ReviewedRevisionID: failed.LatestRevisionID})
	if err != nil || requeued.Draft.Status != model.DraftStatusQueued || !requeued.Idempotent || requeued.Outbound.Status != "pending" {
		t.Fatalf("explicit dead retry = %+v err=%v", requeued, err)
	}
	if err := p.MarkOutboundMessagesSent(ctx, []string{requeued.Outbound.ID}); err != nil {
		t.Fatal(err)
	}
	sent, err := p.GetDraft(ctx, claimsA, mailboxID, draft.ID)
	if err != nil || sent.Status != model.DraftStatusSent {
		t.Fatalf("sent draft = %+v err=%v", sent, err)
	}

	autoDraftJob, _, err := p.QueueAutoDraftJob(ctx, mailboxID, messageID, 42)
	if err != nil {
		t.Fatalf("queue auto-draft completion fixture: %v", err)
	}
	// QueueAutoDraftJob timestamps new rows with the application clock while
	// ClaimAutoDraftJob compares against the database clock. Make this immediate
	// claim fixture deterministic when the Docker database clock trails the host.
	if _, err := db.ExecContext(ctx, `UPDATE auto_draft_jobs SET next_attempt_at=now() WHERE id=$1`, autoDraftJob.ID); err != nil {
		t.Fatalf("make auto-draft completion fixture claimable: %v", err)
	}
	claimedJob, claimed, err := p.ClaimAutoDraftJob(ctx, mailboxID, 5*time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim auto-draft job = %+v claimed=%v err=%v", claimedJob, claimed, err)
	}
	if claimedJob.ID != autoDraftJob.ID || claimedJob.ClaimToken == "" {
		t.Fatalf("claimed auto-draft job = %+v", claimedJob)
	}
	if _, secondClaim, err := (&Postgres{DB: db}).ClaimAutoDraftJob(ctx, mailboxID, 5*time.Minute); err != nil || secondClaim {
		t.Fatalf("concurrent worker claim claimed=%v err=%v", secondClaim, err)
	}
	if completed, err := p.CompleteAutoDraftAlias(ctx, autoDraftJob.ID, aliasA, claimedJob.ClaimToken); err != nil || !completed {
		t.Fatalf("complete alias A completed=%v err=%v", completed, err)
	}
	if completed, err := p.CompleteAutoDraftAlias(ctx, autoDraftJob.ID, aliasA, claimedJob.ClaimToken); err != nil || !completed {
		t.Fatalf("deduplicate alias A completion completed=%v err=%v", completed, err)
	}
	if completed, err := p.CompleteAutoDraftAlias(ctx, autoDraftJob.ID, aliasB, "00000000-0000-0000-0000-000000000000"); err != nil || completed {
		t.Fatalf("stale completion completed=%v err=%v", completed, err)
	}
	if renewed, err := p.RenewAutoDraftJobLease(ctx, autoDraftJob.ID, claimedJob.ClaimToken, 5*time.Minute); err != nil || !renewed {
		t.Fatalf("renew claimed job renewed=%v err=%v", renewed, err)
	}
	restartedStore := &Postgres{DB: db}
	completedAliases, err := restartedStore.CompletedAutoDraftAliases(ctx, autoDraftJob.ID)
	if err != nil {
		t.Fatalf("read persisted alias completions: %v", err)
	}
	if len(completedAliases) != 1 || completedAliases[0] != aliasA {
		t.Fatalf("persisted alias completions = %#v, want alias A once", completedAliases)
	}
	if _, err := db.ExecContext(ctx, `UPDATE auto_draft_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, autoDraftJob.ID); err != nil {
		t.Fatal(err)
	}
	if renewed, err := p.RenewAutoDraftJobLease(ctx, autoDraftJob.ID, claimedJob.ClaimToken, 5*time.Minute); err != nil || renewed {
		t.Fatalf("expired owner renewal renewed=%v err=%v", renewed, err)
	}
	if done, err := p.MarkAutoDraftJobDone(ctx, autoDraftJob.ID, claimedJob.ClaimToken); err != nil || done {
		t.Fatalf("expired owner completion done=%v err=%v", done, err)
	}
	successor, claimed, err := restartedStore.ClaimAutoDraftJob(ctx, mailboxID, 5*time.Minute)
	if err != nil || !claimed || successor.ClaimToken == claimedJob.ClaimToken {
		t.Fatalf("successor claim = %+v claimed=%v err=%v", successor, claimed, err)
	}
	if completed, err := restartedStore.CompleteAutoDraftAlias(ctx, autoDraftJob.ID, aliasB, successor.ClaimToken); err != nil || !completed {
		t.Fatalf("complete alias B after reclaim completed=%v err=%v", completed, err)
	}
	completedAliases, err = p.CompletedAutoDraftAliases(ctx, autoDraftJob.ID)
	if err != nil {
		t.Fatalf("read both persisted alias completions: %v", err)
	}
	completedSet := map[string]bool{}
	for _, aliasID := range completedAliases {
		completedSet[aliasID] = true
	}
	if len(completedSet) != 2 || !completedSet[aliasA] || !completedSet[aliasB] {
		t.Fatalf("persisted alias completion set = %#v, want aliases A and B", completedAliases)
	}
	if retried, err := restartedStore.MarkAutoDraftJobRetry(ctx, autoDraftJob.ID, successor.ClaimToken, "retry", time.Minute); err != nil || !retried {
		t.Fatalf("successor retry retried=%v err=%v", retried, err)
	}
	if done, err := p.MarkAutoDraftJobDone(ctx, autoDraftJob.ID, successor.ClaimToken); err != nil || done {
		t.Fatalf("stale post-retry completion done=%v err=%v", done, err)
	}
}
