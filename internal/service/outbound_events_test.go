package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type outboundEventStore struct {
	*fakeStore
	jobs       []store.OutboundMessage
	sentMarked bool
	deadMarked bool
}

func (s *outboundEventStore) ClaimOutboundMessages(context.Context, string, int, time.Duration) ([]store.OutboundMessage, error) {
	return append([]store.OutboundMessage(nil), s.jobs...), nil
}

func (s *outboundEventStore) MarkOutboundMessagesSent(_ context.Context, ids []string) error {
	s.sentMarked = len(ids) > 0
	return nil
}

func (s *outboundEventStore) MarkOutboundMessagesDead(_ context.Context, ids []string, _ string) error {
	s.deadMarked = len(ids) > 0
	return nil
}

type outboundEventSender struct{}

func (outboundEventSender) Send(string, []string, string, string, *string, *string) error {
	return errors.New("unexpected Send call")
}

func (outboundEventSender) SendRaw(_ string, _ []string, raw []byte) error {
	if strings.Contains(string(raw), "dead") {
		return errors.New("smtp 550 permanent rejection")
	}
	return nil
}

type committedEventRecorder struct {
	store  *outboundEventStore
	events []MailboxEvent
	t      *testing.T
}

func (r *committedEventRecorder) Publish(event MailboxEvent) {
	if event.EventType == "outbound_sent" && !r.store.sentMarked {
		r.t.Error("outbound_sent published before sent transition committed")
	}
	if event.EventType == "outbound_dead" && !r.store.deadMarked {
		r.t.Error("outbound_dead published before dead transition committed")
	}
	r.events = append(r.events, event)
}

func TestFlushOutboundQueuePublishesCommittedDeliveryTransitions(t *testing.T) {
	storeHarness := &outboundEventStore{
		fakeStore: newFakeStore(),
		jobs: []store.OutboundMessage{
			{ID: "sent-1", MailboxID: "mailbox-1", AliasID: "alias-a", RawRFC822: []byte("sent")},
			{ID: "dead-1", MailboxID: "mailbox-1", AliasID: "alias-b", RawRFC822: []byte("dead")},
		},
	}
	recorder := &committedEventRecorder{store: storeHarness, t: t}
	svc := &Service{
		Store:       storeHarness,
		Sender:      outboundEventSender{},
		MailboxID:   "mailbox-1",
		MailboxName: "primary",
		Events:      recorder,
	}

	if err := svc.flushOutboundQueue(context.Background(), 16); err != nil {
		t.Fatalf("flush outbound queue: %v", err)
	}
	if len(recorder.events) != 2 {
		t.Fatalf("delivery events = %d, want 2: %+v", len(recorder.events), recorder.events)
	}
	if got := recorder.events[0]; got.EventType != "outbound_sent" || got.MessageID != "sent-1" || got.OwningAliasID != "alias-a" {
		t.Fatalf("unexpected sent event: %+v", got)
	}
	if got := recorder.events[1]; got.EventType != "outbound_dead" || got.MessageID != "dead-1" || got.OwningAliasID != "alias-b" {
		t.Fatalf("unexpected dead event: %+v", got)
	}
}
