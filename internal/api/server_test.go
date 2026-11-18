package api

import (
	"strings"
	"testing"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func TestSummarizeSearchRequestIncludesStructuredPayloadAndHits(t *testing.T) {
	req := model.SearchRequest{
		Folder: "inbox",
		Limit:  50,
		Mode:   model.SearchModeHybrid,
		Intent: model.SearchIntent{
			Text:      "invoice overdue",
			BodyTerms: []string{"chargeback"},
			HeaderTerms: model.SearchHeaderIntent{
				Subject: []string{"invoice"},
			},
		},
		Filters: model.SearchFilters{
			Headers: model.SearchHeaderFilters{
				Subject: []string{"invoice"},
				From:    []string{"billing@example.com"},
			},
		},
		ParserMode: "fallback",
	}

	got := summarizeSearchRequest(req, 0)

	for _, want := range []string{
		"mode=hybrid",
		"parser=fallback",
		"folder=inbox",
		"subject=invoice",
		"from=billing@example.com",
		`text="invoice overdue"`,
		"body=chargeback",
		"hits=0",
		`request={"folder":"inbox","limit":50,`,
		`"subject":["invoice"]`,
		`"from":["billing@example.com"]`,
		`"text":"invoice overdue"`,
		`"body_terms":["chargeback"]`,
		`"mode":"hybrid","parser_mode":"fallback"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
}
