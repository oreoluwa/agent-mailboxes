//go:build unix

package config

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

const maxMailboxCredentialKeyFileBytes = 4096

func readMailboxCredentialKeyFile(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open without following symlinks: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("open key file descriptor")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened key file: %w", err)
	}
	if err := validateMailboxCredentialKeyFile(info, uint32(os.Geteuid())); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxMailboxCredentialKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read opened key file: %w", err)
	}
	if len(data) > maxMailboxCredentialKeyFileBytes {
		return nil, fmt.Errorf("key file exceeds %d bytes", maxMailboxCredentialKeyFileBytes)
	}
	return data, nil
}

func validateMailboxCredentialKeyFile(info os.FileInfo, effectiveUID uint32) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("MAILBOX_CREDENTIAL_KEY_FILE must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("MAILBOX_CREDENTIAL_KEY_FILE permissions must not grant group or other access")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("MAILBOX_CREDENTIAL_KEY_FILE ownership is unavailable")
	}
	if stat.Uid != effectiveUID {
		return fmt.Errorf("MAILBOX_CREDENTIAL_KEY_FILE must be owned by the effective user")
	}
	return nil
}
