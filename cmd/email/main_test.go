package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeFlagsAnywhereSearch(t *testing.T) {
	got := normalizeFlagsAnywhere([]string{"graff", "--limit", "2", "--json"}, map[string]bool{
		"--limit": true,
		"--json":  false,
	})
	want := []string{"--limit", "2", "--json", "graff"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args: got=%v want=%v", got, want)
	}
}

func writeAgentProfile(t *testing.T, edits func(map[string]any)) string {
	t.Helper()
	profile := map[string]any{
		"version": 1,
		"api_url": "http://127.0.0.1:8787",
		"mailbox": "pilot",
		"alias":   "agent@example.test",
		"token":   "profile-secret",
	}
	if edits != nil {
		edits(profile)
	}
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveCLIConfigExplicitProfileIsAuthoritative(t *testing.T) {
	path := writeAgentProfile(t, nil)
	env := map[string]string{
		"EMAIL_PROFILE": "/ignored/profile.json",
		"EMAIL_API":     "https://ignored.example",
		"EMAIL_TOKEN":   "ignored-secret",
	}
	config, err := resolveCLIConfig([]string{"--profile", path, "inbox", "list"}, func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.APIURL != "http://127.0.0.1:8787" || config.Mailbox != "pilot" || config.Token != "profile-secret" {
		t.Fatalf("profile config = %#v", config)
	}
	if !reflect.DeepEqual(config.Args, []string{"inbox", "list"}) {
		t.Fatalf("command args = %v", config.Args)
	}
}

func TestResolveCLIConfigFromEnvironmentProfile(t *testing.T) {
	path := writeAgentProfile(t, nil)
	config, err := resolveCLIConfig([]string{"search", "hello"}, func(key string) string {
		if key == "EMAIL_PROFILE" {
			return path
		}
		return ""
	})
	if err != nil || config.Token != "profile-secret" || config.Mailbox != "pilot" {
		t.Fatalf("config=%#v err=%v", config, err)
	}
}

func TestResolveCLIConfigLegacyEnvironment(t *testing.T) {
	config, err := resolveCLIConfig([]string{"inbox", "list"}, func(key string) string {
		return map[string]string{"EMAIL_API": "https://mail.example.test", "EMAIL_TOKEN": "legacy-secret"}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.APIURL != "https://mail.example.test" || config.Token != "legacy-secret" || config.Mailbox != "" {
		t.Fatalf("legacy config = %#v", config)
	}
}

func TestLoadAgentProfileValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"http non-loopback", func(p map[string]any) { p["api_url"] = "http://mail.example.test" }, "literal loopback"},
		{"URL credentials", func(p map[string]any) { p["api_url"] = "https://user@mail.example.test" }, "origin URL"},
		{"URL path", func(p map[string]any) { p["api_url"] = "https://mail.example.test/api" }, "origin URL"},
		{"bad alias", func(p map[string]any) { p["alias"] = "Agent <agent@example.test>" }, "plain email address"},
		{"empty token", func(p map[string]any) { p["token"] = "" }, "token is required"},
		{"future version", func(p map[string]any) { p["version"] = 2 }, "unsupported profile version"},
		{"unknown field", func(p map[string]any) { p["extra"] = true }, "invalid JSON profile"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadAgentProfile(writeAgentProfile(t, test.edit))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "profile-secret") {
				t.Fatal("error exposed profile token")
			}
		})
	}
}

func TestLoadAgentProfileRejectsPublicPermissionsAndSymlink(t *testing.T) {
	path := writeAgentProfile(t, nil)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgentProfile(path); err == nil {
		t.Fatal("public profile permissions were accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "profile-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgentProfile(link); err == nil {
		t.Fatal("profile symlink was accepted")
	}
}

func TestNormalizeFlagsAnywhereReply(t *testing.T) {
	got := normalizeFlagsAnywhere([]string{"msg-1", "--body-file", "/tmp/reply.txt", "--json"}, map[string]bool{
		"--body-file": true,
		"--json":      false,
	})
	want := []string{"--body-file", "/tmp/reply.txt", "--json", "msg-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args: got=%v want=%v", got, want)
	}
}

func TestNormalizeFlagsAnywhereInboxList(t *testing.T) {
	got := normalizeFlagsAnywhere([]string{"--unread", "--limit", "5"}, map[string]bool{
		"--unread": false,
		"--limit":  true,
		"--json":   false,
	})
	want := []string{"--unread", "--limit", "5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args: got=%v want=%v", got, want)
	}
}
