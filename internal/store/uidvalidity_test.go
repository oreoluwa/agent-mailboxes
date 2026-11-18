package store

import (
	"context"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func TestInsertMessageDeduplicatesWithinUIDValidityEpoch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	parsed := model.ParsedEmail{
		IMAPUID:         7,
		IMAPUIDValidity: 42,
		InternalDate:    time.Now().UTC(),
		FromEmail:       "sender@example.com",
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (folder_id,uidvalidity,imap_uid) DO NOTHING")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text FROM messages WHERE folder_id=$1 AND uidvalidity=$2 AND imap_uid=$3")).
		WithArgs("folder-1", int64(42), int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("existing-message"))
	mock.ExpectCommit()

	id, inserted, err := p.InsertMessage(context.Background(), "mailbox-1", "folder-1", parsed, nil)
	if err != nil {
		t.Fatalf("InsertMessage() err = %v", err)
	}
	if inserted || id != "existing-message" {
		t.Fatalf("InsertMessage() = (%q, %t), want existing epoch message", id, inserted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
