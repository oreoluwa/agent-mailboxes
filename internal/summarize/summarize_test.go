package summarize

import (
	"strings"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func TestParseDigestJSON(t *testing.T) {
	raw := `{"summary_short":"Customer asked for invoice update","summary_bullets":["Invoice #123 referenced","Amount $25"],"intent":"invoice","priority":"high","required_action":"reply","suggested_next_step":"Reply with corrected invoice","extracted_entities":{"invoice_numbers":["123"],"amounts":["$25"]},"confidence":0.91}`
	d, err := ParseDigestJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.SummaryShort == "" || len(d.SummaryBullets) == 0 || d.Intent == "" {
		t.Fatalf("unexpected digest: %+v", d)
	}
}

func TestBuildPromptIncludesThreadContext(t *testing.T) {
	p := buildPrompt(Input{
		Subject:  "Subject",
		FromName: "Sender Name",
		From:     "sender@example.com",
		Text:     "Current body",
		AttachmentSummaries: []AttachmentSummary{
			{
				Filename:   "invoice.pdf",
				MIME:       "application/pdf",
				Source:     "vision-model",
				Summary:    "Invoice states amount due is $420 by 2026-02-25.",
				Confidence: 0.92,
			},
		},
		AttachmentIssues: []string{
			`attachment "scan-2.pdf" vision analysis failed`,
		},
		Thread: []model.ThreadContextMessage{
			{
				MessageID:     "a",
				RFCMessageID:  "<a@test>",
				InternalDate:  time.Unix(100, 0).UTC(),
				FromEmail:     "sender@example.com",
				FromName:      ptr("Sender Name"),
				ExtractedText: ptr("Earlier thread body"),
				DigestSummary: ptr("Earlier summary"),
			},
			{
				MessageID:     "b",
				RFCMessageID:  "<b@test>",
				InternalDate:  time.Unix(200, 0).UTC(),
				FromEmail:     "sender@example.com",
				ExtractedText: ptr("Current body"),
				IsCurrent:     true,
			},
		},
	})
	if !strings.Contains(p, "Earlier thread context (oldest to newest):") {
		t.Fatalf("expected prompt to include thread context section")
	}
	if !strings.Contains(p, "Earlier thread body") {
		t.Fatalf("expected prompt to include prior message body")
	}
	if strings.Contains(p, "message_id=<b@test>") {
		t.Fatalf("expected current message to be excluded from prior thread section")
	}
	if !strings.Contains(p, "Avoid low-signal phrasing and metadata") {
		t.Fatalf("expected prompt to include low-signal metadata guardrail")
	}
	if !strings.Contains(p, `Return exactly one JSON object and no surrounding prose`) {
		t.Fatalf("expected prompt to require json-only output")
	}
	if !strings.Contains(p, `"summary_short": "string"`) {
		t.Fatalf("expected prompt to include explicit output shape example")
	}
	if !strings.Contains(p, `Do not add extra keys`) {
		t.Fatalf("expected prompt to forbid extra output keys")
	}
	if !strings.Contains(p, "Prefer concrete obligations, decisions, deadlines") {
		t.Fatalf("expected prompt to include actionable summary guidance")
	}
	if !strings.Contains(p, "Do not mention signature/footer client lines in output") {
		t.Fatalf("expected prompt to forbid signature/footer mentions")
	}
	if !strings.Contains(p, "Do not use generic actor words like \"sender\"") {
		t.Fatalf("expected prompt to discourage generic actor wording")
	}
	if !strings.Contains(p, "Attachment findings:") {
		t.Fatalf("expected prompt to include attachment findings section")
	}
	if !strings.Contains(p, "attachment \"scan-2.pdf\" vision analysis failed") {
		t.Fatalf("expected prompt to include attachment issue details")
	}
	if !strings.Contains(p, "Use attachment findings to enrich summaries") {
		t.Fatalf("expected prompt to include attachment guidance")
	}
	if !strings.Contains(p, "From: [Sender Name](mailto:sender@example.com)") {
		t.Fatalf("expected prompt to include formatted current sender identity")
	}
	if !strings.Contains(p, "from=[Sender Name](mailto:sender@example.com)") {
		t.Fatalf("expected prompt to include formatted thread sender identity")
	}
}

func ptr[T any](v T) *T { return &v }
