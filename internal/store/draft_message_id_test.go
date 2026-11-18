package store

import (
	"net/mail"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestDraftMessageIDAndReplyHeaders(t *testing.T) {
	parent, refs := "<parent@example.test>", "<root@example.test> <parent@example.test>"
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		raw := buildDraftRawMessage("assistant@example.test", []string{"researcher@example.test"}, "Re: hello", "body", &parent, &refs)
		m, err := mail.ReadMessage(strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		id := m.Header.Get("Message-ID")
		if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@agent-mailboxes.invalid>") {
			t.Fatalf("invalid Message-ID %q", id)
		}
		if _, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(id, "<"), "@agent-mailboxes.invalid>")); err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatal("independent drafts reused ID")
		}
		seen[id] = true
		if m.Header.Get("In-Reply-To") != parent || m.Header.Get("References") != refs {
			t.Fatal("reply headers changed")
		}
	}
}
