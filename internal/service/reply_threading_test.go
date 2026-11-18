package service

import (
	"context"
	"net/mail"
	"strings"
	"testing"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type replyThreadingStore struct {
	*fakeStore
	inReplyTo  *string
	references *string
	subject    *string
	draftInput store.CreateDraftInput
}

func (s *replyThreadingStore) GetMessageThreadMeta(context.Context, model.TokenClaims, string) (*string, *string, *string, error) {
	return s.inReplyTo, s.references, s.subject, nil
}

func (s *replyThreadingStore) CreateDraft(_ context.Context, _ model.TokenClaims, input store.CreateDraftInput) (model.Draft, error) {
	s.draftInput = input
	return model.Draft{}, nil
}

func TestReplyWithIdempotencyUsesReplyThreadMetadata(t *testing.T) {
	harness := &replyThreadingStore{
		fakeStore:  newFakeStore(),
		inReplyTo:  serviceStringPtr("<child@example.test>"),
		references: serviceStringPtr("<root@example.test> <child@example.test>"),
		subject:    serviceStringPtr("Status"),
	}
	svc := &Service{Store: harness, MailboxID: "m1"}
	aliasID := "a1"
	claims := model.TokenClaims{AliasID: &aliasID, Scopes: map[model.Scope]bool{model.ScopeSend: true}}

	outbound, err := svc.ReplyWithIdempotency(context.Background(), claims, "msg-1", "reply body", "reply-key")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(outbound.RawRFC822)))
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Header.Get("In-Reply-To"); got != "<child@example.test>" {
		t.Fatalf("In-Reply-To = %q", got)
	}
	if got := msg.Header.Get("References"); got != "<root@example.test> <child@example.test>" {
		t.Fatalf("References = %q", got)
	}
}

func TestCreateDraftUsesReplyThreadMetadata(t *testing.T) {
	harness := &replyThreadingStore{
		fakeStore:  newFakeStore(),
		inReplyTo:  serviceStringPtr("<child@example.test>"),
		references: serviceStringPtr("<root@example.test> <child@example.test>"),
		subject:    serviceStringPtr("Status"),
	}
	svc := &Service{Store: harness, MailboxID: "m1"}
	aliasID := "a1"
	sourceMessageID := "msg-1"
	claims := model.TokenClaims{AliasID: &aliasID, Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}

	if _, err := svc.CreateDraft(context.Background(), claims, store.CreateDraftInput{SourceMessageID: &sourceMessageID}); err != nil {
		t.Fatal(err)
	}
	if got := serviceStringValue(harness.draftInput.InReplyTo); got != "<child@example.test>" {
		t.Fatalf("draft In-Reply-To = %q", got)
	}
	if got := serviceStringValue(harness.draftInput.References); got != "<root@example.test> <child@example.test>" {
		t.Fatalf("draft References = %q", got)
	}
}

func serviceStringPtr(value string) *string { return &value }

func serviceStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
