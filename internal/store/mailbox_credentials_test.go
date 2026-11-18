package store

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

type encryptedCredentialArgument struct {
	p         *Postgres
	mailboxID string
	want      string
}

func (a encryptedCredentialArgument) Match(value driver.Value) bool {
	envelope, ok := value.(string)
	if !ok {
		return false
	}
	plaintext, keyIndex, err := a.p.decryptMailboxCredential(a.mailboxID, envelope)
	return err == nil && keyIndex == 0 && plaintext == a.want
}

func testCredentialKey(fill byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{fill}), 32)))
}

func configuredCredentialStore(t *testing.T, active string, old ...string) *Postgres {
	t.Helper()
	p := &Postgres{}
	if err := p.ConfigureCredentialEncryption(active, old); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMailboxCredentialEncryptionRoundTripAndAAD(t *testing.T) {
	p := configuredCredentialStore(t, testCredentialKey('a'))
	envelope, err := p.encryptMailboxCredential("mailbox-a", "zoho-app-password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(envelope, mailboxCredentialEnvelopePrefix) || strings.Contains(envelope, "zoho-app-password") {
		t.Fatalf("credential was not safely enveloped: %q", envelope)
	}
	second, err := p.encryptMailboxCredential("mailbox-a", "zoho-app-password")
	if err != nil {
		t.Fatal(err)
	}
	if envelope == second {
		t.Fatal("credential envelopes must use random nonces")
	}
	plaintext, keyIndex, err := p.decryptMailboxCredential("mailbox-a", envelope)
	if err != nil || plaintext != "zoho-app-password" || keyIndex != 0 {
		t.Fatalf("decrypt = %q key=%d err=%v", plaintext, keyIndex, err)
	}
	if _, _, err := p.decryptMailboxCredential("mailbox-b", envelope); err == nil {
		t.Fatal("mailbox ID must be authenticated as associated data")
	}
	tampered := envelope[:len(envelope)-1] + map[bool]string{true: "A", false: "B"}[envelope[len(envelope)-1] != 'A']
	if _, _, err := p.decryptMailboxCredential("mailbox-a", tampered); err == nil {
		t.Fatal("tampered envelope must be rejected")
	}
}

func TestMailboxCredentialEncryptionRequiresConfiguredKeyAndSupportsRotation(t *testing.T) {
	p := &Postgres{}
	if _, err := p.encryptMailboxCredential("m1", "secret"); !errors.Is(err, ErrCredentialEncryptionNotConfigured) {
		t.Fatalf("encrypt without key = %v", err)
	}
	if _, _, err := p.decryptMailboxCredential("m1", mailboxCredentialEnvelopePrefix+"invalid"); !errors.Is(err, ErrCredentialEncryptionNotConfigured) {
		t.Fatalf("decrypt without key = %v", err)
	}
	if _, err := p.mailboxFromRow(mailboxRow{ID: "m1", PasswordEncrypted: "plaintext"}); err == nil {
		t.Fatal("normal reads must reject plaintext credentials")
	}
	if _, err := p.CreateMailbox(context.Background(), "missing-key", MailboxCreds{Password: "secret"}); !errors.Is(err, ErrCredentialEncryptionNotConfigured) {
		t.Fatalf("CreateMailbox without key = %v", err)
	}
	if err := p.ConfigureCredentialEncryption("not-base64", nil); err == nil {
		t.Fatal("invalid active key must be rejected")
	}

	oldKey := testCredentialKey('o')
	oldStore := configuredCredentialStore(t, oldKey)
	envelope, err := oldStore.encryptMailboxCredential("m1", "rotated-secret")
	if err != nil {
		t.Fatal(err)
	}
	rotated := configuredCredentialStore(t, testCredentialKey('n'), oldKey)
	plaintext, keyIndex, err := rotated.decryptMailboxCredential("m1", envelope)
	if err != nil || plaintext != "rotated-secret" || keyIndex != 1 {
		t.Fatalf("old-key decrypt = %q key=%d err=%v", plaintext, keyIndex, err)
	}
	wrong := configuredCredentialStore(t, testCredentialKey('x'))
	if _, _, err := wrong.decryptMailboxCredential("m1", envelope); err == nil {
		t.Fatal("wrong key must be rejected")
	}
	if _, _, err := rotated.decryptMailboxCredential("m1", "enc:v2:future"); err == nil {
		t.Fatal("unsupported envelope versions must be rejected")
	}
}

func TestMailboxCredsJSONOmitsPassword(t *testing.T) {
	encoded, err := json.Marshal(MailboxCreds{Username: "agent@example.test", Password: "never-json"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "never-json") || strings.Contains(strings.ToLower(string(encoded)), "password") {
		t.Fatalf("password leaked in JSON: %s", encoded)
	}
}

func TestEncryptLegacyMailboxCredentialsMigratesAndRotatesAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	oldKey := testCredentialKey('o')
	p := NewPostgres(db)
	if err := p.ConfigureCredentialEncryption(testCredentialKey('n'), []string{oldKey}); err != nil {
		t.Fatal(err)
	}
	oldStore := configuredCredentialStore(t, oldKey)
	oldEnvelope, err := oldStore.encryptMailboxCredential("old", "old-secret")
	if err != nil {
		t.Fatal(err)
	}
	currentEnvelope, err := p.encryptMailboxCredential("current", "current-secret")
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id::text,password_encrypted FROM mailboxes ORDER BY id FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "password_encrypted"}).
			AddRow("legacy", "legacy-secret").
			AddRow("old", oldEnvelope).
			AddRow("current", currentEnvelope))
	mock.ExpectExec(`UPDATE mailboxes SET password_encrypted=\$1 WHERE id=\$2`).
		WithArgs(encryptedCredentialArgument{p: p, mailboxID: "legacy", want: "legacy-secret"}, "legacy").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailboxes SET password_encrypted=\$1 WHERE id=\$2`).
		WithArgs(encryptedCredentialArgument{p: p, mailboxID: "old", want: "old-secret"}, "old").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	updated, err := p.EncryptLegacyMailboxCredentials(context.Background())
	if err != nil || updated != 2 {
		t.Fatalf("migration updated=%d err=%v", updated, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptLegacyMailboxCredentialsRollsBackOnInvalidEncryptedRecord(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := NewPostgres(db)
	if err := p.ConfigureCredentialEncryption(testCredentialKey('a'), nil); err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id::text,password_encrypted FROM mailboxes ORDER BY id FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "password_encrypted"}).AddRow("broken", "enc:v2:future"))
	mock.ExpectRollback()

	if _, err := p.EncryptLegacyMailboxCredentials(context.Background()); err == nil {
		t.Fatal("invalid encrypted record must abort migration")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptLegacyMailboxCredentialsAllowsExplicitPrefixCollisionOverrides(t *testing.T) {
	const legacyV1ID = "10000000-0000-0000-0000-000000000001"
	const legacyV2ID = "20000000-0000-0000-0000-000000000001"
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := NewPostgres(db)
	if err := p.ConfigureCredentialEncryption(testCredentialKey('a'), nil); err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id::text,password_encrypted FROM mailboxes ORDER BY id FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "password_encrypted"}).
			AddRow(legacyV1ID, "enc:v1:legacy-plaintext").
			AddRow(legacyV2ID, "enc:v2:legacy-plaintext"))
	mock.ExpectExec(`UPDATE mailboxes SET password_encrypted=\$1 WHERE id=\$2`).
		WithArgs(encryptedCredentialArgument{p: p, mailboxID: legacyV1ID, want: "enc:v1:legacy-plaintext"}, legacyV1ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailboxes SET password_encrypted=\$1 WHERE id=\$2`).
		WithArgs(encryptedCredentialArgument{p: p, mailboxID: legacyV2ID, want: "enc:v2:legacy-plaintext"}, legacyV2ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	updated, err := p.EncryptLegacyMailboxCredentialsWithPlaintextOverrides(
		context.Background(), []string{legacyV1ID, legacyV2ID},
	)
	if err != nil || updated != 2 {
		t.Fatalf("prefix collision migration updated=%d err=%v", updated, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptLegacyMailboxCredentialsRejectsUnsafeOverrides(t *testing.T) {
	const validID = "30000000-0000-0000-0000-000000000001"
	const missingID = "40000000-0000-0000-0000-000000000001"
	key := testCredentialKey('a')
	p := configuredCredentialStore(t, key)
	validEnvelope, err := p.encryptMailboxCredential(validID, "secret")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("already encrypted", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		configured := NewPostgres(db)
		if err := configured.ConfigureCredentialEncryption(key, nil); err != nil {
			t.Fatal(err)
		}
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT id::text,password_encrypted FROM mailboxes ORDER BY id FOR UPDATE`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "password_encrypted"}).AddRow(validID, validEnvelope))
		mock.ExpectRollback()
		if _, err := configured.EncryptLegacyMailboxCredentialsWithPlaintextOverrides(context.Background(), []string{validID}); err == nil {
			t.Fatal("valid encrypted credential must not be treated as plaintext")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown mailbox", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		configured := NewPostgres(db)
		if err := configured.ConfigureCredentialEncryption(key, nil); err != nil {
			t.Fatal(err)
		}
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT id::text,password_encrypted FROM mailboxes ORDER BY id FOR UPDATE`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "password_encrypted"}))
		mock.ExpectRollback()
		if _, err := configured.EncryptLegacyMailboxCredentialsWithPlaintextOverrides(context.Background(), []string{missingID}); err == nil {
			t.Fatal("unknown override must fail")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ordinary plaintext", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		configured := NewPostgres(db)
		if err := configured.ConfigureCredentialEncryption(key, nil); err != nil {
			t.Fatal(err)
		}
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT id::text,password_encrypted FROM mailboxes ORDER BY id FOR UPDATE`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "password_encrypted"}).AddRow(missingID, "ordinary-plaintext"))
		mock.ExpectRollback()
		if _, err := configured.EncryptLegacyMailboxCredentialsWithPlaintextOverrides(context.Background(), []string{missingID}); err == nil {
			t.Fatal("override must be limited to reserved-prefix collisions")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPlaintextOverrideCanonicalizesUUIDs(t *testing.T) {
	const canonical = "abcdefab-cdef-abcd-efab-cdefabcdefab"
	for _, input := range []string{strings.ToUpper(canonical), strings.ReplaceAll(canonical, "-", "")} {
		t.Run(input, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			p := NewPostgres(db)
			if err := p.ConfigureCredentialEncryption(testCredentialKey('a'), nil); err != nil {
				t.Fatal(err)
			}
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT id::text,password_encrypted FROM mailboxes ORDER BY id FOR UPDATE`).WillReturnRows(sqlmock.NewRows([]string{"id", "password_encrypted"}).AddRow(canonical, "enc:v2:literal"))
			mock.ExpectExec(`UPDATE mailboxes SET password_encrypted=\$1 WHERE id=\$2`).WithArgs(encryptedCredentialArgument{p: p, mailboxID: canonical, want: "enc:v2:literal"}, canonical).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			count, err := p.EncryptLegacyMailboxCredentialsWithPlaintextOverrides(context.Background(), []string{input})
			if err != nil || count != 1 {
				t.Fatalf("count=%d err=%v", count, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
	p := configuredCredentialStore(t, testCredentialKey('a'))
	if _, err := p.EncryptLegacyMailboxCredentialsWithPlaintextOverrides(context.Background(), []string{canonical, strings.ToUpper(canonical)}); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("expected canonical duplicate rejection, got %v", err)
	}
}
