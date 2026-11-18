package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

type Summarizer interface {
	Summarize(ctx context.Context, input Input) (model.Digest, error)
}

type Input struct {
	Subject             string
	FromName            string
	From                string
	Text                string
	Attachments         []string
	AttachmentSummaries []AttachmentSummary
	AttachmentIssues    []string
	Thread              []model.ThreadContextMessage
}

type AttachmentSummary struct {
	Filename   string
	MIME       string
	Source     string
	Summary    string
	Confidence float64
}

type OpenAICompat struct {
	BaseURL   string
	ModelName string
	APIKey    string
	Client    *http.Client
}

type openAICompatReq struct {
	Model          string                 `json:"model"`
	Messages       []openAIChatMessage    `json:"messages"`
	Temperature    float64                `json:"temperature"`
	ResponseFormat map[string]interface{} `json:"response_format,omitempty"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAICompatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// NewOllama is kept for backward compatibility with existing wiring.
func NewOllama(host, model string) *OpenAICompat {
	return NewOpenAICompat(host, model, "")
}

func NewOpenAICompat(baseURL, model, apiKey string) *OpenAICompat {
	baseURL = strings.TrimRight(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	return &OpenAICompat{
		BaseURL:   baseURL,
		ModelName: model,
		APIKey:    apiKey,
		Client:    &http.Client{Timeout: 45 * time.Second},
	}
}

func (o *OpenAICompat) Summarize(ctx context.Context, input Input) (model.Digest, error) {
	prompt := buildPrompt(input)
	payload, _ := json.Marshal(openAICompatReq{
		Model: o.ModelName,
		Messages: []openAIChatMessage{
			{Role: "system", Content: "You summarize emails and must return only valid JSON matching the requested schema."},
			{Role: "user", Content: prompt},
		},
		Temperature: 0,
		ResponseFormat: map[string]interface{}{
			"type": "json_object",
		},
	})
	url := o.BaseURL + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return model.Digest{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return model.Digest{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return model.Digest{}, fmt.Errorf("openai-compatible status %d: %s", resp.StatusCode, string(body))
	}
	var or openAICompatResp
	if err := json.Unmarshal(body, &or); err != nil {
		return model.Digest{}, fmt.Errorf("decode openai-compatible envelope: %w", err)
	}
	if len(or.Choices) == 0 {
		return model.Digest{}, fmt.Errorf("openai-compatible response missing choices")
	}
	raw := extractJSON(or.Choices[0].Message.Content)
	d, err := ParseDigestJSON(raw)
	if err != nil {
		return model.Digest{}, err
	}
	d.ModelName = o.ModelName
	return d, nil
}

type digestPayload struct {
	SummaryShort      string          `json:"summary_short"`
	SummaryBullets    []string        `json:"summary_bullets"`
	Intent            string          `json:"intent"`
	Priority          string          `json:"priority"`
	RequiredAction    string          `json:"required_action"`
	SuggestedNextStep string          `json:"suggested_next_step"`
	ExtractedEntities json.RawMessage `json:"extracted_entities"`
	Confidence        float64         `json:"confidence"`
}

func ParseDigestJSON(raw string) (model.Digest, error) {
	raw = strings.TrimSpace(raw)
	var p digestPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return model.Digest{}, fmt.Errorf("parse digest json: %w", err)
	}
	if p.SummaryShort == "" {
		return model.Digest{}, fmt.Errorf("summary_short missing")
	}
	if len(p.SummaryBullets) == 0 {
		return model.Digest{}, fmt.Errorf("summary_bullets missing")
	}
	if p.Intent == "" {
		p.Intent = "other"
	}
	if p.Priority == "" {
		p.Priority = "med"
	}
	if p.RequiredAction == "" {
		p.RequiredAction = "unknown"
	}
	if p.SuggestedNextStep == "" {
		p.SuggestedNextStep = "Review the original email and determine the safest next action."
	}
	if p.Confidence < 0 {
		p.Confidence = 0
	}
	if p.Confidence > 1 {
		p.Confidence = 1
	}
	if len(p.ExtractedEntities) == 0 {
		p.ExtractedEntities = json.RawMessage(`{}`)
	}
	return model.Digest{
		SummaryShort:      p.SummaryShort,
		SummaryBullets:    p.SummaryBullets,
		Intent:            p.Intent,
		Priority:          p.Priority,
		RequiredAction:    p.RequiredAction,
		SuggestedNextStep: p.SuggestedNextStep,
		ExtractedEntities: p.ExtractedEntities,
		Confidence:        p.Confidence,
	}, nil
}

func extractJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(raw, "json") {
			raw = strings.TrimSpace(strings.TrimPrefix(raw, "json"))
		}
		if idx := strings.LastIndex(raw, "```"); idx >= 0 {
			raw = raw[:idx]
		}
		return strings.TrimSpace(raw)
	}
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			return strings.TrimSpace(raw[i : j+1])
		}
	}
	return raw
}

func buildPrompt(input Input) string {
	att := "none"
	if len(input.Attachments) > 0 {
		att = strings.Join(input.Attachments, ", ")
	}
	attachmentFindings := "none"
	if len(input.AttachmentSummaries) > 0 {
		lines := make([]string, 0, len(input.AttachmentSummaries))
		for _, item := range input.AttachmentSummaries {
			line := fmt.Sprintf("- filename=%s mime=%s source=%s confidence=%.2f summary=%s", defaultString(item.Filename, "(unknown)"), defaultString(item.MIME, "(unknown)"), defaultString(item.Source, "(unknown)"), item.Confidence, strings.TrimSpace(item.Summary))
			lines = append(lines, line)
		}
		attachmentFindings = strings.Join(lines, "\n")
	}
	attachmentIssues := "none"
	if len(input.AttachmentIssues) > 0 {
		attachmentIssues = strings.Join(input.AttachmentIssues, "\n")
	}
	thread := "none"
	if len(input.Thread) > 0 {
		lines := make([]string, 0, len(input.Thread))
		for _, tm := range input.Thread {
			if tm.IsCurrent {
				continue
			}
			subject := "(no subject)"
			if tm.Subject != nil && strings.TrimSpace(*tm.Subject) != "" {
				subject = strings.TrimSpace(*tm.Subject)
			}
			messageID := tm.RFCMessageID
			if strings.TrimSpace(messageID) == "" {
				messageID = "(missing)"
			}
			line := fmt.Sprintf("- [%s] from=%s subject=%s message_id=%s", tm.InternalDate.UTC().Format(time.RFC3339), formatSender(derefStr(tm.FromName), tm.FromEmail), subject, messageID)
			if tm.DigestSummary != nil && strings.TrimSpace(*tm.DigestSummary) != "" {
				line += "\n  digest_summary: " + strings.TrimSpace(*tm.DigestSummary)
			}
			if tm.ExtractedText != nil && strings.TrimSpace(*tm.ExtractedText) != "" {
				line += "\n  body: " + strings.TrimSpace(*tm.ExtractedText)
			}
			lines = append(lines, line)
		}
		if len(lines) > 0 {
			thread = strings.Join(lines, "\n")
		}
	}
	return fmt.Sprintf(`You summarize emails for operators. Use only facts from input. Do not invent details.
Return exactly one JSON object and no surrounding prose, markdown, or code fences.
Allowed top-level keys only:
- "summary_short": string, 1-3 short sentences
- "summary_bullets": array of 3-7 strings
- "intent": one of "invoice" | "support" | "scheduling" | "sales" | "legal" | "personal" | "other"
- "priority": one of "low" | "med" | "high" | "urgent"
- "required_action": one of "reply" | "forward" | "review" | "no_action" | "unknown"
- "suggested_next_step": string
- "extracted_entities": object with only these keys when present: "dates", "amounts", "invoice_numbers", "order_ids", "phones", "addresses", "links", "emails"
- "confidence": number from 0 to 1
If a field is unknown, still return the key and use the safest allowed fallback. Do not omit required keys. Do not add extra keys. Do not nest explanations outside the JSON object.
Use this exact JSON shape:
{
  "summary_short": "string",
  "summary_bullets": ["string", "string", "string"],
  "intent": "other",
  "priority": "med",
  "required_action": "review",
  "suggested_next_step": "string",
  "extracted_entities": {
    "dates": [],
    "amounts": [],
    "invoice_numbers": [],
    "order_ids": [],
    "phones": [],
    "addresses": [],
    "links": [],
    "emails": []
  },
  "confidence": 0.0
}
Use thread context to disambiguate intent and references, but prioritize the current email for required_action and suggested_next_step.
Use attachment findings to enrich summaries when available. If attachment findings conflict with body text, mention uncertainty explicitly and lower confidence.
Write summaries that explain what changed, what matters, and what should happen next.
Avoid low-signal phrasing and metadata such as mail client/device signatures (for example "Outlook for iOS"), transport details, or generic text like "Sender sent/replied".
Prefer concrete obligations, decisions, deadlines, asks, blockers, and deliverables.
If body text is missing or unclear, say that explicitly in summary_bullets and lower confidence.
If any attachment could not be analyzed, mention that limitation briefly in summary_bullets and lower confidence.
When referring to people, prefer explicit sender identity from this format: [Display Name](mailto:email) when both are available, otherwise use the email.
Do not mention signature/footer client lines in output (for example "Sent from Outlook for iOS").
Do not use generic actor words like "sender", "someone", or "the user" when a specific identity is available.

Email subject: %s
From: %s
Attachments: %s
Attachment findings:
%s
Attachment issues:
%s
Body:
%s

Earlier thread context (oldest to newest):
%s`, input.Subject, formatSender(input.FromName, input.From), att, attachmentFindings, attachmentIssues, input.Text, thread)
}

func formatSender(name, email string) string {
	email = strings.TrimSpace(email)
	name = strings.TrimSpace(name)
	if email == "" {
		if name == "" {
			return "(unknown sender)"
		}
		return name
	}
	if name == "" {
		return email
	}
	return fmt.Sprintf("[%s](mailto:%s)", name, email)
}

func derefStr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}
