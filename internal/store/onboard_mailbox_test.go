package store

import (
	"context"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestOnboardMailboxCommitsEncryptedMailboxAliasAndToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := NewPostgres(db)
	if err := p.ConfigureCredentialEncryption(testCredentialKey('o'), nil); err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mailboxes")).
		WithArgs(sqlmock.AnyArg(), "zoho", true, "imap.example.test", 993, true, "smtp.example.test", 465, true, "owner@example.test", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aliases")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "agent@example.test", true, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO tokens")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "agent", "hash", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := p.OnboardMailbox(context.Background(), OnboardMailboxInput{
		Name: " zoho ", Creds: MailboxCreds{
			IMAPHost: " imap.example.test ", IMAPPort: 993, IMAPTLS: true,
			SMTPHost: " smtp.example.test ", SMTPPort: 465, SMTPTLS: true,
			Username: " owner@example.test ", Password: "provider-secret",
		}, AliasEmail: " Agent@Example.Test ", TokenName: " agent ", TokenHash: "hash", Scopes: []string{"read", "send"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mailbox.Creds.Password != "provider-secret" || result.AliasID == "" || result.TokenID == "" {
		t.Fatalf("result=%+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardMailboxRollsBackWhenAliasOrTokenFails(t *testing.T) {
	for _, failAt := range []string{"alias", "token"} {
		t.Run(failAt, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			p := NewPostgres(db)
			if err := p.ConfigureCredentialEncryption(testCredentialKey('o'), nil); err != nil {
				t.Fatal(err)
			}
			mock.ExpectBegin()
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mailboxes")).WillReturnResult(sqlmock.NewResult(0, 1))
			if failAt == "alias" {
				mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aliases")).WillReturnError(context.Canceled)
			} else {
				mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aliases")).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(regexp.QuoteMeta("INSERT INTO tokens")).WillReturnError(context.Canceled)
			}
			mock.ExpectRollback()
			_, err = p.OnboardMailbox(context.Background(), OnboardMailboxInput{
				Name: "zoho", Creds: MailboxCreds{Password: "secret"}, AliasEmail: "agent@example.test", TokenName: "agent", TokenHash: "hash",
			})
			if err == nil {
				t.Fatal("expected onboarding failure")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
