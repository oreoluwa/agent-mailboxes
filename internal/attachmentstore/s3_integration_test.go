package attachmentstore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestS3StorePut_MinIOIntegration(t *testing.T) {
	if os.Getenv("RUN_S3_INTEGRATION") != "1" {
		t.Skip("set RUN_S3_INTEGRATION=1 to run MinIO integration test")
	}

	cfg, err := integrationS3Config()
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewS3(cfg)
	if err != nil {
		t.Fatalf("new s3 store: %v", err)
	}

	key := fmt.Sprintf("integration/%d/probe.txt", time.Now().UnixNano())
	body := []byte("agent-emails minio integration test")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	storagePath, err := store.Put(ctx, key, body, "text/plain")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if wantPrefix := fmt.Sprintf("s3://%s/", cfg.Bucket); !strings.HasPrefix(storagePath, wantPrefix) {
		t.Fatalf("unexpected storage path %q, want prefix %q", storagePath, wantPrefix)
	}
}

// Real service tests are opt-in and never fall back to a developer account.
func integrationS3Config() (S3Config, error) {
	for _, key := range []string{"ATTACHMENT_S3_ENDPOINT", "ATTACHMENT_S3_BUCKET", "ATTACHMENT_S3_ACCESS_KEY", "ATTACHMENT_S3_SECRET_KEY"} {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			return S3Config{}, fmt.Errorf("%s is required for the opt-in S3 integration test", key)
		}
	}
	return S3Config{
		Endpoint:  os.Getenv("ATTACHMENT_S3_ENDPOINT"),
		Region:    getenv("ATTACHMENT_S3_REGION", "us-east-1"),
		Bucket:    os.Getenv("ATTACHMENT_S3_BUCKET"),
		AccessKey: os.Getenv("ATTACHMENT_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("ATTACHMENT_S3_SECRET_KEY"),
		Prefix:    getenv("ATTACHMENT_S3_PREFIX", "test"),
		PathStyle: getenvBool("ATTACHMENT_S3_PATH_STYLE", true),
	}, nil
}

func TestIntegrationS3ConfigRequiresExplicitCredentials(t *testing.T) {
	keys := []string{"ATTACHMENT_S3_ENDPOINT", "ATTACHMENT_S3_BUCKET", "ATTACHMENT_S3_ACCESS_KEY", "ATTACHMENT_S3_SECRET_KEY"}
	for _, key := range keys {
		t.Setenv(key, "")
	}
	if _, err := integrationS3Config(); err == nil {
		t.Fatal("missing configuration must not use defaults")
	}
	for _, key := range keys {
		t.Setenv(key, "synthetic-value")
	}
	t.Setenv("ATTACHMENT_S3_ENDPOINT", "http://127.0.0.1:9000")
	if _, err := integrationS3Config(); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		t.Setenv(key, "")
		if _, err := integrationS3Config(); err == nil {
			t.Fatalf("accepted missing %s", key)
		}
		t.Setenv(key, "synthetic-value")
	}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); strings.TrimSpace(v) != "" {
		return v
	}
	return d
}

func getenvBool(k string, d bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	case "":
		return d
	default:
		return d
	}
}
