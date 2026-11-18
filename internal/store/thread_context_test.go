package store

import (
	"context"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func TestGetThreadContextForMessageFiltersEveryMemberByAlias(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	aliasID := "a1"
	claims := model.TokenClaims{AliasID: &aliasID, Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	mock.ExpectQuery("SELECT EXISTS").WithArgs("msg-1", aliasID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`(?s).*WHERE candidate_aud\.message_id=m\.id AND candidate_aud\.alias_id=\$4.*ORDER BY.*`).
		WithArgs("mailbox-1", "msg-1", "alias", aliasID).
		WillReturnRows(threadContextRows().AddRow(
			"msg-1", "<one@example.test>", time.Now(), "sender@example.test", nil, "subject", "body", "summary",
		))

	got, err := p.GetThreadContextForMessage(context.Background(), claims, "mailbox-1", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].IsCurrent {
		t.Fatalf("unexpected thread context: %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetThreadContextForMessageRejectsInvisibleStartingMessage(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	aliasID := "a1"
	claims := model.TokenClaims{AliasID: &aliasID, Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	mock.ExpectQuery("SELECT EXISTS").WithArgs("msg-1", aliasID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	_, err = p.GetThreadContextForMessage(context.Background(), claims, "mailbox-1", "msg-1")
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetThreadContextForMessageAliasAdminStillUsesAliasScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	aliasID := "a1"
	claims := model.TokenClaims{
		AliasID: &aliasID,
		Scopes:  map[model.Scope]bool{model.ScopeRead: true, model.ScopeAdmin: true},
	}
	mock.ExpectQuery("SELECT EXISTS").WithArgs("msg-1", aliasID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`(?s).*WHERE candidate_aud\.message_id=m\.id AND candidate_aud\.alias_id=\$4.*ORDER BY.*`).
		WithArgs("mailbox-1", "msg-1", "alias", aliasID).
		WillReturnRows(threadContextRows().AddRow(
			"msg-1", "<one@example.test>", time.Now(), "sender@example.test", nil, "subject", "body", "summary",
		))

	if _, err := p.GetThreadContextForMessage(context.Background(), claims, "mailbox-1", "msg-1"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetThreadContextForMessageBackgroundRequiresSharedAudience(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	mailboxID := "mailbox-1"
	claims := model.TokenClaims{MailboxID: &mailboxID}
	mock.ExpectQuery(`(?s).*FROM message_audience source_aud.*candidate_aud\.alias_id=source_aud\.alias_id.*ORDER BY.*`).
		WithArgs(mailboxID, "msg-1", "shared", nil).
		WillReturnRows(threadContextRows().AddRow(
			"msg-1", "<one@example.test>", time.Now(), "sender@example.test", nil, "subject", "body", "summary",
		))

	got, err := p.GetThreadContextForMessage(context.Background(), claims, mailboxID, "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one shared message, got %d", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetThreadContextForMessageRejectsMailboxMismatch(t *testing.T) {
	p := &Postgres{}
	boundMailbox := "mailbox-1"
	claims := model.TokenClaims{MailboxID: &boundMailbox, Scopes: map[model.Scope]bool{model.ScopeAdmin: true}}

	_, err := p.GetThreadContextForMessage(context.Background(), claims, "mailbox-2", "msg-1")
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestGetThreadContextForMessageRejectsExternalMailboxOnlyClaims(t *testing.T) {
	p := &Postgres{}
	boundMailbox := "mailbox-1"
	claims := model.TokenClaims{TokenID: "token-1", MailboxID: &boundMailbox}

	_, err := p.GetThreadContextForMessage(context.Background(), claims, boundMailbox, "msg-1")
	if err != ErrUnauthorized {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func threadContextRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "message_id", "internal_date", "from_email", "from_name", "subject", "extracted_text", "summary_short",
	})
}
