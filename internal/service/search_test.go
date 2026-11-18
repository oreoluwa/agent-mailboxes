package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/embed"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

type stubSearchParser struct {
	req model.SearchRequest
	err error
}

func (s stubSearchParser) Parse(ctx context.Context, input string) (model.SearchRequest, error) {
	if s.err != nil {
		return model.SearchRequest{}, s.err
	}
	return s.req, nil
}

type stubEmbedder struct{}

func (stubEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, 0, len(inputs))
	for range inputs {
		out = append(out, []float32{0.1, 0.2, 0.3})
	}
	return out, nil
}

func (stubEmbedder) ModelName() string { return "stub" }

var _ embed.Embedder = stubEmbedder{}

func TestHasRecencyIntent(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{name: "empty", query: "", want: true},
		{name: "recent", query: "my recent emails", want: true},
		{name: "latest", query: "latest invoice", want: true},
		{name: "last", query: "last auth code", want: true},
		{name: "none", query: "invoice overdue", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasRecencyIntent(tt.query); got != tt.want {
				t.Fatalf("hasRecencyIntent(%q)=%v want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestIsRecencyOnlyQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{name: "empty", query: "", want: true},
		{name: "my recent emails", query: "my recent emails", want: true},
		{name: "recent inbox messages", query: "recent inbox messages", want: true},
		{name: "recent google code", query: "recent google code", want: false},
		{name: "invoice", query: "invoice", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRecencyOnlyQuery(tt.query); got != tt.want {
				t.Fatalf("isRecencyOnlyQuery(%q)=%v want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestApplyRecencyAndLimit(t *testing.T) {
	now := time.Now()
	items := []model.ListItem{
		{MsgID: "old", ReceivedAt: now.Add(-2 * time.Hour)},
		{MsgID: "new", ReceivedAt: now.Add(-30 * time.Minute)},
		{MsgID: "mid", ReceivedAt: now.Add(-1 * time.Hour)},
	}

	out := applyRecencyAndLimit(append([]model.ListItem(nil), items...), true, 2)
	if len(out) != 2 {
		t.Fatalf("unexpected len=%d", len(out))
	}
	if out[0].MsgID != "new" || out[1].MsgID != "mid" {
		t.Fatalf("unexpected order: %s, %s", out[0].MsgID, out[1].MsgID)
	}

	outNoRecency := applyRecencyAndLimit(append([]model.ListItem(nil), items...), false, 2)
	if outNoRecency[0].MsgID != "old" {
		t.Fatalf("expected original ordering when recency disabled")
	}
}

func TestInferFolderFromQuery(t *testing.T) {
	tests := []struct {
		name         string
		query        string
		wantFolder   string
		wantStripped string
	}{
		{name: "junk intent", query: "junk emails from alice@example.com", wantFolder: "junk", wantStripped: "emails from alice@example.com"},
		{name: "spam intent", query: "show spam from bob", wantFolder: "junk", wantStripped: "show from bob"},
		{name: "inbox intent", query: "inbox recent updates", wantFolder: "inbox", wantStripped: "recent updates"},
		{name: "sent intent", query: "sent message to carol", wantFolder: "sent", wantStripped: "message to carol"},
		{name: "none", query: "invoice overdue", wantFolder: "", wantStripped: "invoice overdue"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folder, stripped := inferFolderFromQuery(tt.query)
			if folder != tt.wantFolder || stripped != tt.wantStripped {
				t.Fatalf("inferFolderFromQuery(%q)=(%q,%q) want (%q,%q)", tt.query, folder, stripped, tt.wantFolder, tt.wantStripped)
			}
		})
	}
}

func TestParseNaturalLanguageSearch(t *testing.T) {
	svc := &Service{}
	req := svc.normalizeStructuredSearch(context.Background(), model.SearchRequest{
		Intent: model.SearchIntent{
			Text: "my mails that I've not seen since yesterday from x@mail.example.test and has not been responded to",
		},
	})
	if len(req.Filters.Headers.From) != 1 || req.Filters.Headers.From[0] != "x@mail.example.test" {
		t.Fatalf("from filters = %#v", req.Filters.Headers.From)
	}
	if len(req.Filters.Flags.Without) != 2 {
		t.Fatalf("without flags = %#v", req.Filters.Flags.Without)
	}
	if req.Filters.Date.InternalSince == nil {
		t.Fatalf("expected internal since to be extracted")
	}
	if req.Intent.Text != "" {
		t.Fatalf("expected residual text to be empty, got %q", req.Intent.Text)
	}
}

func TestParseNaturalLanguageSearchKeepsResidualIntent(t *testing.T) {
	svc := &Service{}
	req := svc.normalizeStructuredSearch(context.Background(), model.SearchRequest{
		Intent: model.SearchIntent{
			Text: "my unseen emails since yesterday from x@mail.example.test about the contract issue",
		},
	})
	if got := req.Intent.Text; got != "about the contract issue" {
		t.Fatalf("residual intent = %q", got)
	}
	if req.ParserMode != "fallback" {
		t.Fatalf("parser mode = %q", req.ParserMode)
	}
}

func TestNormalizeStructuredSearchUsesLLMParserFirst(t *testing.T) {
	svc := &Service{
		SearchParser: stubSearchParser{
			req: model.SearchRequest{
				Filters: model.SearchFilters{
					Headers: model.SearchHeaderFilters{From: []string{"x@mail.example.test"}},
					Flags:   model.SearchFlagFilters{Without: []string{"seen", "answered"}},
				},
				Intent: model.SearchIntent{Text: "contract issue"},
				Mode:   model.SearchModeHybrid,
			},
		},
	}
	req := svc.normalizeStructuredSearch(context.Background(), model.SearchRequest{
		Intent: model.SearchIntent{Text: "my unseen mails from x@mail.example.test that have not been replied to about contract issue"},
	})
	if len(req.Filters.Headers.From) != 1 || req.Filters.Headers.From[0] != "x@mail.example.test" {
		t.Fatalf("from filters = %#v", req.Filters.Headers.From)
	}
	if got := req.Intent.Text; got != "contract issue" {
		t.Fatalf("intent text = %q", got)
	}
	if req.ParserMode != "llm" {
		t.Fatalf("parser mode = %q", req.ParserMode)
	}
}

func TestNormalizeStructuredSearchFallsBackWhenParserFails(t *testing.T) {
	svc := &Service{
		SearchParser: stubSearchParser{err: errors.New("upstream parser failed")},
	}
	req := svc.normalizeStructuredSearch(context.Background(), model.SearchRequest{
		Intent: model.SearchIntent{Text: "my unseen emails since yesterday from x@mail.example.test"},
	})
	if len(req.Filters.Headers.From) != 1 || req.Filters.Headers.From[0] != "x@mail.example.test" {
		t.Fatalf("from filters = %#v", req.Filters.Headers.From)
	}
	if len(req.Filters.Flags.Without) == 0 {
		t.Fatalf("expected fallback flags")
	}
	if req.ParserMode != "fallback" {
		t.Fatalf("parser mode = %q", req.ParserMode)
	}
}

func TestNormalizeStructuredSearchResolvesTemporalFilter(t *testing.T) {
	loc, err := time.LoadLocation("America/Indiana/Indianapolis")
	if err != nil {
		t.Fatalf("LoadLocation() err = %v", err)
	}
	svc := &Service{
		SearchParser: stubSearchParser{
			req: model.SearchRequest{
				Filters: model.SearchFilters{
					Date: model.SearchDateFilters{
						Temporal: &model.SearchTemporalFilter{
							Kind:     "relative_day",
							Value:    "yesterday",
							Field:    "internal",
							Timezone: "America/Indiana/Indianapolis",
						},
					},
				},
				Intent: model.SearchIntent{Text: "mails from yesterday"},
				Mode:   model.SearchModeHybrid,
			},
		},
	}
	req := svc.normalizeStructuredSearch(context.Background(), model.SearchRequest{
		Intent: model.SearchIntent{Text: "mails from yesterday"},
	})
	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).Add(-24 * time.Hour)
	end := start.Add(24 * time.Hour)
	if req.Filters.Date.InternalSince == nil || !req.Filters.Date.InternalSince.Equal(start.UTC()) {
		t.Fatalf("internal since = %v want %v", req.Filters.Date.InternalSince, start.UTC())
	}
	if req.Filters.Date.InternalBefore == nil || !req.Filters.Date.InternalBefore.Equal(end.UTC()) {
		t.Fatalf("internal before = %v want %v", req.Filters.Date.InternalBefore, end.UTC())
	}
}

func TestNormalizeStructuredSearchResolvesTemporalFilterWithDefaultTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/Indiana/Indianapolis")
	if err != nil {
		t.Fatalf("LoadLocation() err = %v", err)
	}
	svc := &Service{
		SearchDefaultTimezone: "America/Indiana/Indianapolis",
		SearchParser: stubSearchParser{
			req: model.SearchRequest{
				Filters: model.SearchFilters{
					Date: model.SearchDateFilters{
						Temporal: &model.SearchTemporalFilter{
							Kind:  "relative_day",
							Value: "yesterday",
							Field: "internal",
						},
					},
				},
				Intent: model.SearchIntent{Text: "mails from yesterday"},
				Mode:   model.SearchModeHybrid,
			},
		},
	}
	req := svc.normalizeStructuredSearch(context.Background(), model.SearchRequest{
		Intent: model.SearchIntent{Text: "mails from yesterday"},
	})
	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).Add(-24 * time.Hour)
	end := start.Add(24 * time.Hour)
	if req.Filters.Date.InternalSince == nil || !req.Filters.Date.InternalSince.Equal(start.UTC()) {
		t.Fatalf("internal since = %v want %v", req.Filters.Date.InternalSince, start.UTC())
	}
	if req.Filters.Date.InternalBefore == nil || !req.Filters.Date.InternalBefore.Equal(end.UTC()) {
		t.Fatalf("internal before = %v want %v", req.Filters.Date.InternalBefore, end.UTC())
	}
}

func TestNormalizeStructuredSearchResolvesTemporalRangeWithDefaultTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/Indiana/Indianapolis")
	if err != nil {
		t.Fatalf("LoadLocation() err = %v", err)
	}
	svc := &Service{
		SearchDefaultTimezone: "America/Indiana/Indianapolis",
		SearchParser: stubSearchParser{
			req: model.SearchRequest{
				Filters: model.SearchFilters{
					Date: model.SearchDateFilters{
						Field: "internal",
						Range: &model.SearchDateRange{
							Start: &model.SearchTemporalPoint{
								Type:      "relative_offset",
								Direction: "past",
								Amount:    2,
								Unit:      "week",
								Snap:      "start_of_day",
							},
							End: &model.SearchTemporalPoint{
								Type:  "relative_day",
								Value: "yesterday",
							},
						},
					},
				},
				Intent: model.SearchIntent{Text: "mails from 2 weeks ago to yesterday"},
				Mode:   model.SearchModeHybrid,
			},
		},
	}
	req := svc.normalizeStructuredSearch(context.Background(), model.SearchRequest{
		Intent: model.SearchIntent{Text: "mails from 2 weeks ago to yesterday"},
	})
	now := time.Now().In(loc)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	wantStart := todayStart.AddDate(0, 0, -14)
	wantEnd := todayStart
	if req.Filters.Date.InternalSince == nil || !req.Filters.Date.InternalSince.Equal(wantStart.UTC()) {
		t.Fatalf("internal since = %v want %v", req.Filters.Date.InternalSince, wantStart.UTC())
	}
	if req.Filters.Date.InternalBefore == nil || !req.Filters.Date.InternalBefore.Equal(wantEnd.UTC()) {
		t.Fatalf("internal before = %v want %v", req.Filters.Date.InternalBefore, wantEnd.UTC())
	}
}

func TestNormalizeStructuredSearchResolvesLastWeekNamedWeekdayRange(t *testing.T) {
	loc, err := time.LoadLocation("America/Indiana/Indianapolis")
	if err != nil {
		t.Fatalf("LoadLocation() err = %v", err)
	}
	svc := &Service{
		SearchDefaultTimezone: "America/Indiana/Indianapolis",
		SearchParser: stubSearchParser{
			req: model.SearchRequest{
				Filters: model.SearchFilters{
					Date: model.SearchDateFilters{
						Field: "internal",
						Range: &model.SearchDateRange{
							Start: &model.SearchTemporalPoint{
								Type:    "relative_named_weekday",
								Value:   "last",
								Weekday: "monday",
							},
							End: &model.SearchTemporalPoint{
								Type:    "relative_named_weekday",
								Value:   "last",
								Weekday: "sunday",
							},
							EndInclusive: true,
						},
					},
				},
				Intent: model.SearchIntent{Text: "test emails from last week"},
				Mode:   model.SearchModeHybrid,
			},
		},
	}
	req := svc.normalizeStructuredSearch(context.Background(), model.SearchRequest{
		Intent: model.SearchIntent{Text: "test emails from last week"},
	})
	now := time.Now().In(loc)
	thisWeekMonday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	for thisWeekMonday.Weekday() != time.Monday {
		thisWeekMonday = thisWeekMonday.AddDate(0, 0, -1)
	}
	lastMonday := thisWeekMonday.AddDate(0, 0, -7)
	lastSundayEnd := thisWeekMonday
	if req.Filters.Date.InternalSince == nil || !req.Filters.Date.InternalSince.Equal(lastMonday.UTC()) {
		t.Fatalf("internal since = %v want %v", req.Filters.Date.InternalSince, lastMonday.UTC())
	}
	if req.Filters.Date.InternalBefore == nil || req.Filters.Date.InternalBefore.UTC().Before(lastSundayEnd.UTC()) || req.Filters.Date.InternalBefore.UTC().After(lastSundayEnd.UTC().Add(time.Millisecond)) {
		t.Fatalf("internal before = %v want approx %v", req.Filters.Date.InternalBefore, lastSundayEnd.UTC())
	}
}

func TestSearchStructuredSoftensLLMSubjectAndBodyHintsInHybridMode(t *testing.T) {
	store := newFakeStore()
	var keywordReqs []model.SearchRequest
	var semanticReqs []model.SearchRequest
	store.searchStructuredFn = func(ctx context.Context, claims model.TokenClaims, req model.SearchRequest) ([]model.ListItem, error) {
		keywordReqs = append(keywordReqs, req)
		if len(req.Filters.Headers.Subject) > 0 || len(req.Intent.BodyTerms) > 0 || len(req.Intent.HeaderTerms.Subject) > 0 {
			return []model.ListItem{{MsgID: "strict", ReceivedAt: time.Unix(200, 0).UTC()}}, nil
		}
		return []model.ListItem{{MsgID: "broad", ReceivedAt: time.Unix(100, 0).UTC()}}, nil
	}
	store.searchSemanticFn = func(ctx context.Context, claims model.TokenClaims, req model.SearchRequest, embedding []float32) ([]model.ListItem, error) {
		semanticReqs = append(semanticReqs, req)
		return []model.ListItem{{MsgID: "broad", ReceivedAt: time.Unix(100, 0).UTC()}}, nil
	}
	svc := &Service{
		Store:    store,
		Embedder: stubEmbedder{},
		SearchParser: stubSearchParser{
			req: model.SearchRequest{
				Filters: model.SearchFilters{
					Headers: model.SearchHeaderFilters{Subject: []string{"testing"}},
				},
				Intent: model.SearchIntent{
					Text:      "emails about testing",
					BodyTerms: []string{"testing"},
					HeaderTerms: model.SearchHeaderIntent{
						Subject: []string{"testing"},
					},
				},
				Mode: model.SearchModeHybrid,
			},
		},
	}
	out, err := svc.SearchStructured(context.Background(), model.TokenClaims{}, model.SearchRequest{
		Intent: model.SearchIntent{Text: "emails about testing"},
		Mode:   model.SearchModeHybrid,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("SearchStructured() err = %v", err)
	}
	if len(out) != 2 || out[0].MsgID != "strict" || out[1].MsgID != "broad" {
		t.Fatalf("unexpected results = %#v", out)
	}
	if len(keywordReqs) != 2 {
		t.Fatalf("keyword requests = %d want 2", len(keywordReqs))
	}
	if len(keywordReqs[0].Filters.Headers.Subject) != 0 || len(keywordReqs[0].Intent.BodyTerms) != 0 || len(keywordReqs[0].Intent.HeaderTerms.Subject) != 0 {
		t.Fatalf("first keyword request was not broadened: %#v", keywordReqs[0])
	}
	if len(keywordReqs[1].Filters.Headers.Subject) == 0 || len(keywordReqs[1].Intent.BodyTerms) == 0 {
		t.Fatalf("second keyword request lost strict hints: %#v", keywordReqs[1])
	}
	if len(semanticReqs) != 1 {
		t.Fatalf("semantic requests = %d want 1", len(semanticReqs))
	}
	if len(semanticReqs[0].Filters.Headers.Subject) != 0 || len(semanticReqs[0].Intent.BodyTerms) != 0 || len(semanticReqs[0].Intent.HeaderTerms.Subject) != 0 {
		t.Fatalf("semantic request was not broadened: %#v", semanticReqs[0])
	}
}
