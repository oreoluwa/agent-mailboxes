package api

import (
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/service"
)

func TestEventBrokerFiltersEveryEventBeforeAliasDelivery(t *testing.T) {
	broker := NewEventBroker()
	mailboxID := "mailbox-1"
	aliasA := "alias-a"
	aliasB := "alias-b"
	claimsA := model.TokenClaims{AliasID: &aliasA, MailboxID: &mailboxID, Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	claimsB := model.TokenClaims{AliasID: &aliasB, MailboxID: &mailboxID, Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	adminClaims := model.TokenClaims{MailboxID: &mailboxID, Scopes: map[model.Scope]bool{model.ScopeAdmin: true}}

	aEvents, cancelA := broker.Subscribe(mailboxID, "primary", claimsA)
	defer cancelA()
	bEvents, cancelB := broker.Subscribe(mailboxID, "primary", claimsB)
	defer cancelB()
	adminEvents, cancelAdmin := broker.Subscribe(mailboxID, "primary", adminClaims)
	defer cancelAdmin()

	broker.Publish(service.MailboxEvent{
		MailboxID:     mailboxID,
		MailboxName:   "primary",
		OwningAliasID: aliasB,
		EventType:     "draft_created",
		MessageID:     "draft-b",
	})

	assertNoMailboxEvent(t, aEvents)
	assertMailboxEvent(t, bEvents, "draft_created", "draft-b")
	assertMailboxEvent(t, adminEvents, "draft_created", "draft-b")

	broker.Publish(service.MailboxEvent{
		MailboxID:   mailboxID,
		MailboxName: "primary",
		Audience:    []string{aliasA},
		EventType:   "message_added",
		MessageID:   "message-a",
	})

	assertMailboxEvent(t, aEvents, "message_added", "message-a")
	assertNoMailboxEvent(t, bEvents)
	assertMailboxEvent(t, adminEvents, "message_added", "message-a")

	// An unscoped event is admin-only, and an alias-bound admin token remains
	// constrained to its alias.
	aliasAdminEvents, cancelAliasAdmin := broker.Subscribe(mailboxID, "primary", model.TokenClaims{
		AliasID:   &aliasA,
		MailboxID: &mailboxID,
		Scopes:    map[model.Scope]bool{model.ScopeRead: true, model.ScopeAdmin: true},
	})
	defer cancelAliasAdmin()
	broker.Publish(service.MailboxEvent{MailboxID: mailboxID, MailboxName: "primary", EventType: "admin_changed"})
	assertNoMailboxEvent(t, aEvents)
	assertNoMailboxEvent(t, bEvents)
	assertNoMailboxEvent(t, aliasAdminEvents)
	assertMailboxEvent(t, adminEvents, "admin_changed", "")

	otherMailboxID := "mailbox-2"
	wrongMailboxAdmin, cancelWrongMailbox := broker.Subscribe(mailboxID, "primary", model.TokenClaims{
		MailboxID: &otherMailboxID,
		Scopes:    map[model.Scope]bool{model.ScopeAdmin: true},
	})
	defer cancelWrongMailbox()
	broker.Publish(service.MailboxEvent{MailboxID: mailboxID, MailboxName: "primary", EventType: "admin_changed"})
	assertNoMailboxEvent(t, wrongMailboxAdmin)
}

func assertMailboxEvent(t *testing.T, events <-chan service.MailboxEvent, eventType, messageID string) {
	t.Helper()
	select {
	case event := <-events:
		if event.EventType != eventType || event.MessageID != messageID {
			t.Fatalf("unexpected mailbox event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s event", eventType)
	}
}

func assertNoMailboxEvent(t *testing.T, events <-chan service.MailboxEvent) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("received unauthorized mailbox event: %+v", event)
	default:
	}
}
