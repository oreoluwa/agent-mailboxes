//go:build unix

package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadMailboxCredentialKeyFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.key")
	link := filepath.Join(dir, "mailbox.key")
	if err := os.WriteFile(target, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readMailboxCredentialKeyFile(link); err == nil || !strings.Contains(err.Error(), "without following symlinks") {
		t.Fatalf("symlink key file error = %v", err)
	}
}

func TestReadMailboxCredentialKeyFileRejectsDirectoryAndOversizedFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := readMailboxCredentialKeyFile(dir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory key file error = %v", err)
	}

	path := filepath.Join(dir, "oversized.key")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxMailboxCredentialKeyFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMailboxCredentialKeyFile(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized key file error = %v", err)
	}
}

func TestReadMailboxCredentialKeyFileRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox.key")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readMailboxCredentialKeyFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("FIFO key file error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO key file read blocked")
	}
}

func TestValidateMailboxCredentialKeyFileRejectsDifferentOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox.key")
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	differentUID := uint32(os.Geteuid()) + 1
	if err := validateMailboxCredentialKeyFile(info, differentUID); err == nil || !strings.Contains(err.Error(), "owned") {
		t.Fatalf("different-owner key file error = %v", err)
	}
}
