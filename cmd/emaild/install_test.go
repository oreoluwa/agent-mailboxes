package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnvFileCandidates(t *testing.T) {
	t.Setenv("EMAILD_ENV_FILE", "")
	got := envFileCandidates()
	if got[len(got)-1] != ".env" {
		t.Fatalf("expected .env fallback last, got=%v", got)
	}
}

func TestExplicitEnvFileDoesNotFallThrough(t *testing.T) {
	t.Setenv("EMAILD_ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	if err := loadEnvAtStartup(); err == nil || !strings.Contains(err.Error(), "explicit env file") {
		t.Fatalf("loadEnvAtStartup error = %v", err)
	}
}

func TestEnvFileCandidatesDarwinDefault(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only expectation")
	}
	t.Setenv("EMAILD_ENV_FILE", "")
	got := envFileCandidates()
	if len(got) < 2 {
		t.Fatalf("expected default candidates, got=%v", got)
	}
	wantMid := filepath.Join(mustUserConfigDir(t), "emaild", ".env")
	if got[0] != wantMid {
		t.Fatalf("expected first candidate to be user config env, got=%v want=%s", got, wantMid)
	}
}

func TestBuildLaunchAgentPlistIncludesEnvVar(t *testing.T) {
	plist := buildLaunchAgentPlist("/tmp/emaild", "/tmp/work", "/tmp/.env", "/tmp/stdout.log", "/tmp/stderr.log")
	if !strings.Contains(plist, "<key>EMAILD_ENV_FILE</key>") {
		t.Fatalf("expected EMAILD_ENV_FILE key in plist")
	}
	if !strings.Contains(plist, "<string>/tmp/.env</string>") {
		t.Fatalf("expected env file value in plist")
	}
}

func TestStageEnvFileCopiesSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.env")
	dst := filepath.Join(dir, "installed.env")
	if err := os.WriteFile(src, []byte("MASTER_KEY=test\n"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := stageEnvFile(src, dst); err != nil {
		t.Fatalf("stage env file: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(b) != "MASTER_KEY=test\n" {
		t.Fatalf("unexpected content: %q", string(b))
	}
}

func mustUserConfigDir(t *testing.T) string {
	t.Helper()
	p, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("user config dir: %v", err)
	}
	return p
}
