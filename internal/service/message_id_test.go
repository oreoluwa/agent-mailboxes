package service

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

func TestQueuedMessagesHaveStableMessageIDs(t *testing.T) {
	alias := "alias-1"
	svc := &Service{Store: &fakeStore{}, MailboxID: "mailbox-1"}
	claims := model.TokenClaims{AliasID: &alias}
	parent, refs := "<parent@example.test>", "<root@example.test> <parent@example.test>"
	raw := buildSimpleRawMessage("assistant@example.test", []string{"researcher@example.test"}, "Reply", "synthetic body", &parent, &refs)
	job, err := svc.enqueueOutbound(context.Background(), claims, "assistant@example.test", []string{"researcher@example.test"}, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(job.RawRFC822)))
	if err != nil {
		t.Fatal(err)
	}
	id := msg.Header.Get("Message-ID")
	if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@agent-mailboxes.invalid>") {
		t.Fatalf("invalid Message-ID %q", id)
	}
	if _, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(id, "<"), "@agent-mailboxes.invalid>")); err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("In-Reply-To") != parent || msg.Header.Get("References") != refs {
		t.Fatal("reply headers changed")
	}
	wantKey := makeIdempotencyKey("mailbox-1", alias, "assistant@example.test", []string{"researcher@example.test"}, raw, "")
	if job.IdempotencyKey != wantKey {
		t.Fatal("Message-ID generation changed existing idempotency key")
	}
	second, err := svc.enqueueOutbound(context.Background(), claims, "assistant@example.test", []string{"researcher@example.test"}, raw, "another-request")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := mail.ReadMessage(strings.NewReader(string(second.RawRFC822)))
	if other.Header.Get("Message-ID") == id {
		t.Fatal("independent messages reused an ID")
	}
	// Reusing already composed bytes must preserve their identity exactly.
	retained, err := svc.enqueueOutbound(context.Background(), claims, "assistant@example.test", []string{"researcher@example.test"}, job.RawRFC822, "retained")
	if err != nil {
		t.Fatal(err)
	}
	if string(retained.RawRFC822) != string(job.RawRFC822) {
		t.Fatal("supplied Message-ID or message bytes changed")
	}
}

type retryMessageSender struct {
	outboundEventSender
	sent [][]byte
}

func (s *retryMessageSender) SendRaw(_ string, _ []string, raw []byte) error {
	s.sent = append(s.sent, append([]byte(nil), raw...))
	if len(s.sent) == 1 {
		return errors.New("temporary SMTP failure")
	}
	return nil
}
func TestOutboundRetryPreservesQueuedMessageID(t *testing.T) {
	alias := "alias-1"
	svc := &Service{Store: &fakeStore{}, MailboxID: "mailbox-1"}
	job, err := svc.enqueueOutbound(context.Background(), model.TokenClaims{AliasID: &alias}, "assistant@example.test", []string{"researcher@example.test"}, []byte("From: assistant@example.test\r\nTo: researcher@example.test\r\nSubject: retry\r\n\r\nbody"), "retry-test")
	if err != nil {
		t.Fatal(err)
	}
	sender := &retryMessageSender{}
	svc.Store = &outboundEventStore{fakeStore: newFakeStore(), jobs: []store.OutboundMessage{job}}
	svc.Sender = sender
	for i := 0; i < 2; i++ {
		if err := svc.flushOutboundQueue(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	if len(sender.sent) != 2 || string(sender.sent[0]) != string(job.RawRFC822) || string(sender.sent[1]) != string(job.RawRFC822) {
		t.Fatal("retry changed queued message bytes")
	}
}
