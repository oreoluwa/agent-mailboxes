package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/config"
	"github.com/oreoluwa/agent-mailboxes/internal/embed"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/service"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type pocIntent struct {
	RewriteQuery    string   `json:"rewrite_query"`
	Folder          string   `json:"folder"`
	TemporalStart   string   `json:"temporal_start"`
	TemporalEnd     string   `json:"temporal_end"`
	TemporalMode    string   `json:"temporal_mode"`
	TimeConfidence  float64  `json:"time_confidence"`
	PlanType        string   `json:"plan_type"`
	EntityHints     []string `json:"entity_hints"`
	TopicHints      []string `json:"topic_hints"`
	NeedsRefinement bool     `json:"needs_refinement"`
	Notes           []string `json:"notes"`
}

type pocVariantResult struct {
	Name             string           `json:"name"`
	Top              []model.ListItem `json:"top"`
	TopIDs           []string         `json:"top_ids"`
	LLMRelevance     float64          `json:"llm_relevance"`
	LexicalRelevance float64          `json:"lexical_relevance"`
	TemporalHit      *bool            `json:"temporal_hit,omitempty"`
	LatencyMS        int64            `json:"latency_ms"`
	Trace            []string         `json:"trace"`
	Error            string           `json:"error,omitempty"`
}

type pocQueryResult struct {
	MailboxName string           `json:"mailbox_name"`
	MailboxID   string           `json:"mailbox_id"`
	Query       string           `json:"query"`
	Intent      pocIntent        `json:"intent"`
	Baseline    pocVariantResult `json:"baseline"`
	MinimalLLM  pocVariantResult `json:"minimal_llm"`
	ExpandedLLM pocVariantResult `json:"expanded_llm"`
	StartedAt   time.Time        `json:"started_at"`
	FinishedAt  time.Time        `json:"finished_at"`
	Meta        map[string]any   `json:"meta"`
}

type pocMailboxSummary struct {
	MailboxName string  `json:"mailbox_name"`
	MailboxID   string  `json:"mailbox_id"`
	Messages    int     `json:"messages"`
	Queries     int     `json:"queries"`
	AvgBaseline float64 `json:"avg_baseline"`
	AvgMinimal  float64 `json:"avg_minimal"`
	AvgExpanded float64 `json:"avg_expanded"`
	TempBaseHit float64 `json:"temporal_hit_baseline"`
	TempMinHit  float64 `json:"temporal_hit_minimal"`
	TempExpHit  float64 `json:"temporal_hit_expanded"`
}

type pocReport struct {
	GeneratedAt      time.Time           `json:"generated_at"`
	Model            string              `json:"model"`
	BaseURL          string              `json:"base_url"`
	MailboxSummaries []pocMailboxSummary `json:"mailbox_summaries"`
	Results          []pocQueryResult    `json:"results"`
	Notes            []string            `json:"notes"`
}

type llmClient struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client
}

type chatReq struct {
	Model       string          `json:"model"`
	Messages    []chatReqMsg    `json:"messages"`
	Temperature float64         `json:"temperature"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Format      json.RawMessage `json:"response_format,omitempty"`
}

type chatReqMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func runSearchPOCEval() {
	fs := flag.NewFlagSet("emaild search-poc-eval", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { searchPOCEvalUsage(fs.Output()) }
	mailboxName := fs.String("mailbox", "", "Evaluate one enabled mailbox")
	allMailboxes := fs.Bool("all-mailboxes", false, "Evaluate all enabled mailboxes")
	maxQueries := fs.Int("max-queries", 12, "Max queries per mailbox")
	days := fs.Int("days", 30, "Audit lookback window in days for sampling queries")
	limit := fs.Int("limit", 5, "Top results returned per strategy")
	candidateLimit := fs.Int("candidate-limit", 24, "Candidate pool size for expanded strategy")
	judge := fs.Bool("judge", true, "Use LLM judging for top-result relevance scoring")
	jsonOut := fs.Bool("json", false, "Print full JSON report")
	timezone := fs.String("timezone", "", "Timezone for temporal grounding (default local)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fatal(err)
	}
	if *allMailboxes && strings.TrimSpace(*mailboxName) != "" {
		fatal(fmt.Errorf("cannot combine --mailbox with --all-mailboxes"))
	}
	if *maxQueries <= 0 {
		fatal(fmt.Errorf("--max-queries must be > 0"))
	}
	if *days <= 0 {
		fatal(fmt.Errorf("--days must be > 0"))
	}
	if *limit <= 0 {
		fatal(fmt.Errorf("--limit must be > 0"))
	}
	if *candidateLimit < *limit {
		*candidateLimit = *limit
	}

	cfg := mustConfig()
	db := dbConnect(cfg.PostgresDSN)
	defer db.Close()
	st := store.NewPostgres(db)
	if err := configureMailboxCredentialEncryption(st, cfg); err != nil {
		fatal(err)
	}
	mailboxes, _, err := resolveMailboxes(context.Background(), st, strings.TrimSpace(*mailboxName), *allMailboxes)
	if err != nil {
		fatal(err)
	}
	if len(mailboxes) == 0 {
		fatal(fmt.Errorf("no enabled mailbox to evaluate"))
	}

	loc := time.Local
	if tz := strings.TrimSpace(*timezone); tz != "" {
		loaded, err := time.LoadLocation(tz)
		if err != nil {
			fatal(fmt.Errorf("invalid timezone %q: %w", tz, err))
		}
		loc = loaded
	}
	llm := newLLMClient(cfg)

	report := pocReport{
		GeneratedAt: time.Now().UTC(),
		Model:       cfg.ModelName,
		BaseURL:     cfg.OpenAIBaseURL,
		Results:     make([]pocQueryResult, 0, len(mailboxes)*(*maxQueries)),
		Notes: []string{
			"POC runs baseline current search unchanged, then two optional intelligence overlays.",
			"Minimal LLM: intent extraction + temporal/folder-aware post-filtering.",
			"Expanded LLM: intent extraction + multi-candidate retrieval + LLM rerank.",
		},
	}

	for _, mb := range mailboxes {
		var emb embed.Embedder
		if strings.TrimSpace(cfg.EmbeddingModel) != "" {
			emb = embed.NewOpenAICompat(cfg.OpenAIBaseURL, cfg.EmbeddingModel, cfg.OpenAIAPIKey)
		}
		svc := &service.Service{Store: st, Embedder: emb, MailboxID: mb.ID}
		claims := adminClaimsForMailbox(mb.ID)
		queries, qmeta, err := sampleQueries(context.Background(), db, mb.ID, *days, *maxQueries)
		if err != nil {
			fatal(err)
		}
		if len(queries) == 0 {
			log.Printf("search-poc: mailbox=%s has no sampled queries", mb.Name)
			report.MailboxSummaries = append(report.MailboxSummaries, pocMailboxSummary{MailboxName: mb.Name, MailboxID: mb.ID, Messages: qmeta["messages_count"].(int)})
			continue
		}
		for _, q := range queries {
			start := time.Now()
			intent, intentErr := extractIntent(context.Background(), llm, q, loc, false)
			if intentErr != nil {
				intent = pocIntent{RewriteQuery: q, Folder: "", Notes: []string{"intent extraction failed; fallback to baseline semantics"}}
			}
			baseline := runBaseline(context.Background(), svc, claims, q, *limit)
			minimal := runMinimal(context.Background(), llm, svc, claims, q, intent, *limit, loc)
			expanded := runExpanded(context.Background(), llm, svc, claims, q, intent, *limit, *candidateLimit, loc)

			baseline.LexicalRelevance = lexicalRelevance(q, baseline.Top)
			minimal.LexicalRelevance = lexicalRelevance(q, minimal.Top)
			expanded.LexicalRelevance = lexicalRelevance(q, expanded.Top)

			if *judge {
				baseline.LLMRelevance = judgeVariant(context.Background(), llm, q, baseline.Top)
				minimal.LLMRelevance = judgeVariant(context.Background(), llm, q, minimal.Top)
				expanded.LLMRelevance = judgeVariant(context.Background(), llm, q, expanded.Top)
			} else {
				baseline.LLMRelevance = baseline.LexicalRelevance
				minimal.LLMRelevance = minimal.LexicalRelevance
				expanded.LLMRelevance = expanded.LexicalRelevance
			}

			if ws, we, ok := parseWindow(intent, loc); ok {
				baseline.TemporalHit = ptrBool(topHitsWindow(baseline.Top, ws, we))
				minimal.TemporalHit = ptrBool(topHitsWindow(minimal.Top, ws, we))
				expanded.TemporalHit = ptrBool(topHitsWindow(expanded.Top, ws, we))
			}

			report.Results = append(report.Results, pocQueryResult{
				MailboxName: mb.Name,
				MailboxID:   mb.ID,
				Query:       q,
				Intent:      intent,
				Baseline:    baseline,
				MinimalLLM:  minimal,
				ExpandedLLM: expanded,
				StartedAt:   start.UTC(),
				FinishedAt:  time.Now().UTC(),
				Meta: map[string]any{
					"sampled_from": qmeta["source"],
					"intent_error": errorString(intentErr),
				},
			})
		}
		report.MailboxSummaries = append(report.MailboxSummaries, summarizeMailboxResults(mb, qmeta["messages_count"].(int), report.Results))
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return
	}
	printPOCReport(os.Stdout, report)
}

func newLLMClient(cfg config.Config) *llmClient {
	base := strings.TrimRight(strings.TrimSpace(cfg.OpenAIBaseURL), "/")
	base = strings.TrimSuffix(base, "/v1")
	return &llmClient{
		baseURL: base,
		model:   strings.TrimSpace(cfg.ModelName),
		apiKey:  strings.TrimSpace(cfg.OpenAIAPIKey),
		http:    &http.Client{Timeout: 40 * time.Second},
	}
}

func adminClaimsForMailbox(mailboxID string) model.TokenClaims {
	id := strings.TrimSpace(mailboxID)
	return model.TokenClaims{
		TokenID:   "search-poc-eval",
		MailboxID: &id,
		Scopes: map[model.Scope]bool{
			model.ScopeRead:  true,
			model.ScopeAdmin: true,
		},
	}
}

func sampleQueries(ctx context.Context, db *sql.DB, mailboxID string, days, max int) ([]string, map[string]any, error) {
	rows, err := db.QueryContext(ctx, `SELECT al.target
FROM audit_log al
JOIN tokens t ON t.id=al.token_id
LEFT JOIN aliases a ON a.id=t.alias_id
WHERE al.action='search'
  AND NULLIF(btrim(al.target),'') IS NOT NULL
  AND al.created_at >= now() - ($2::text || ' days')::interval
  AND (a.mailbox_id=$1 OR t.alias_id IS NULL)
ORDER BY al.created_at DESC
LIMIT 200`, mailboxID, strconv.Itoa(days))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	queries := make([]string, 0, max)
	seen := map[string]struct{}{}
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			return nil, nil, err
		}
		norm := normalizeQuery(q)
		if norm == "" {
			continue
		}
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		queries = append(queries, q)
		if len(queries) >= max {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	msgCount := 0
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE mailbox_id=$1`, mailboxID).Scan(&msgCount); err != nil {
		return nil, nil, err
	}
	if len(queries) < max {
		auto, err := generatedQueries(ctx, db, mailboxID, max-len(queries))
		if err == nil {
			for _, q := range auto {
				norm := normalizeQuery(q)
				if norm == "" {
					continue
				}
				if _, ok := seen[norm]; ok {
					continue
				}
				seen[norm] = struct{}{}
				queries = append(queries, q)
				if len(queries) >= max {
					break
				}
			}
		}
	}
	if len(queries) == 0 {
		queries = append(queries, "my recent emails", "emails from yesterday")
	}
	return queries, map[string]any{
		"source":         "audit_log + generated",
		"messages_count": msgCount,
	}, nil
}

func generatedQueries(ctx context.Context, db *sql.DB, mailboxID string, need int) ([]string, error) {
	if need <= 0 {
		return nil, nil
	}
	out := []string{"my recent emails", "emails from yesterday", "emails from last week"}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(m.subject,''), COALESCE(m.from_email::text,''), m.internal_date
FROM messages m
WHERE m.mailbox_id=$1
ORDER BY m.internal_date DESC
LIMIT 20`, mailboxID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var subj, from string
		var dt time.Time
		if err := rows.Scan(&subj, &from, &dt); err != nil {
			return out, err
		}
		kw := firstMeaningfulToken(subj)
		if kw != "" {
			out = append(out, "recent "+kw+" emails")
			out = append(out, kw+" from yesterday")
		}
		if from != "" {
			out = append(out, "emails from "+from)
		}
		if len(out) >= need+3 {
			break
		}
	}
	return out, rows.Err()
}

func firstMeaningfulToken(s string) string {
	stop := map[string]struct{}{"re": {}, "fw": {}, "fwd": {}, "the": {}, "and": {}, "for": {}, "your": {}, "from": {}, "with": {}, "this": {}, "that": {}, "again": {}, "test": {}}
	parts := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, p := range parts {
		if len(p) < 3 {
			continue
		}
		if _, ok := stop[p]; ok {
			continue
		}
		return p
	}
	return ""
}

func runBaseline(ctx context.Context, svc *service.Service, claims model.TokenClaims, query string, limit int) pocVariantResult {
	start := time.Now()
	items, err := svc.Search(ctx, claims, query, "", limit)
	res := pocVariantResult{Name: "baseline", LatencyMS: time.Since(start).Milliseconds(), Top: items, TopIDs: topIDs(items), Trace: []string{"current Service.Search pipeline"}}
	if err != nil {
		res.Error = err.Error()
		res.Top = nil
		res.TopIDs = nil
	}
	return res
}

func runMinimal(ctx context.Context, llm *llmClient, svc *service.Service, claims model.TokenClaims, query string, seed pocIntent, limit int, loc *time.Location) pocVariantResult {
	start := time.Now()
	intent := seed
	trace := []string{"intent extraction (minimal)", "search + optional temporal hard-filter"}
	if normalizeQuery(intent.RewriteQuery) == "" {
		intent.RewriteQuery = query
	}
	if strings.TrimSpace(intent.RewriteQuery) == "" {
		intent.RewriteQuery = query
	}
	items, err := svc.Search(ctx, claims, intent.RewriteQuery, normalizeFolder(intent.Folder), max(limit*3, limit))
	res := pocVariantResult{Name: "minimal_llm", LatencyMS: time.Since(start).Milliseconds(), Top: items, TopIDs: topIDs(items), Trace: trace}
	if err != nil {
		res.Error = err.Error()
		res.Top = nil
		res.TopIDs = nil
		return res
	}
	if ws, we, ok := parseWindow(intent, loc); ok && intent.TimeConfidence >= 0.65 {
		filtered := filterItemsByWindow(items, ws, we)
		if len(filtered) > 0 {
			items = filtered
			res.Trace = append(res.Trace, "applied hard temporal filter")
		} else {
			res.Trace = append(res.Trace, "temporal filter empty; retained unfiltered candidates")
		}
	}
	if len(items) > limit {
		items = items[:limit]
	}
	res.Top = items
	res.TopIDs = topIDs(items)
	res.LatencyMS = time.Since(start).Milliseconds()
	return res
}

func runExpanded(ctx context.Context, llm *llmClient, svc *service.Service, claims model.TokenClaims, query string, seed pocIntent, limit, candidateLimit int, loc *time.Location) pocVariantResult {
	start := time.Now()
	intent := seed
	trace := []string{"intent extraction (expanded)", "multi-retriever candidate harvest", "LLM rerank"}
	if next, err := extractIntent(ctx, llm, query, loc, true); err == nil {
		intent = next
	} else {
		trace = append(trace, "expanded intent extraction failed; fallback to seed intent")
	}
	candidateMap := map[string]model.ListItem{}
	queries := []string{query}
	if rq := strings.TrimSpace(intent.RewriteQuery); rq != "" && normalizeQuery(rq) != normalizeQuery(query) {
		queries = append(queries, rq)
	}
	for _, hint := range intent.TopicHints {
		hint = strings.TrimSpace(hint)
		if hint == "" {
			continue
		}
		queries = append(queries, hint)
	}
	folder := normalizeFolder(intent.Folder)
	for _, q := range uniqueStrings(queries) {
		items, err := svc.Search(ctx, claims, q, folder, candidateLimit)
		if err != nil {
			continue
		}
		for _, it := range items {
			candidateMap[it.MsgID] = it
		}
	}
	if ws, we, ok := parseWindow(intent, loc); ok {
		items, err := svc.Search(ctx, claims, "", folder, candidateLimit)
		if err == nil {
			for _, it := range filterItemsByWindow(items, ws, we) {
				candidateMap[it.MsgID] = it
			}
		}
	}
	candidates := make([]model.ListItem, 0, len(candidateMap))
	for _, it := range candidateMap {
		candidates = append(candidates, it)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ReceivedAt.After(candidates[j].ReceivedAt)
	})
	if len(candidates) == 0 {
		return pocVariantResult{Name: "expanded_llm", LatencyMS: time.Since(start).Milliseconds(), Trace: append(trace, "no candidates")}
	}
	ranked, notes, err := rerankCandidatesLLM(ctx, llm, query, intent, candidates)
	res := pocVariantResult{Name: "expanded_llm", Trace: trace, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
		res.Trace = append(res.Trace, "LLM rerank failed; fallback recency ordering")
		ranked = candidates
	} else if len(notes) > 0 {
		res.Trace = append(res.Trace, notes...)
	}
	if ws, we, ok := parseWindow(intent, loc); ok && intent.TimeConfidence >= 0.5 {
		ranked = stableTemporalBoost(ranked, ws, we)
	}
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	res.Top = ranked
	res.TopIDs = topIDs(ranked)
	res.LatencyMS = time.Since(start).Milliseconds()
	return res
}

func extractIntent(ctx context.Context, llm *llmClient, query string, loc *time.Location, expanded bool) (pocIntent, error) {
	if llm == nil {
		return pocIntent{RewriteQuery: query}, fmt.Errorf("llm unavailable")
	}
	now := time.Now().In(loc).Format(time.RFC3339)
	mode := "minimal"
	if expanded {
		mode = "expanded"
	}
	system := "You are an email search intent extractor. Return strict JSON only."
	user := fmt.Sprintf(`Mode: %s
Timezone: %s
Now: %s
User query: %q

Return JSON schema:
{
  "rewrite_query": string,
  "folder": ""|"all"|"inbox"|"junk"|"sent",
  "temporal_start": RFC3339 or "",
  "temporal_end": RFC3339 or "",
  "temporal_mode": "none"|"hard_filter"|"soft_boost",
  "time_confidence": number 0..1,
  "plan_type": "temporal_only"|"temporal_topic"|"entity_topic"|"broad_semantic"|"exact_lookup",
  "entity_hints": string[],
  "topic_hints": string[],
  "needs_refinement": boolean,
  "notes": string[]
}
Rules: preserve user intent; if temporal phrase is explicit (today, yesterday, last week, this month, before/after date), emit concrete RFC3339 bounds in the provided timezone.`, mode, loc.String(), now, query)
	resp, err := llm.chatJSON(ctx, system, user, 800)
	if err != nil {
		return pocIntent{}, err
	}
	var out pocIntent
	if err := json.Unmarshal([]byte(extractJSON(resp)), &out); err != nil {
		return pocIntent{}, err
	}
	if strings.TrimSpace(out.RewriteQuery) == "" {
		out.RewriteQuery = query
	}
	out.Folder = normalizeFolder(out.Folder)
	if out.TimeConfidence < 0 {
		out.TimeConfidence = 0
	}
	if out.TimeConfidence > 1 {
		out.TimeConfidence = 1
	}
	return out, nil
}

func parseWindow(in pocIntent, loc *time.Location) (time.Time, time.Time, bool) {
	if strings.TrimSpace(in.TemporalStart) == "" || strings.TrimSpace(in.TemporalEnd) == "" {
		return time.Time{}, time.Time{}, false
	}
	start, err := time.Parse(time.RFC3339, in.TemporalStart)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	end, err := time.Parse(time.RFC3339, in.TemporalEnd)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return start.In(loc), end.In(loc), end.After(start)
}

func filterItemsByWindow(items []model.ListItem, start, end time.Time) []model.ListItem {
	out := make([]model.ListItem, 0, len(items))
	for _, it := range items {
		r := it.ReceivedAt
		if (r.Equal(start) || r.After(start)) && (r.Equal(end) || r.Before(end)) {
			out = append(out, it)
		}
	}
	return out
}

func stableTemporalBoost(items []model.ListItem, start, end time.Time) []model.ListItem {
	if len(items) <= 1 {
		return items
	}
	in := make([]model.ListItem, 0, len(items))
	out := make([]model.ListItem, 0, len(items))
	for _, it := range items {
		r := it.ReceivedAt
		if (r.Equal(start) || r.After(start)) && (r.Equal(end) || r.Before(end)) {
			in = append(in, it)
			continue
		}
		out = append(out, it)
	}
	return append(in, out...)
}

func rerankCandidatesLLM(ctx context.Context, llm *llmClient, query string, intent pocIntent, candidates []model.ListItem) ([]model.ListItem, []string, error) {
	if llm == nil {
		return nil, nil, fmt.Errorf("llm unavailable")
	}
	if len(candidates) > 40 {
		candidates = candidates[:40]
	}
	type c struct {
		ID         string `json:"id"`
		Subject    string `json:"subject"`
		From       string `json:"from"`
		Folder     string `json:"folder"`
		ReceivedAt string `json:"received_at"`
		Summary    string `json:"summary"`
	}
	packed := make([]c, 0, len(candidates))
	for _, it := range candidates {
		packed = append(packed, c{
			ID:         it.MsgID,
			Subject:    stringValue(it.Subject),
			From:       it.From.Email,
			Folder:     it.Folder,
			ReceivedAt: it.ReceivedAt.Format(time.RFC3339),
			Summary:    it.Digest.SummaryShort,
		})
	}
	payload, _ := json.Marshal(packed)
	intentRaw, _ := json.Marshal(intent)
	system := "You are an email search ranker. Return strict JSON only."
	user := fmt.Sprintf(`Query: %q
Intent: %s
Candidates: %s

Return JSON:
{"ranked_ids":["id1","id2"],"confidence":0..1,"notes":["short"]}
Rules: rank by semantic relevance first, then temporal/folder/entity alignment from Intent.`, query, string(intentRaw), string(payload))
	resp, err := llm.chatJSON(ctx, system, user, 1200)
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		RankedIDs  []string `json:"ranked_ids"`
		Confidence float64  `json:"confidence"`
		Notes      []string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(extractJSON(resp)), &out); err != nil {
		return nil, nil, err
	}
	idx := map[string]model.ListItem{}
	for _, it := range candidates {
		idx[it.MsgID] = it
	}
	ranked := make([]model.ListItem, 0, len(candidates))
	seen := map[string]struct{}{}
	for _, id := range out.RankedIDs {
		it, ok := idx[id]
		if !ok {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ranked = append(ranked, it)
	}
	for _, it := range candidates {
		if _, exists := seen[it.MsgID]; exists {
			continue
		}
		ranked = append(ranked, it)
	}
	notes := append([]string{}, out.Notes...)
	notes = append(notes, fmt.Sprintf("llm_rerank_confidence=%.2f", out.Confidence))
	return ranked, notes, nil
}

func judgeVariant(ctx context.Context, llm *llmClient, query string, items []model.ListItem) float64 {
	if len(items) == 0 {
		return 0
	}
	if llm == nil {
		return lexicalRelevance(query, items)
	}
	best := items[0]
	system := "You are a strict evaluator for email search relevance. Return JSON only."
	user := fmt.Sprintf(`Query: %q
Top result:
- subject: %q
- from: %q
- folder: %q
- received_at: %q
- summary: %q

Return JSON: {"score": number 0..1, "reason": "short"}
Score should reflect how well this result satisfies the query intent.`, query, stringValue(best.Subject), best.From.Email, best.Folder, best.ReceivedAt.Format(time.RFC3339), best.Digest.SummaryShort)
	resp, err := llm.chatJSON(ctx, system, user, 180)
	if err != nil {
		return lexicalRelevance(query, items)
	}
	var out struct {
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal([]byte(extractJSON(resp)), &out); err != nil {
		return lexicalRelevance(query, items)
	}
	if out.Score < 0 {
		return 0
	}
	if out.Score > 1 {
		return 1
	}
	return out.Score
}

func lexicalRelevance(query string, items []model.ListItem) float64 {
	if len(items) == 0 {
		return 0
	}
	toks := tokenSet(query)
	if len(toks) == 0 {
		return 0.5
	}
	best := 0.0
	for _, it := range items {
		h := strings.ToLower(strings.Join([]string{stringValue(it.Subject), it.From.Email, it.Digest.SummaryShort}, " "))
		hit := 0
		for tok := range toks {
			if strings.Contains(h, tok) {
				hit++
			}
		}
		score := float64(hit) / float64(len(toks))
		if score > best {
			best = score
		}
	}
	return best
}

func tokenSet(s string) map[string]struct{} {
	parts := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	stop := map[string]struct{}{"the": {}, "and": {}, "for": {}, "with": {}, "from": {}, "about": {}, "email": {}, "emails": {}, "message": {}, "messages": {}, "my": {}}
	out := map[string]struct{}{}
	for _, p := range parts {
		if len(p) < 3 {
			continue
		}
		if _, ok := stop[p]; ok {
			continue
		}
		out[p] = struct{}{}
	}
	return out
}

func topHitsWindow(items []model.ListItem, start, end time.Time) bool {
	if len(items) == 0 {
		return false
	}
	r := items[0].ReceivedAt
	return (r.Equal(start) || r.After(start)) && (r.Equal(end) || r.Before(end))
}

func summarizeMailboxResults(mb store.Mailbox, messageCount int, all []pocQueryResult) pocMailboxSummary {
	own := make([]pocQueryResult, 0, len(all))
	for _, r := range all {
		if r.MailboxID == mb.ID {
			own = append(own, r)
		}
	}
	s := pocMailboxSummary{MailboxName: mb.Name, MailboxID: mb.ID, Messages: messageCount, Queries: len(own)}
	if len(own) == 0 {
		return s
	}
	var base, minv, exp float64
	var tBaseN, tMinN, tExpN float64
	var tBaseH, tMinH, tExpH float64
	for _, r := range own {
		base += r.Baseline.LLMRelevance
		minv += r.MinimalLLM.LLMRelevance
		exp += r.ExpandedLLM.LLMRelevance
		if r.Baseline.TemporalHit != nil {
			tBaseN++
			if *r.Baseline.TemporalHit {
				tBaseH++
			}
		}
		if r.MinimalLLM.TemporalHit != nil {
			tMinN++
			if *r.MinimalLLM.TemporalHit {
				tMinH++
			}
		}
		if r.ExpandedLLM.TemporalHit != nil {
			tExpN++
			if *r.ExpandedLLM.TemporalHit {
				tExpH++
			}
		}
	}
	s.AvgBaseline = round3(base / float64(len(own)))
	s.AvgMinimal = round3(minv / float64(len(own)))
	s.AvgExpanded = round3(exp / float64(len(own)))
	if tBaseN > 0 {
		s.TempBaseHit = round3(tBaseH / tBaseN)
	}
	if tMinN > 0 {
		s.TempMinHit = round3(tMinH / tMinN)
	}
	if tExpN > 0 {
		s.TempExpHit = round3(tExpH / tExpN)
	}
	return s
}

func printPOCReport(w io.Writer, rep pocReport) {
	fmt.Fprintf(w, "search-poc-eval generated_at=%s model=%s base_url=%s\n", rep.GeneratedAt.Format(time.RFC3339), rep.Model, rep.BaseURL)
	fmt.Fprintln(w, "")
	for _, s := range rep.MailboxSummaries {
		fmt.Fprintf(w, "mailbox=%s id=%s messages=%d queries=%d avg_score baseline=%.3f minimal=%.3f expanded=%.3f temporal_hit baseline=%.3f minimal=%.3f expanded=%.3f\n",
			s.MailboxName, s.MailboxID, s.Messages, s.Queries, s.AvgBaseline, s.AvgMinimal, s.AvgExpanded, s.TempBaseHit, s.TempMinHit, s.TempExpHit)
	}
	fmt.Fprintln(w, "")
	for i, r := range rep.Results {
		fmt.Fprintf(w, "%d) mailbox=%s query=%q\n", i+1, r.MailboxName, r.Query)
		fmt.Fprintf(w, "   baseline score=%.2f top=%s\n", r.Baseline.LLMRelevance, topSubject(r.Baseline.Top))
		fmt.Fprintf(w, "   minimal  score=%.2f top=%s\n", r.MinimalLLM.LLMRelevance, topSubject(r.MinimalLLM.Top))
		fmt.Fprintf(w, "   expanded score=%.2f top=%s\n", r.ExpandedLLM.LLMRelevance, topSubject(r.ExpandedLLM.Top))
		if r.Intent.NeedsRefinement {
			fmt.Fprintln(w, "   refinement_note=refining results with intent-aware plan")
		}
	}
}

func topSubject(items []model.ListItem) string {
	if len(items) == 0 {
		return "(none)"
	}
	s := stringValue(items[0].Subject)
	if strings.TrimSpace(s) == "" {
		s = "(no subject)"
	}
	return fmt.Sprintf("%q @ %s", s, items[0].ReceivedAt.Format(time.RFC3339))
}

func topIDs(items []model.ListItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.MsgID)
	}
	return out
}

func normalizeQuery(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

func normalizeFolder(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "all", "inbox", "junk", "spam", "sent":
		if s == "spam" {
			return "junk"
		}
		return s
	default:
		return ""
	}
}

func uniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, v := range in {
		n := normalizeQuery(v)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, strings.TrimSpace(v))
	}
	return out
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func stringValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptrBool(v bool) *bool { return &v }

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (c *llmClient) chatJSON(ctx context.Context, system, user string, maxTokens int) (string, error) {
	if c == nil {
		return "", fmt.Errorf("llm client is nil")
	}
	if strings.TrimSpace(c.baseURL) == "" || strings.TrimSpace(c.model) == "" {
		return "", fmt.Errorf("llm client is not configured")
	}
	reqBody := chatReq{
		Model: c.model,
		Messages: []chatReqMsg{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		MaxTokens:   maxTokens,
	}
	buf, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("chat status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out chatResp
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("chat response has no choices")
	}
	return out.Choices[0].Message.Content, nil
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

func searchPOCEvalUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild search-poc-eval [--mailbox <name> | --all-mailboxes] [--max-queries N] [--days N] [--limit N] [--candidate-limit N] [--judge=false] [--timezone TZ] [--json]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Description:")
	fmt.Fprintln(w, "  Runs a side-by-side search POC on live mailbox DB data without changing /v1/search behavior.")
	fmt.Fprintln(w, "  Strategies: baseline current search, minimal LLM intent layer, expanded LLM multi-layer rerank.")
}
