package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestProvisionMailboxAgentCreatesAliasAndTokenAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := NewPostgres(db)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text FROM mailboxes WHERE name=$1 AND enabled=TRUE FOR UPDATE")).
		WithArgs("primary").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("mailbox-1"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text,enabled FROM aliases WHERE mailbox_id=$1 AND email=$2 FOR UPDATE")).
		WithArgs("mailbox-1", "agent@example.test").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aliases")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text FROM tokens WHERE alias_id=$1 AND name=$2 AND revoked_at IS NULL FOR UPDATE")).
		WithArgs(sqlmock.AnyArg(), "assistant").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO tokens")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	result, err := p.ProvisionMailboxAgent(context.Background(), ProvisionMailboxAgentInput{
		MailboxName: "primary", AliasEmail: "agent@example.test", TokenName: "assistant", TokenHash: "hash", Scopes: []string{"read", "send"},
	})
	if err != nil || result.MailboxID != "mailbox-1" || result.AliasID == "" || result.TokenID == "" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionMailboxAgentRejectsExistingNamedCredentialWithoutReplace(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := NewPostgres(db)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text FROM mailboxes WHERE name=$1 AND enabled=TRUE FOR UPDATE")).
		WithArgs("primary").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("mailbox-1"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text,enabled FROM aliases WHERE mailbox_id=$1 AND email=$2 FOR UPDATE")).
		WithArgs("mailbox-1", "agent@example.test").WillReturnRows(sqlmock.NewRows([]string{"id", "enabled"}).AddRow("alias-1", true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id::text FROM tokens WHERE alias_id=$1 AND name=$2 AND revoked_at IS NULL FOR UPDATE")).
		WithArgs("alias-1", "assistant").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("token-1"))
	mock.ExpectRollback()
	_, err = p.ProvisionMailboxAgent(context.Background(), ProvisionMailboxAgentInput{
		MailboxName: "primary", AliasEmail: "agent@example.test", TokenName: "assistant", TokenHash: "hash", Scopes: []string{"read", "send"},
	})
	if !errors.Is(err, ErrAgentCredentialExists) {
		t.Fatalf("unexpected error=%v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
