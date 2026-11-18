package store

import (
	"net/mail"
	"strings"
	"testing"
)

func TestSanitizeDraftRecipientsRejectsHeaderInjection(t *testing.T) {
	_, err := sanitizeDraftRecipients([]string{"victim@example.com\r\nBcc: hidden@example.com"})
	if err == nil {
		t.Fatal("expected invalid recipient header error")
	}
}

func TestBuildDraftRawMessageSanitizesHeaders(t *testing.T) {
	inReplyTo := "<thread@test>\r\nX-Injected: no"
	references := "<root@test>\nBcc: hidden@example.com"
	raw := buildDraftRawMessage(
		"Sender <sender@example.com>\r\nX-Injected: nope",
		[]string{"Recipient <recipient@example.com>"},
		"Subject line\r\nBcc: hidden@example.com",
		"Body",
		&inReplyTo,
		&references,
	)

	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if got := msg.Header.Get("From"); got != "Sender <sender@example.com> X-Injected: nope" {
		t.Fatalf("unexpected From header: %q", got)
	}
	if got := msg.Header.Get("To"); got != "recipient@example.com" {
		t.Fatalf("unexpected To header: %q", got)
	}
	if got := msg.Header.Get("Subject"); got != "Subject line Bcc: hidden@example.com" {
		t.Fatalf("unexpected Subject header: %q", got)
	}
	if got := msg.Header.Get("In-Reply-To"); got != "<thread@test> X-Injected: no" {
		t.Fatalf("unexpected In-Reply-To header: %q", got)
	}
	if got := msg.Header.Get("References"); got != "<root@test> Bcc: hidden@example.com" {
		t.Fatalf("unexpected References header: %q", got)
	}
	if got := msg.Header.Get("X-Injected"); got != "" {
		t.Fatalf("unexpected injected header: %q", got)
	}
}

func TestNormalizeDraftListLimit(t *testing.T) {
	for _, test := range []struct {
		name  string
		input int
		want  int
	}{
		{name: "default", input: 0, want: 100},
		{name: "negative defaults", input: -1, want: 100},
		{name: "requested", input: 17, want: 17},
		{name: "capped", input: 201, want: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeDraftListLimit(test.input); got != test.want {
				t.Fatalf("normalizeDraftListLimit(%d) = %d, want %d", test.input, got, test.want)
			}
		})
	}
}
