package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("POSTGRES_DSN", "postgres://example.invalid/db")
	t.Setenv("MASTER_KEY", "test-master-key")
	t.Setenv("INITIAL_SYNC_MAX_MESSAGES", "200")
	t.Setenv("MAILBOX_CREDENTIAL_KEY", "")
	t.Setenv("MAILBOX_CREDENTIAL_KEY_FILE", "")
	t.Setenv("MAILBOX_CREDENTIAL_KEY_OLDS", "")
}

func TestLoadDisablesBackgroundAI(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SUMMARY_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SummaryEnabled {
		t.Fatal("expected summary processing to be disabled")
	}
}

func TestLoadRejectsInitialSyncLimitBelowAllSentinel(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("INITIAL_SYNC_MAX_MESSAGES", "-2")

	if _, err := Load(); err == nil {
		t.Fatal("expected INITIAL_SYNC_MAX_MESSAGES below -1 to be rejected")
	}
}

func TestLoadMailboxCredentialKeyFromPrivateFileAndSyncFolders(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MAILBOX_CREDENTIAL_KEY", "")
	t.Setenv("SYNC_FOLDERS", " INBOX, Archive , inbox ")
	path := filepath.Join(t.TempDir(), "mailbox.key")
	if err := os.WriteFile(path, []byte("test-encryption-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILBOX_CREDENTIAL_KEY_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MailboxCredentialKey != "test-encryption-key" {
		t.Fatalf("credential key = %q", cfg.MailboxCredentialKey)
	}
	if !reflect.DeepEqual(cfg.SyncFolders, []string{"INBOX", "Archive", "inbox"}) {
		t.Fatalf("sync folders = %#v", cfg.SyncFolders)
	}
}

func TestLoadRejectsAmbiguousOrPublicMailboxCredentialKeyFile(t *testing.T) {
	setRequiredEnv(t)
	path := filepath.Join(t.TempDir(), "mailbox.key")
	if err := os.WriteFile(path, []byte("test-encryption-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILBOX_CREDENTIAL_KEY_FILE", path)

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("public key file error = %v", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILBOX_CREDENTIAL_KEY", "also-set")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "cannot both be set") {
		t.Fatalf("ambiguous key source error = %v", err)
	}
}
