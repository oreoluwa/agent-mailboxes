package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/router"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type autoDraftCompletionStore struct {
	*agentHarnessStore
	job        store.AutoDraftJob
	claimed    bool
	doneCalls  int
	retryCalls int
	runCount   int
}

func newAutoDraftCompletionStore() *autoDraftCompletionStore {
	harness := newAgentHarnessStore()
	harness.settings = model.MailboxAgentSettings{
		MailboxID:        "m1",
		EnabledTools:     []string{"create_draft"},
		MaxToolSteps:     1,
		ToolTimeoutSec:   45,
		PolicyVersion:    3,
		AutoDraftEnabled: true,
	}
	return &autoDraftCompletionStore{
		agentHarnessStore: harness,
		job: store.AutoDraftJob{
			ID:            "job-1",
			MailboxID:     "m1",
			MessageID:     "msg-1",
			PolicyVersion: 3,
			Attempts:      1,
			ClaimToken:    "claim-1",
		},
	}
}

func (s *autoDraftCompletionStore) ListAliases(context.Context, string) ([]router.Alias, error) {
	return []router.Alias{
		{ID: "alias-a", Email: "user+a@example.test", Enabled: true},
		{ID: "alias-b", Email: "user+b@example.test", Enabled: true},
	}, nil
}

func (s *autoDraftCompletionStore) ClaimAutoDraftJob(context.Context, string, time.Duration) (store.AutoDraftJob, bool, error) {
	if s.claimed {
		return store.AutoDraftJob{}, false, nil
	}
	s.claimed = true
	return s.job, true, nil
}

func (s *autoDraftCompletionStore) CreateAutoDraftRun(_ context.Context, mailboxID, messageID, jobID string, policyVersion int, status string, failureReason *string) (model.AutoDraftRun, error) {
	s.runCount++
	return model.AutoDraftRun{
		ID:            fmt.Sprintf("run-%d", s.runCount),
		MailboxID:     mailboxID,
		MessageID:     messageID,
		JobID:         jobID,
		PolicyVersion: policyVersion,
		Status:        status,
		FailureReason: failureReason,
	}, nil
}

func (s *autoDraftCompletionStore) RenewAutoDraftJobLease(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}

func (s *autoDraftCompletionStore) MarkAutoDraftJobDone(_ context.Context, id, claimToken string) (bool, error) {
	s.doneCalls++
	return true, nil
}

func (s *autoDraftCompletionStore) MarkAutoDraftJobRetry(_ context.Context, id, claimToken, lastErr string, retryAfter time.Duration) (bool, error) {
	s.retryCalls++
	s.job.Attempts++
	s.job.ClaimToken = fmt.Sprintf("claim-%d", s.job.Attempts)
	return true, nil
}

func (s *autoDraftCompletionStore) CreateAgentConversation(_ context.Context, claims model.TokenClaims, mailboxID string, sourceMessageID, threadRFCMessageID, createdBy *string) (model.AgentConversation, error) {
	if claims.AliasID == nil {
		return model.AgentConversation{}, store.ErrUnauthorized
	}
	for _, existing := range s.conversations {
		if existing.AliasID == nil || *existing.AliasID != *claims.AliasID {
			continue
		}
		if threadRFCMessageID != nil && existing.ThreadRFCMessageID != nil && *existing.ThreadRFCMessageID == *threadRFCMessageID {
			return existing, nil
		}
	}
	id := fmt.Sprintf("conv-%d", len(s.conversations)+1)
	aliasID := *claims.AliasID
	conv := model.AgentConversation{
		ID:                 id,
		MailboxID:          mailboxID,
		AliasID:            &aliasID,
		MessageID:          sourceMessageID,
		ThreadRFCMessageID: threadRFCMessageID,
		Status:             model.ConversationStatusRunning,
		CreatedBy:          createdBy,
		CreatedAt:          time.Now().UTC(),
		UpdatedAt:          time.Now().UTC(),
	}
	s.conversations[id] = conv
	return conv, nil
}

type perAliasRetryPlanner struct {
	calls      map[string]int
	failBOnce  bool
	bHasFailed bool
}

func (p *perAliasRetryPlanner) Next(_ context.Context, req AgentPlanRequest) (AgentPlanStep, error) {
	if req.Conversation.AliasID == nil {
		return AgentPlanStep{}, errors.New("planner received unscoped conversation")
	}
	aliasID := *req.Conversation.AliasID
	p.calls[aliasID]++
	if aliasID == "alias-b" && p.failBOnce && !p.bHasFailed {
		p.bHasFailed = true
		return AgentPlanStep{}, errors.New("temporary alias-b planner failure")
	}
	if req.Conversation.LastDraftID == nil {
		return AgentPlanStep{
			Kind:      "tool",
			ToolName:  "create_draft",
			Arguments: json.RawMessage(`{"body":"Ready for review"}`),
		}, nil
	}
	return AgentPlanStep{Kind: "complete", CompletionMessage: "done"}, nil
}

func TestFlushAutoDraftJobsPersistsAliasCompletionAcrossRetryAndRestart(t *testing.T) {
	harness := newAutoDraftCompletionStore()
	firstPlanner := &perAliasRetryPlanner{calls: map[string]int{}, failBOnce: true}
	firstService := &Service{
		Store:        harness,
		MailboxID:    "m1",
		MailboxName:  "primary",
		AgentPlanner: firstPlanner,
	}

	if err := firstService.flushAutoDraftJobs(context.Background(), 8); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if firstPlanner.calls["alias-a"] != 2 || firstPlanner.calls["alias-b"] != 1 {
		t.Fatalf("first planner calls = %#v, want alias-a=2 alias-b=1", firstPlanner.calls)
	}
	completed, err := harness.CompletedAutoDraftAliases(context.Background(), harness.job.ID)
	if err != nil {
		t.Fatalf("read first completion: %v", err)
	}
	if len(completed) != 1 || completed[0] != "alias-a" {
		t.Fatalf("completed aliases after first flush = %#v, want alias-a only", completed)
	}
	if harness.retryCalls != 1 || harness.doneCalls != 0 {
		t.Fatalf("first job transitions doneCalls=%d retryCalls=%d, want 0/1", harness.doneCalls, harness.retryCalls)
	}

	// Reconstruct the service and planner while retaining the same store. This
	// models a worker restart: alias A's durable completion must suppress a
	// second RunAgent invocation while alias B resumes and completes.
	secondPlanner := &perAliasRetryPlanner{calls: map[string]int{}}
	secondService := &Service{
		Store:        harness,
		MailboxID:    "m1",
		MailboxName:  "primary",
		AgentPlanner: secondPlanner,
	}
	harness.claimed = false
	if err := secondService.flushAutoDraftJobs(context.Background(), 8); err != nil {
		t.Fatalf("retry flush after restart: %v", err)
	}
	if secondPlanner.calls["alias-a"] != 0 || secondPlanner.calls["alias-b"] != 2 {
		t.Fatalf("retry planner calls = %#v, want alias-a=0 alias-b=2", secondPlanner.calls)
	}
	completed, err = harness.CompletedAutoDraftAliases(context.Background(), harness.job.ID)
	if err != nil {
		t.Fatalf("read final completions: %v", err)
	}
	completedSet := map[string]bool{}
	for _, aliasID := range completed {
		completedSet[aliasID] = true
	}
	if len(completedSet) != 2 || !completedSet["alias-a"] || !completedSet["alias-b"] {
		t.Fatalf("final completed aliases = %#v", completed)
	}
	if harness.retryCalls != 1 || harness.doneCalls != 1 {
		t.Fatalf("final job transitions doneCalls=%d retryCalls=%d, want 1/1", harness.doneCalls, harness.retryCalls)
	}
}
