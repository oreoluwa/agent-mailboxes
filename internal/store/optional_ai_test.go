package store

import (
	"context"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func TestUpsertFolderCheckpointResetsUIDWhenUIDValidityChanges(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	mock.ExpectQuery(regexp.QuoteMeta("last_seen_uid = CASE WHEN folder_row.uidvalidity = EXCLUDED.uidvalidity THEN GREATEST(folder_row.last_seen_uid, EXCLUDED.last_seen_uid) ELSE EXCLUDED.last_seen_uid END")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("folder-1"))

	if _, err := p.UpsertFolderCheckpoint(context.Background(), "mailbox-1", "INBOX", 22, 4); err != nil {
		t.Fatalf("UpsertFolderCheckpoint() err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func unsummarizedListRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"msg_id", "folder", "received_at", "from_name", "from_email", "subject",
		"seen", "body_state", "thread_message_id", "thread_in_reply_to", "thread_references",
		"model_name", "summary_short", "summary_bullets", "intent", "priority",
		"required_action", "suggested_next_step", "extracted_entities", "confidence",
		"attachments_count",
	}).AddRow(
		"m1", "INBOX", time.Now(), nil, "sender@example.com", "Hello",
		false, "stored", "<m1@example.com>", nil, nil,
		"", "", "{}", "", "", "", "", []byte("{}"), 0.0, 0,
	)
}

func TestListInboxIncludesMessageWithoutDigest(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	aliasID := "a1"
	claims := model.TokenClaims{AliasID: &aliasID, Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	mock.ExpectQuery("LEFT JOIN message_digest AS md").WillReturnRows(unsummarizedListRows())

	items, err := p.ListInbox(context.Background(), claims, "inbox", false, 20)
	if err != nil {
		t.Fatalf("ListInbox() err = %v", err)
	}
	if len(items) != 1 || items[0].MsgID != "m1" {
		t.Fatalf("ListInbox() = %+v, want unsummarized message", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReadMessageWithoutDigestReturnsNilDigest(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	aliasID := "a1"
	claims := model.TokenClaims{AliasID: &aliasID, Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	mock.ExpectQuery("SELECT EXISTS").WithArgs("m1", "a1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("LEFT JOIN message_digest AS md").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "internal_date", "sent_date", "from_email", "from_name", "subject", "snippet",
			"body_state", "rfc_message_id", "in_reply_to", "references", "digest_message_id",
			"model_name", "summary_short", "summary_bullets", "intent", "priority", "required_action",
			"suggested_next_step", "extracted_entities", "confidence",
		}).AddRow(
			"m1", time.Now(), nil, "sender@example.com", nil, "Hello", "Preview", "stored",
			"<m1@example.com>", nil, nil, nil, "", "", "{}", "", "", "", "", []byte("{}"), 0.0,
		))
	mock.ExpectQuery("SELECT .* FROM \"attachments\"").WillReturnRows(sqlmock.NewRows([]string{
		"id", "filename", "mime", "size_bytes", "storage_path", "created_at",
	}))

	msg, err := p.ReadMessage(context.Background(), claims, "m1")
	if err != nil {
		t.Fatalf("ReadMessage() err = %v", err)
	}
	if msg.ID != "m1" || msg.Digest != nil {
		t.Fatalf("ReadMessage() = %+v, want message with nil digest", msg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSearchStructuredIncludesMessageWithoutDigest(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	aliasID := "a1"
	claims := model.TokenClaims{AliasID: &aliasID, Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	mock.ExpectQuery("LEFT JOIN message_digest md").WillReturnRows(unsummarizedListRows())

	items, err := p.SearchStructured(context.Background(), claims, model.SearchRequest{
		Intent: model.SearchIntent{Text: "hello"},
		Mode:   model.SearchModeKeyword,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("SearchStructured() err = %v", err)
	}
	if len(items) != 1 || items[0].MsgID != "m1" {
		t.Fatalf("SearchStructured() = %+v, want unsummarized message", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
