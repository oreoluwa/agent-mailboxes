package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSetsValuesWithoutOverriding(t *testing.T) {
	t.Setenv("ENVFILE_B", "existing")

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "ENVFILE_A=hello\nENVFILE_B=from_file\nexport ENVFILE_C=\"quoted value\"\nENVFILE_D='single quoted'\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Load(path); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := os.Getenv("ENVFILE_A"); got != "hello" {
		t.Fatalf("ENVFILE_A = %q, want %q", got, "hello")
	}
	if got := os.Getenv("ENVFILE_B"); got != "existing" {
		t.Fatalf("ENVFILE_B = %q, want %q", got, "existing")
	}
	if got := os.Getenv("ENVFILE_C"); got != "quoted value" {
		t.Fatalf("ENVFILE_C = %q, want %q", got, "quoted value")
	}
	if got := os.Getenv("ENVFILE_D"); got != "single quoted" {
		t.Fatalf("ENVFILE_D = %q, want %q", got, "single quoted")
	}
}

func TestLoadMissingFileIsNoop(t *testing.T) {
	if err := Load(filepath.Join(t.TempDir(), ".env")); err != nil {
		t.Fatalf("Load should not fail for missing file: %v", err)
	}
}

func TestLoadParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("NOT_VALID"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Load(path); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}
