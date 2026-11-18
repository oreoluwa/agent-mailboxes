package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func TestMailboxCredentialEncryptionAndMigration(t *testing.T) {
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
	schema := "mailbox_credentials_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	defer func() { _, _ = db.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) }()
	if _, err := db.ExecContext(ctx, `SET search_path TO `+schema+`, public`); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, db, integrationMigrationsDir(t)); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	key := testCredentialKey('k')
	p := NewPostgres(db)
	if err := p.ConfigureCredentialEncryption(key, nil); err != nil {
		t.Fatal(err)
	}
	created, err := p.CreateMailbox(ctx, "zoho-test", MailboxCreds{
		IMAPHost: "imap.zoho.test", IMAPPort: 993, IMAPTLS: true,
		SMTPHost: "smtp.zoho.test", SMTPPort: 465, SMTPTLS: true,
		Username: "agent@example.test", Password: "new-write-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Creds.Password != "new-write-secret" {
		t.Fatal("created mailbox did not return decrypted credential")
	}
	var stored string
	if err := db.QueryRowContext(ctx, `SELECT password_encrypted FROM mailboxes WHERE id=$1`, created.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, mailboxCredentialEnvelopePrefix) || strings.Contains(stored, "new-write-secret") {
		t.Fatalf("new mailbox credential is not encrypted at rest: %q", stored)
	}

	restarted := NewPostgres(db)
	if err := restarted.ConfigureCredentialEncryption(key, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.GetMailboxByName(ctx, "zoho-test")
	if err != nil || loaded.Creds.Password != "new-write-secret" {
		t.Fatalf("restart decrypt password=%q err=%v", loaded.Creds.Password, err)
	}

	const legacyID = "10000000-0000-0000-0000-000000000001"
	if _, err := db.ExecContext(ctx, `INSERT INTO mailboxes
(id,name,enabled,imap_host,imap_port,imap_tls,smtp_host,smtp_port,smtp_tls,username,password_encrypted,created_at)
VALUES ($1,'legacy',true,'imap.invalid',993,true,'smtp.invalid',465,true,'legacy@example.test','legacy-secret',now())`, legacyID); err != nil {
		t.Fatal(err)
	}
	updated, err := restarted.EncryptLegacyMailboxCredentials(ctx)
	if err != nil || updated != 1 {
		t.Fatalf("legacy migration updated=%d err=%v", updated, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT password_encrypted FROM mailboxes WHERE id=$1`, legacyID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, mailboxCredentialEnvelopePrefix) || strings.Contains(stored, "legacy-secret") {
		t.Fatalf("legacy credential is not encrypted at rest: %q", stored)
	}
	loaded, err = restarted.GetMailboxByName(ctx, "legacy")
	if err != nil || loaded.Creds.Password != "legacy-secret" {
		t.Fatalf("legacy decrypt password=%q err=%v", loaded.Creds.Password, err)
	}

	const rollbackLegacyID = "20000000-0000-0000-0000-000000000001"
	const brokenID = "30000000-0000-0000-0000-000000000001"
	if _, err := db.ExecContext(ctx, `INSERT INTO mailboxes
(id,name,enabled,imap_host,imap_port,imap_tls,smtp_host,smtp_port,smtp_tls,username,password_encrypted,created_at)
VALUES ($1,'rollback-legacy',true,'imap.invalid',993,true,'smtp.invalid',465,true,'rollback@example.test','rollback-secret',now()),
       ($2,'broken',true,'imap.invalid',993,true,'smtp.invalid',465,true,'broken@example.test',$3,now())`,
		rollbackLegacyID, brokenID, "enc:v1:legacy-prefix-collision"); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.EncryptLegacyMailboxCredentials(ctx); err == nil {
		t.Fatal("invalid encrypted record must roll back migration")
	}
	if err := db.QueryRowContext(ctx, `SELECT password_encrypted FROM mailboxes WHERE id=$1`, rollbackLegacyID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "rollback-secret" {
		t.Fatal("transaction did not roll back the preceding legacy update")
	}
	updated, err = restarted.EncryptLegacyMailboxCredentialsWithPlaintextOverrides(ctx, []string{brokenID})
	if err != nil || updated != 2 {
		t.Fatalf("prefix override migration updated=%d err=%v", updated, err)
	}
	loaded, err = restarted.GetMailboxByName(ctx, "broken")
	if err != nil || loaded.Creds.Password != "enc:v1:legacy-prefix-collision" {
		t.Fatalf("prefix collision decrypt password=%q err=%v", loaded.Creds.Password, err)
	}
}

func TestAtomicMailboxOnboarding(t *testing.T) {
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
	schema := "atomic_onboarding_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) }()
	if _, err := db.ExecContext(ctx, `SET search_path TO `+schema+`, public`); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, db, integrationMigrationsDir(t)); err != nil {
		t.Fatal(err)
	}
	p := NewPostgres(db)
	if err := p.ConfigureCredentialEncryption(testCredentialKey('n'), nil); err != nil {
		t.Fatal(err)
	}
	result, err := p.OnboardMailbox(ctx, OnboardMailboxInput{
		Name: "supported", Creds: MailboxCreds{
			IMAPHost: "imap.example.test", IMAPPort: 993, IMAPTLS: true,
			SMTPHost: "smtp.example.test", SMTPPort: 465, SMTPTLS: true,
			Username: "owner@example.test", Password: "provider-secret",
		}, AliasEmail: "agent@example.test", TokenName: "agent", TokenHash: "token-hash", Scopes: []string{"read", "send"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRowContext(ctx, `SELECT password_encrypted FROM mailboxes WHERE id=$1`, result.Mailbox.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, mailboxCredentialEnvelopePrefix) || strings.Contains(stored, "provider-secret") {
		t.Fatalf("credential was not encrypted: %q", stored)
	}
	claims, err := p.AuthenticateToken(ctx, "token-hash")
	if err != nil || claims.AliasID == nil || *claims.AliasID != result.AliasID || !claims.Scopes[model.ScopeRead] || !claims.Scopes[model.ScopeSend] {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	if _, err := p.OnboardMailbox(ctx, OnboardMailboxInput{
		Name: "supported", Creds: MailboxCreds{Password: "replacement-secret"}, AliasEmail: "other@example.test", TokenName: "other", TokenHash: "other-hash",
	}); err == nil {
		t.Fatal("duplicate mailbox onboarding unexpectedly succeeded")
	}
	var mailboxCount, aliasCount, tokenCount int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM mailboxes),(SELECT count(*) FROM aliases),(SELECT count(*) FROM tokens)`).Scan(&mailboxCount, &aliasCount, &tokenCount); err != nil {
		t.Fatal(err)
	}
	if mailboxCount != 1 || aliasCount != 1 || tokenCount != 1 {
		t.Fatalf("duplicate onboarding partially committed mailboxes=%d aliases=%d tokens=%d", mailboxCount, aliasCount, tokenCount)
	}

	provisioned, err := p.ProvisionMailboxAgent(ctx, ProvisionMailboxAgentInput{
		MailboxName: "supported", AliasEmail: "second@example.test", TokenName: "assistant",
		TokenHash: "first-second-hash", Scopes: []string{"read", "send"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ProvisionMailboxAgent(ctx, ProvisionMailboxAgentInput{
		MailboxName: "supported", AliasEmail: "second@example.test", TokenName: "assistant",
		TokenHash: "unexpected-hash", Scopes: []string{"read", "send"},
	}); !errors.Is(err, ErrAgentCredentialExists) {
		t.Fatalf("duplicate named credential error=%v", err)
	}
	unrelatedID, err := p.MintToken(ctx, &provisioned.AliasID, "other-purpose", "unrelated-hash", []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ProvisionMailboxAgent(ctx, ProvisionMailboxAgentInput{
		MailboxName: "supported", AliasEmail: "second@example.test", TokenName: "assistant",
		TokenHash: "unrelated-hash", Scopes: []string{"read", "send"}, Replace: true,
	}); err == nil {
		t.Fatal("forced replacement insert collision unexpectedly succeeded")
	}
	if _, err := p.AuthenticateToken(ctx, "first-second-hash"); err != nil {
		t.Fatalf("failed replacement did not roll back old credential: %v", err)
	}
	replaced, err := p.ProvisionMailboxAgent(ctx, ProvisionMailboxAgentInput{
		MailboxName: "supported", AliasEmail: "second@example.test", TokenName: "assistant",
		TokenHash: "replacement-hash", Scopes: []string{"read", "send"}, Replace: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replaced.TokenID == provisioned.TokenID {
		t.Fatal("replacement did not mint a new token identity")
	}
	if _, err := p.AuthenticateToken(ctx, "first-second-hash"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old replaced token remains active: %v", err)
	}
	if _, err := p.AuthenticateToken(ctx, "replacement-hash"); err != nil {
		t.Fatalf("replacement token is not active: %v", err)
	}
	var unrelatedRevoked sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT revoked_at FROM tokens WHERE id=$1`, unrelatedID).Scan(&unrelatedRevoked); err != nil || unrelatedRevoked.Valid {
		t.Fatalf("unrelated named token changed revoked=%v err=%v", unrelatedRevoked.Valid, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE aliases SET enabled=FALSE WHERE id=$1`, provisioned.AliasID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ProvisionMailboxAgent(ctx, ProvisionMailboxAgentInput{
		MailboxName: "supported", AliasEmail: "second@example.test", TokenName: "third",
		TokenHash: "third-hash", Scopes: []string{"read", "send"},
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled alias provisioning error=%v", err)
	}
}
