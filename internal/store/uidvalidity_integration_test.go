package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

const uidValidityIntegrationDSNEnv = "AGENT_EMAILS_UIDVALIDITY_TEST_DSN"

func TestUIDValidityMigrationAndUnsummarizedMailIntegration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(uidValidityIntegrationDSNEnv))
	if dsn == "" {
		t.Skipf("set %s to a dedicated empty PostgreSQL database", uidValidityIntegrationDSNEnv)
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
		t.Fatalf("%s must reference a dedicated empty database; found %d public tables", uidValidityIntegrationDSNEnv, publicTables)
	}

	migrationsDir := integrationMigrationsDir(t)
	preUpgradeDir := copyMigrationsBefore(t, migrationsDir, "0010_message_uidvalidity.sql")
	if err := ApplyMigrations(ctx, db, preUpgradeDir); err != nil {
		t.Fatalf("apply schema through 0009: %v", err)
	}

	const (
		mailboxID = "00000000-0000-0000-0000-000000000001"
		folderID  = "00000000-0000-0000-0000-000000000002"
		aliasID   = "00000000-0000-0000-0000-000000000003"
		oldMsgID  = "00000000-0000-0000-0000-000000000004"
	)
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `INSERT INTO mailboxes
(id,name,enabled,imap_host,imap_port,imap_tls,smtp_host,smtp_port,smtp_tls,username,password_encrypted,created_at)
VALUES ($1,'integration',true,'imap.invalid',993,true,'smtp.invalid',587,true,'user','password',$2)`, mailboxID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO folders (id,mailbox_id,name,uidvalidity,last_seen_uid,updated_at)
VALUES ($1,$2,'INBOX',11,900,$3)`, folderID, mailboxID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO aliases (id,mailbox_id,email,enabled,created_at)
VALUES ($1,$2,'user+agent@example.com',true,$3)`, aliasID, mailboxID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages
(id,mailbox_id,folder_id,imap_uid,message_id,internal_date,from_email,subject,raw_headers,has_attachments,spam_flag,body_state,created_at)
VALUES ($1,$2,$3,7,'<old@example.com>',$4,'old@example.com','old epoch','',false,false,'fetched',$4)`, oldMsgID, mailboxID, folderID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO message_audience (message_id,alias_id) VALUES ($1,$2)`, oldMsgID, aliasID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO message_bodies (message_id,text_plain,text_html,extracted_text,fetched_at)
VALUES ($1,'old body','','old body',$2)`, oldMsgID, now); err != nil {
		t.Fatal(err)
	}

	if err := ApplyMigrations(ctx, db, migrationsDir); err != nil {
		t.Fatalf("upgrade through 0010: %v", err)
	}
	if err := ApplyMigrations(ctx, db, migrationsDir); err != nil {
		t.Fatalf("repeat migration runner: %v", err)
	}
	var backfilledEpoch int64
	if err := db.QueryRowContext(ctx, `SELECT uidvalidity FROM messages WHERE id=$1`, oldMsgID).Scan(&backfilledEpoch); err != nil {
		t.Fatal(err)
	}
	if backfilledEpoch != 11 {
		t.Fatalf("backfilled old message UIDVALIDITY = %d, want 11", backfilledEpoch)
	}

	p := &Postgres{DB: db}
	if _, err := p.UpsertFolderCheckpoint(ctx, mailboxID, "INBOX", 22, 5); err != nil {
		t.Fatalf("reset folder checkpoint: %v", err)
	}
	var folderEpoch, checkpoint int64
	if err := db.QueryRowContext(ctx, `SELECT uidvalidity,last_seen_uid FROM folders WHERE id=$1`, folderID).Scan(&folderEpoch, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if folderEpoch != 22 || checkpoint != 5 {
		t.Fatalf("reset checkpoint = (%d,%d), want (22,5)", folderEpoch, checkpoint)
	}

	subject := "epochnew unsummarized"
	parsed := model.ParsedEmail{
		IMAPUID:         7,
		IMAPUIDValidity: 22,
		RFCMessageID:    "<new@example.com>",
		InternalDate:    now.Add(time.Minute),
		FromEmail:       "new@example.com",
		Subject:         &subject,
		ExtractedText:   "new epoch body",
		RawRFC822:       []byte("From: new@example.com\r\nSubject: epochnew unsummarized\r\n\r\nnew epoch body"),
	}
	newMsgID, inserted, err := p.InsertMessage(ctx, mailboxID, folderID, parsed, []string{aliasID})
	if err != nil || !inserted {
		t.Fatalf("insert new epoch message: id=%q inserted=%t err=%v", newMsgID, inserted, err)
	}
	duplicateSubject := "must not replace existing"
	parsed.Subject = &duplicateSubject
	duplicateID, inserted, err := p.InsertMessage(ctx, mailboxID, folderID, parsed, []string{aliasID})
	if err != nil || inserted || duplicateID != newMsgID {
		t.Fatalf("same-epoch dedupe: id=%q inserted=%t err=%v, want id=%q", duplicateID, inserted, err, newMsgID)
	}

	var messageCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE folder_id=$1 AND imap_uid=7`, folderID).Scan(&messageCount); err != nil {
		t.Fatal(err)
	}
	if messageCount != 2 {
		t.Fatalf("same UID across epochs count = %d, want 2", messageCount)
	}
	var oldSubject string
	if err := db.QueryRowContext(ctx, `SELECT subject FROM messages WHERE id=$1`, oldMsgID).Scan(&oldSubject); err != nil {
		t.Fatal(err)
	}
	if oldSubject != "old epoch" {
		t.Fatalf("old epoch content changed to %q", oldSubject)
	}

	claims := model.TokenClaims{AliasID: stringPointer(aliasID), Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	items, err := p.ListInbox(ctx, claims, "inbox", false, 20)
	if err != nil {
		t.Fatalf("list unsummarized mail: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("list unsummarized mail count = %d, want 2", len(items))
	}
	results, err := p.SearchStructured(ctx, claims, model.SearchRequest{
		Intent: model.SearchIntent{Text: "epochnew"},
		Mode:   model.SearchModeKeyword,
		Limit:  20,
	})
	if err != nil {
		t.Fatalf("search unsummarized mail: %v", err)
	}
	if len(results) != 1 || results[0].MsgID != newMsgID {
		t.Fatalf("search unsummarized mail = %+v, want new epoch message %s", results, newMsgID)
	}
	message, err := p.ReadMessage(ctx, claims, newMsgID)
	if err != nil {
		t.Fatalf("read unsummarized mail: %v", err)
	}
	if message.Digest != nil {
		t.Fatalf("read unsummarized mail returned digest: %+v", message.Digest)
	}
}

func integrationMigrationsDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve integration test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "migrations"))
}

func copyMigrationsBefore(t *testing.T, sourceDir, stopFilename string) string {
	t.Helper()
	destination := t.TempDir()
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= stopFilename {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(sourceDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return destination
}

func stringPointer(value string) *string { return &value }
