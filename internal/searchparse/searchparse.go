package searchparse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

type Parser interface {
	Parse(ctx context.Context, input string) (model.SearchRequest, error)
}

type OpenAICompat struct {
	BaseURL   string
	ModelName string
	APIKey    string
	Client    *http.Client
}

type requestEnvelope struct {
	Model          string                 `json:"model"`
	Messages       []chatMessage          `json:"messages"`
	Temperature    float64                `json:"temperature"`
	ResponseFormat map[string]interface{} `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseEnvelope struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func NewOpenAICompat(baseURL, model, apiKey string, timeout time.Duration) *OpenAICompat {
	baseURL = strings.TrimRight(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &OpenAICompat{
		BaseURL:   baseURL,
		ModelName: strings.TrimSpace(model),
		APIKey:    strings.TrimSpace(apiKey),
		Client:    &http.Client{Timeout: timeout},
	}
}

func (o *OpenAICompat) Parse(ctx context.Context, input string) (model.SearchRequest, error) {
	if strings.TrimSpace(o.ModelName) == "" {
		return model.SearchRequest{}, fmt.Errorf("search parser model is not configured")
	}
	now := time.Now()
	payload, _ := json.Marshal(requestEnvelope{
		Model: o.ModelName,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "system", Content: parserTimeContext(now)},
			{Role: "user", Content: "Query: " + input},
		},
		Temperature: 0,
		ResponseFormat: map[string]interface{}{
			"type": "json_object",
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return model.SearchRequest{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return model.SearchRequest{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return model.SearchRequest{}, fmt.Errorf("search parser status %d: %s", resp.StatusCode, string(body))
	}
	var out responseEnvelope
	if err := json.Unmarshal(body, &out); err != nil {
		return model.SearchRequest{}, fmt.Errorf("decode parser response: %w", err)
	}
	if len(out.Choices) == 0 {
		return model.SearchRequest{}, fmt.Errorf("search parser response missing choices")
	}
	raw := extractJSON(out.Choices[0].Message.Content)
	parsed, err := ParseSearchRequestJSON(raw)
	if err != nil {
		return model.SearchRequest{}, err
	}
	return parsed, nil
}

func ParseSearchRequestJSON(raw string) (model.SearchRequest, error) {
	raw = strings.TrimSpace(raw)
	var req model.SearchRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		var generic map[string]any
		if err2 := json.Unmarshal([]byte(raw), &generic); err2 != nil {
			return model.SearchRequest{}, fmt.Errorf("parse search request json: %w", err)
		}
		normalized, err2 := normalizeSearchRequestMap(generic)
		if err2 != nil {
			return model.SearchRequest{}, fmt.Errorf("parse search request json: %w", err)
		}
		return normalized, nil
	}
	return req, nil
}

func normalizeSearchRequestMap(m map[string]any) (model.SearchRequest, error) {
	var req model.SearchRequest
	req.Folder = stringValue(m["folder"])
	req.Limit = intValue(m["limit"])
	req.Mode = model.SearchMode(strings.TrimSpace(stringValue(m["mode"])))
	req.ParserMode = strings.TrimSpace(stringValue(m["parser_mode"]))
	if filters, ok := m["filters"].(map[string]any); ok {
		req.Filters = normalizeSearchFilters(filters)
	}
	if intent, ok := m["intent"].(map[string]any); ok {
		req.Intent = normalizeSearchIntent(intent)
	}
	return req, nil
}

func normalizeSearchFilters(m map[string]any) model.SearchFilters {
	var out model.SearchFilters
	if headers, ok := m["headers"].(map[string]any); ok {
		out.Headers.Subject = stringSliceValue(headers["subject"])
		out.Headers.From = stringSliceValue(headers["from"])
		out.Headers.To = stringSliceValue(headers["to"])
		out.Headers.MessageID = stringSliceValue(headers["message_id"])
		if generic, ok := headers["generic"].(map[string]any); ok {
			out.Headers.Generic = map[string][]string{}
			keys := make([]string, 0, len(generic))
			for key := range generic {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				values := stringSliceValue(generic[key])
				if len(values) > 0 {
					out.Headers.Generic[strings.ToLower(strings.TrimSpace(key))] = values
				}
			}
		}
	}
	if flags, ok := m["flags"].(map[string]any); ok {
		out.Flags.With = stringSliceValue(flags["with"])
		out.Flags.Without = stringSliceValue(flags["without"])
	}
	if date, ok := m["date"].(map[string]any); ok {
		out.Date.InternalSince = timePtrValue(date["internal_since"])
		out.Date.InternalBefore = timePtrValue(date["internal_before"])
		out.Date.SentSince = timePtrValue(date["sent_since"])
		out.Date.SentBefore = timePtrValue(date["sent_before"])
		out.Date.Field = strings.TrimSpace(stringValue(date["field"]))
		if rng, ok := date["range"].(map[string]any); ok {
			out.Date.Range = normalizeSearchDateRange(rng)
		}
		if temporal, ok := date["temporal"].(map[string]any); ok {
			out.Date.Temporal = normalizeSearchTemporalFilter(temporal)
		}
	}
	if size, ok := m["size"].(map[string]any); ok {
		out.Size.Larger = int64PtrValue(size["larger"])
		out.Size.Smaller = int64PtrValue(size["smaller"])
	}
	if logic, ok := m["logic"].(map[string]any); ok {
		out.Logic.Not = normalizeSearchSubqueries(logic["not"])
		out.Logic.Or = normalizeSearchOrClauses(logic["or"])
	}
	return out
}

func normalizeSearchTemporalFilter(m map[string]any) *model.SearchTemporalFilter {
	out := &model.SearchTemporalFilter{
		Kind:     strings.TrimSpace(stringValue(m["kind"])),
		Value:    strings.TrimSpace(stringValue(m["value"])),
		Amount:   intValue(m["amount"]),
		Unit:     strings.TrimSpace(stringValue(m["unit"])),
		Field:    strings.TrimSpace(stringValue(m["field"])),
		Timezone: strings.TrimSpace(stringValue(m["timezone"])),
		Date:     strings.TrimSpace(stringValue(m["date"])),
	}
	if out.Kind == "" && out.Value == "" && out.Amount == 0 && out.Unit == "" && out.Field == "" && out.Timezone == "" && out.Date == "" {
		return nil
	}
	return out
}

func normalizeSearchDateRange(m map[string]any) *model.SearchDateRange {
	out := &model.SearchDateRange{
		EndInclusive: boolValue(m["end_inclusive"]),
	}
	if start, ok := m["start"].(map[string]any); ok {
		out.Start = normalizeSearchTemporalPoint(start)
	}
	if end, ok := m["end"].(map[string]any); ok {
		out.End = normalizeSearchTemporalPoint(end)
	}
	if out.Start == nil && out.End == nil && !out.EndInclusive {
		return nil
	}
	return out
}

func normalizeSearchTemporalPoint(m map[string]any) *model.SearchTemporalPoint {
	out := &model.SearchTemporalPoint{
		Type:      strings.TrimSpace(stringValue(m["type"])),
		Value:     strings.TrimSpace(stringValue(m["value"])),
		Timestamp: strings.TrimSpace(stringValue(m["timestamp"])),
		Date:      strings.TrimSpace(stringValue(m["date"])),
		Direction: strings.TrimSpace(stringValue(m["direction"])),
		Amount:    intValue(m["amount"]),
		Unit:      strings.TrimSpace(stringValue(m["unit"])),
		Weekday:   strings.TrimSpace(stringValue(m["weekday"])),
		Period:    strings.TrimSpace(stringValue(m["period"])),
		Boundary:  strings.TrimSpace(stringValue(m["boundary"])),
		Snap:      strings.TrimSpace(stringValue(m["snap"])),
		Timezone:  strings.TrimSpace(stringValue(m["timezone"])),
	}
	if out.Type == "" && out.Value == "" && out.Timestamp == "" && out.Date == "" && out.Direction == "" && out.Amount == 0 && out.Unit == "" && out.Weekday == "" && out.Period == "" && out.Boundary == "" && out.Snap == "" && out.Timezone == "" {
		return nil
	}
	return out
}

func boolValue(v any) bool {
	switch typed := v.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func normalizeSearchIntent(m map[string]any) model.SearchIntent {
	var out model.SearchIntent
	out.Text = strings.TrimSpace(stringValue(m["text"]))
	out.BodyTerms = stringSliceValue(m["body_terms"])
	if headerTerms, ok := m["header_terms"].(map[string]any); ok {
		out.HeaderTerms.Subject = stringSliceValue(headerTerms["subject"])
		out.HeaderTerms.From = stringSliceValue(headerTerms["from"])
		out.HeaderTerms.To = stringSliceValue(headerTerms["to"])
	}
	return out
}

func normalizeSearchSubqueries(v any) []model.SearchSubquery {
	items := anySlice(v)
	out := make([]model.SearchSubquery, 0, len(items))
	for _, item := range items {
		switch typed := item.(type) {
		case map[string]any:
			var sub model.SearchSubquery
			if filters, ok := typed["filters"].(map[string]any); ok {
				sub.Filters = normalizeSearchFilters(filters)
			}
			if intent, ok := typed["intent"].(map[string]any); ok {
				sub.Intent = normalizeSearchIntent(intent)
			} else if text := strings.TrimSpace(stringValue(typed["text"])); text != "" {
				sub.Intent.Text = text
			}
			out = append(out, sub)
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				out = append(out, model.SearchSubquery{Intent: model.SearchIntent{Text: trimmed}})
			}
		}
	}
	return out
}

func normalizeSearchOrClauses(v any) []model.SearchOrClause {
	items := anySlice(v)
	out := make([]model.SearchOrClause, 0, len(items))
	for _, item := range items {
		switch typed := item.(type) {
		case map[string]any:
			if anyVal, ok := typed["any"]; ok {
				if subs := normalizeSearchSubqueries(anyVal); len(subs) > 0 {
					out = append(out, model.SearchOrClause{Any: subs})
				}
				continue
			}
			if left, lok := typed["left"]; lok {
				if right, rok := typed["right"]; rok {
					pair := append(normalizeSearchSubqueries([]any{left}), normalizeSearchSubqueries([]any{right})...)
					if len(pair) > 0 {
						out = append(out, model.SearchOrClause{Any: pair})
					}
				}
				continue
			}
			if subs := normalizeSearchSubqueries([]any{typed}); len(subs) > 0 {
				out = append(out, model.SearchOrClause{Any: subs})
			}
		case []any:
			if subs := normalizeSearchSubqueries(typed); len(subs) > 0 {
				out = append(out, model.SearchOrClause{Any: subs})
			}
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				out = append(out, model.SearchOrClause{Any: []model.SearchSubquery{{Intent: model.SearchIntent{Text: trimmed}}}})
			}
		}
	}
	return out
}

func anySlice(v any) []any {
	if v == nil {
		return nil
	}
	switch typed := v.(type) {
	case []any:
		return typed
	case []map[string]any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, item)
		}
		return out
	default:
		return []any{typed}
	}
}

func stringSliceValue(v any) []string {
	items := anySlice(v)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s := strings.TrimSpace(stringValue(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func stringValue(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return fmt.Sprintf("%.0f", typed)
	default:
		return ""
	}
}

func intValue(v any) int {
	switch typed := v.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		n, _ := typed.Int64()
		return int(n)
	default:
		return 0
	}
}

func int64PtrValue(v any) *int64 {
	switch typed := v.(type) {
	case float64:
		n := int64(typed)
		return &n
	case int64:
		n := typed
		return &n
	case int:
		n := int64(typed)
		return &n
	case json.Number:
		n, err := typed.Int64()
		if err == nil {
			return &n
		}
	}
	return nil
}

func timePtrValue(v any) *time.Time {
	raw := strings.TrimSpace(stringValue(v))
	if raw == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			tt := t.UTC()
			return &tt
		}
	}
	return nil
}

func extractJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(strings.ToLower(raw), "json") {
			raw = strings.TrimSpace(raw[4:])
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

func parserTimeContext(now time.Time) string {
	local := now
	zoneName, zoneOffset := local.Zone()
	return fmt.Sprintf(
		"Current local time: %s\nCurrent UTC time: %s\nLocal timezone: %s (UTC%+03d:%02d)\nResolve relative temporal phrases like today, yesterday, last week, and since last 3 days against this current local time unless the query specifies another timezone.\nWhen you return RFC3339 timestamps, convert the local boundary times to UTC. Do not use UTC calendar days unless the query explicitly asks for UTC.\nExample: if current local time is 2026-03-03T19:38:20-05:00, then \"yesterday\" means the local day 2026-03-02, so return internal_since=2026-03-02T05:00:00Z and internal_before=2026-03-03T05:00:00Z.",
		local.Format(time.RFC3339),
		now.UTC().Format(time.RFC3339),
		zoneName,
		zoneOffset/3600,
		absInt(zoneOffset%3600)/60,
	)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

const systemPrompt = `You convert natural-language email search requests into JSON.
Return only valid JSON matching this Go-JSON shape:
{
  "folder": "INBOX|Sent|Junk|",
  "limit": 0,
  "filters": {
    "headers": {
      "subject": ["..."],
      "from": ["..."],
      "message_id": ["..."],
      "generic": { "header-name": ["..."] }
    },
    "flags": {
      "with": ["seen","answered"],
      "without": ["seen","answered"]
    },
    "date": {
      "field": "internal|sent|",
      "internal_since": "RFC3339 or omit",
      "internal_before": "RFC3339 or omit",
      "sent_since": "RFC3339 or omit",
      "sent_before": "RFC3339 or omit",
      "range": {
        "start": {
          "type": "absolute_timestamp|absolute_local_day|relative_day|relative_offset|relative_named_weekday|named_period_boundary|now",
          "value": "today|yesterday|this|last|next|",
          "timestamp": "RFC3339 exact instant",
          "date": "YYYY-MM-DD",
          "direction": "past|future|this|last|next",
          "amount": 0,
          "unit": "hour|day|week|month|year",
          "weekday": "monday|tuesday|...|sunday",
          "period": "day|week|month|quarter|year",
          "boundary": "start|end",
          "snap": "none|start_of_day|end_of_day",
          "timezone": "IANA timezone or omit"
        },
        "end": {
          "type": "absolute_timestamp|absolute_local_day|relative_day|relative_offset|relative_named_weekday|named_period_boundary|now",
          "value": "today|yesterday|this|last|next|",
          "timestamp": "RFC3339 exact instant",
          "date": "YYYY-MM-DD",
          "direction": "past|future|this|last|next",
          "amount": 0,
          "unit": "hour|day|week|month|year",
          "weekday": "monday|tuesday|...|sunday",
          "period": "day|week|month|quarter|year",
          "boundary": "start|end",
          "snap": "none|start_of_day|end_of_day",
          "timezone": "IANA timezone or omit"
        },
        "end_inclusive": false
      },
      "temporal": {
        "kind": "relative_day|relative_window|absolute_local_day",
        "value": "today|yesterday|",
        "amount": 0,
        "unit": "day|week|",
        "field": "internal|sent|",
        "timezone": "IANA timezone or omit",
        "date": "YYYY-MM-DD for absolute_local_day"
      }
    },
    "size": {
      "larger": 0,
      "smaller": 0
    },
    "logic": {
      "not": [],
      "or": []
    }
  },
  "intent": {
    "text": "...",
    "body_terms": ["..."],
    "header_terms": {
      "subject": ["..."],
      "from": ["..."]
    }
  },
  "mode": "hybrid"
}

Rules:
- Extract exact constraints into filters.
- Leave residual fuzzy meaning in intent.text.
- Resolve relative dates/times from the provided current time context.
- Prefer filters.date.range.start/end for temporal constraints so start and end share the same reusable endpoint schema.
- Use filters.date.range for single-day, relative, absolute-day, and bounded range queries whenever possible.
- The legacy filters.date.temporal field is allowed, but only as fallback if range is unnecessary.
- Use filters.date.internal_* / sent_* directly only when the user gave an explicit absolute timestamp/date range or when no temporal structure is needed.
- For relative day words like "today" and "yesterday", use endpoint type="relative_day".
- For expressions like "2 weeks ago", use endpoint type="relative_offset" with direction, amount, unit, and snap when needed.
- For explicit local dates that still need timezone-aware boundary resolution, use endpoint type="absolute_local_day" with date="YYYY-MM-DD".
- For "last Friday" or "next Monday", use endpoint type="relative_named_weekday".
- For "start of this month" or "end of last quarter", use endpoint type="named_period_boundary".
- Use endpoint timezone only when the query specifies one; otherwise omit it and let the caller-provided default timezone apply.
- Use only supported flags: seen, answered.
- If the query says "not responded to" or "unanswered", use without=["answered"].
- If the query says unseen/unread/not seen, use without=["seen"].
- Use folder only when clearly requested.
- Default mode to "hybrid".
- If you cannot infer a field, omit it.
- Do not invent facts.
- Return JSON only.`
