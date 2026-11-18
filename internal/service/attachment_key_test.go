package service

import "testing"

func TestBuildAttachmentKeyUsesMessageIdentity(t *testing.T) {
	first := buildAttachmentKey("mailbox-1", "message-uuid-1", 0, "../same report.txt")
	second := buildAttachmentKey("mailbox-1", "message-uuid-2", 0, "../same report.txt")

	if first == second {
		t.Fatalf("attachment keys must differ across messages: %q", first)
	}
	if first != "mailbox-1/message-message-uuid-1/001-same_report.txt" {
		t.Fatalf("unexpected attachment key: %q", first)
	}
}
