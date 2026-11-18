//go:build unix

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

func readPrivateAgentProfile(path string, maxBytes int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("cannot open profile securely")
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("cannot open profile securely")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("cannot inspect profile securely")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("profile must be a private regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("profile must be owned by the effective user")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, errors.New("cannot read profile")
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("profile exceeds %d bytes", maxBytes)
	}
	return data, nil
}
