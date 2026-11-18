package main

import (
	"reflect"
	"testing"

	"github.com/oreoluwa/agent-mailboxes/internal/config"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

func TestNormalizeFlagsAnywhereTokenMint(t *testing.T) {
	got := normalizeFlagsAnywhere([]string{"chief.assistant@example.test", "--name", "chief", "--scopes", "read,send"}, map[string]bool{
		"--name":   true,
		"--scopes": true,
	})
	want := []string{"--name", "chief", "--scopes", "read,send", "chief.assistant@example.test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args: got=%v want=%v", got, want)
	}
}

func TestStripBoolFlag(t *testing.T) {
	got, found := stripBoolFlag([]string{"emaild", "--mint", "status"}, "--mint")
	want := []string{"emaild", "status"}
	if !found {
		t.Fatalf("expected flag to be found")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args: got=%v want=%v", got, want)
	}
}

func TestAttachmentDependencyIssuesRequiredMissing(t *testing.T) {
	cfg := config.Config{
		SummaryEnabled: true,
		AttachSummary: config.AttachmentSummaryConfig{
			Enabled:            true,
			PDFRenderToolBin:   "definitely-missing-tool-pdf",
			OfficeConverterBin: "definitely-missing-tool-office",
			RequirePDFRenderer: true,
		},
	}
	issues := attachmentDependencyIssues(cfg)
	if len(issues) < 2 {
		t.Fatalf("expected issues for missing tools, got=%d", len(issues))
	}
	foundRequired := false
	for _, is := range issues {
		if is.Name == "definitely-missing-tool-pdf" && is.Required {
			foundRequired = true
		}
	}
	if !foundRequired {
		t.Fatalf("expected required pdf dependency issue")
	}
}

func TestAttachmentDependencyIssuesSkippedWhenSummariesDisabled(t *testing.T) {
	cfg := config.Config{
		SummaryEnabled: false,
		AttachSummary: config.AttachmentSummaryConfig{
			Enabled:            true,
			PDFRenderToolBin:   "definitely-missing-tool-pdf",
			OfficeConverterBin: "definitely-missing-tool-office",
			RequirePDFRenderer: true,
		},
	}
	if issues := attachmentDependencyIssues(cfg); len(issues) != 0 {
		t.Fatalf("expected no attachment dependency checks with summaries disabled, got %+v", issues)
	}
}

func TestAgentPlannerForConfigDisabled(t *testing.T) {
	cfg := config.Config{
		SummaryEnabled: false,
		OpenAIBaseURL:  "http://127.0.0.1:1",
		AgentModelName: "would-call-model",
	}
	if planner := agentPlannerForConfig(cfg); planner != nil {
		t.Fatalf("expected no agent planner with summaries disabled, got %T", planner)
	}

	cfg.SummaryEnabled = true
	if planner := agentPlannerForConfig(cfg); planner == nil {
		t.Fatal("expected agent planner to remain available when summaries are enabled")
	}
}

func TestMailboxInShardDeterministic(t *testing.T) {
	id := "7f0552df-9d19-4af0-8f8a-f7f68e205233"
	a := mailboxInShard(id, 1, 8)
	b := mailboxInShard(id, 1, 8)
	if a != b {
		t.Fatalf("mailbox shard assignment is not deterministic")
	}
}

func TestFilterMailboxesForShardPartitionsAll(t *testing.T) {
	in := []store.Mailbox{
		{ID: "a1111111-1111-1111-1111-111111111111", Name: "a"},
		{ID: "b2222222-2222-2222-2222-222222222222", Name: "b"},
		{ID: "c3333333-3333-3333-3333-333333333333", Name: "c"},
		{ID: "d4444444-4444-4444-4444-444444444444", Name: "d"},
	}
	total := 0
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		out := filterMailboxesForShard(in, i, 3)
		total += len(out)
		for _, mb := range out {
			if seen[mb.ID] {
				t.Fatalf("mailbox %s assigned to multiple shards", mb.ID)
			}
			seen[mb.ID] = true
		}
	}
	if total != len(in) {
		t.Fatalf("expected all mailboxes assigned once across shards: got=%d want=%d", total, len(in))
	}
}
