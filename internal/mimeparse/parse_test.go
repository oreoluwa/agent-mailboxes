package mimeparse

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseRawEmailNestedMultipartAlternativeWithAttachment(t *testing.T) {
	raw := strings.Join([]string{
		"From: Sender <sender@example.com>",
		"To: x.ai@example.com",
		"Subject: Re: Picture",
		"Message-ID: <nested@test>",
		"Date: Mon, 2 Jan 2006 15:04:05 -0700",
		"Content-Type: multipart/mixed; boundary=\"mix\"",
		"",
		"--mix",
		"Content-Type: multipart/alternative; boundary=\"alt\"",
		"",
		"--alt",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Here's the picture. Wdyt?",
		"--alt",
		"Content-Type: text/html; charset=utf-8",
		"",
		"<p>Here's the picture. Wdyt?</p>",
		"--alt--",
		"--mix",
		"Content-Type: image/png; name=\"photo.png\"",
		"Content-Disposition: attachment; filename=\"photo.png\"",
		"Content-Transfer-Encoding: base64",
		"",
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8Xw8AAqMBgQW2KI4AAAAASUVORK5CYII=",
		"--mix--",
		"",
	}, "\r\n")

	parsed, err := ParseRawEmail([]byte(raw), 10, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ExtractedText != "Here's the picture. Wdyt?" {
		t.Fatalf("expected extracted body text, got %q", parsed.ExtractedText)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(parsed.Attachments))
	}
	if parsed.Attachments[0].Filename != "photo.png" {
		t.Fatalf("expected photo.png attachment, got %q", parsed.Attachments[0].Filename)
	}
}

func TestParseRawEmailCollectsOnlyParsedRecipientHeaders(t *testing.T) {
	raw := strings.Join([]string{
		"From: owner+sender@mail.example.test",
		"To: Owner <owner+alpha@mail.example.test>",
		"Cc: owner+beta@mail.example.test",
		"Bcc: owner+blind@mail.example.test",
		"Delivered-To: owner+delivered@mail.example.test",
		"Envelope-To: owner+envelope@mail.example.test",
		"X-Original-To: owner+forwarded@mail.example.test",
		"Reply-To: owner+reply@mail.example.test",
		"X-Note: owner+note@mail.example.test",
		"Subject: recipients",
		"",
		"body",
	}, "\r\n")

	parsed, err := ParseRawEmail([]byte(raw), 11, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(parsed.Recipients))
	for _, recipient := range parsed.Recipients {
		got = append(got, recipient.Email)
	}
	slices.Sort(got)
	want := []string{
		"owner+alpha@mail.example.test",
		"owner+beta@mail.example.test",
		"owner+blind@mail.example.test",
		"owner+delivered@mail.example.test",
		"owner+envelope@mail.example.test",
		"owner+forwarded@mail.example.test",
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected recipients: got %v want %v", got, want)
	}
}

func TestParseRawEmailRejectsMalformedRecipientHeader(t *testing.T) {
	raw := strings.Join([]string{
		"From: sender@example.com",
		"To: Agent <owner+alpha@mail.example.test>, malformed recipient",
		"Subject: malformed recipient",
		"",
		"body",
	}, "\r\n")

	parsed, err := ParseRawEmail([]byte(raw), 12, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Recipients) != 0 {
		t.Fatalf("expected malformed recipient header to be ignored, got %+v", parsed.Recipients)
	}
}

func TestParseRawEmailCollectsRepeatedDeliveryHeaders(t *testing.T) {
	raw := strings.Join([]string{
		"From: sender@example.com",
		"Delivered-To: owner+alpha@mail.example.test",
		"Delivered-To: owner+beta@mail.example.test",
		"Subject: repeated delivery headers",
		"",
		"body",
	}, "\r\n")

	parsed, err := ParseRawEmail([]byte(raw), 13, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(parsed.Recipients))
	for _, recipient := range parsed.Recipients {
		got = append(got, recipient.Email)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"owner+alpha@mail.example.test", "owner+beta@mail.example.test"}) {
		t.Fatalf("expected both delivery recipients, got %v", got)
	}
}
