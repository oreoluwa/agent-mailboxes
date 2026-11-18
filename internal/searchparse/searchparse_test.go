package searchparse

import (
	"strings"
	"testing"
	"time"
)

func TestParseSearchRequestJSONNormalizesLooseLogicShapes(t *testing.T) {
	raw := `{
		"mode":"hybrid",
		"filters":{
			"logic":{
				"not":["spam"],
				"or":[{"left":"contract","right":{"intent":{"text":"renewal"}}}]
			}
		},
		"intent":{"text":"invoice"}
	}`
	req, err := ParseSearchRequestJSON(raw)
	if err != nil {
		t.Fatalf("ParseSearchRequestJSON() error = %v", err)
	}
	if len(req.Filters.Logic.Not) != 1 || req.Filters.Logic.Not[0].Intent.Text != "spam" {
		t.Fatalf("not logic = %#v", req.Filters.Logic.Not)
	}
	if len(req.Filters.Logic.Or) != 1 || len(req.Filters.Logic.Or[0].Any) != 2 {
		t.Fatalf("or logic = %#v", req.Filters.Logic.Or)
	}
	if req.Filters.Logic.Or[0].Any[0].Intent.Text != "contract" {
		t.Fatalf("left or term = %#v", req.Filters.Logic.Or[0].Any[0])
	}
	if req.Filters.Logic.Or[0].Any[1].Intent.Text != "renewal" {
		t.Fatalf("right or term = %#v", req.Filters.Logic.Or[0].Any[1])
	}
}

func TestParseSearchRequestJSONPreservesToHeaders(t *testing.T) {
	raw := `{
		"filters":{
			"headers":{
				"from":["oreoluwa"],
				"to":["ops@example.com"]
			}
		},
		"intent":{
			"header_terms":{
				"from":["oreoluwa"],
				"to":["ops@example.com"]
			}
		}
	}`

	req, err := ParseSearchRequestJSON(raw)
	if err != nil {
		t.Fatalf("ParseSearchRequestJSON() error = %v", err)
	}
	if len(req.Filters.Headers.To) != 1 || req.Filters.Headers.To[0] != "ops@example.com" {
		t.Fatalf("to filters = %#v", req.Filters.Headers.To)
	}
	if len(req.Intent.HeaderTerms.To) != 1 || req.Intent.HeaderTerms.To[0] != "ops@example.com" {
		t.Fatalf("to intent = %#v", req.Intent.HeaderTerms.To)
	}
}

func TestParseSearchRequestJSONPreservesTemporalFilter(t *testing.T) {
	raw := `{
		"filters":{
			"date":{
				"temporal":{
					"kind":"relative_day",
					"value":"yesterday",
					"field":"internal",
					"timezone":"America/Indiana/Indianapolis"
				}
			}
		}
	}`
	req, err := ParseSearchRequestJSON(raw)
	if err != nil {
		t.Fatalf("ParseSearchRequestJSON() error = %v", err)
	}
	if req.Filters.Date.Temporal == nil {
		t.Fatalf("temporal filter missing")
	}
	if req.Filters.Date.Temporal.Kind != "relative_day" || req.Filters.Date.Temporal.Value != "yesterday" {
		t.Fatalf("temporal filter = %#v", req.Filters.Date.Temporal)
	}
	if req.Filters.Date.Temporal.Timezone != "America/Indiana/Indianapolis" {
		t.Fatalf("timezone = %q", req.Filters.Date.Temporal.Timezone)
	}
}

func TestParseSearchRequestJSONPreservesTemporalRange(t *testing.T) {
	raw := `{
		"filters":{
			"date":{
				"field":"internal",
				"range":{
					"start":{"type":"relative_offset","direction":"past","amount":2,"unit":"week","snap":"start_of_day"},
					"end":{"type":"relative_day","value":"yesterday"}
				}
			}
		}
	}`
	req, err := ParseSearchRequestJSON(raw)
	if err != nil {
		t.Fatalf("ParseSearchRequestJSON() error = %v", err)
	}
	if req.Filters.Date.Range == nil || req.Filters.Date.Range.Start == nil || req.Filters.Date.Range.End == nil {
		t.Fatalf("range = %#v", req.Filters.Date.Range)
	}
	if req.Filters.Date.Range.Start.Type != "relative_offset" || req.Filters.Date.Range.End.Type != "relative_day" {
		t.Fatalf("range points = %#v", req.Filters.Date.Range)
	}
	if req.Filters.Date.Field != "internal" {
		t.Fatalf("field = %q", req.Filters.Date.Field)
	}
}

func TestParserTimeContextIncludesCurrentTimeAndTimezone(t *testing.T) {
	loc := time.FixedZone("TEST", -5*3600)
	now := time.Date(2026, 3, 3, 19, 26, 6, 0, loc)
	got := parserTimeContext(now)
	for _, want := range []string{
		"2026-03-03T19:26:06-05:00",
		"2026-03-04T00:26:06Z",
		"TEST",
		"UTC-05:00",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("parserTimeContext() missing %q in %q", want, got)
		}
	}
}
