package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type sendDraftHarnessStore struct {
	*fakeStore
	result        store.DraftSendResult
	sendInput     store.SendDraftInput
	sendCalls     int
	aliasEmail    string
	aliasEmailErr error
	sendErr       error
}

func (s *sendDraftHarnessStore) GetAliasEmailByID(ctx context.Context, aliasID string) (string, error) {
	if s.aliasEmailErr != nil {
		return "", s.aliasEmailErr
	}
	return s.aliasEmail, nil
}

func (s *sendDraftHarnessStore) SendDraft(ctx context.Context, claims model.TokenClaims, input store.SendDraftInput) (store.DraftSendResult, error) {
	s.sendCalls++
	s.sendInput = input
	return s.result, s.sendErr
}

type autoDraftHarnessStore struct {
	*fakeStore
	settings      []model.MailboxAgentSettings
	settingsCalls int
	jobs          []store.AutoDraftJob
	runs          []model.AutoDraftRun
	runUpdates    []runStatusUpdate
	doneIDs       []string
	retryIDs      []string
	retryMessage  string
	retryAfter    time.Duration
	createRunErr  error
	doneErr       error
	retryErr      error
}

type runStatusUpdate struct {
	runID         string
	status        string
	failureReason *string
}

func (s *autoDraftHarnessStore) ClaimAutoDraftJob(ctx context.Context, mailboxID string, lease time.Duration) (store.AutoDraftJob, bool, error) {
	if len(s.jobs) == 0 {
		return store.AutoDraftJob{}, false, nil
	}
	job := s.jobs[0]
	s.jobs = s.jobs[1:]
	if job.ClaimToken == "" {
		job.ClaimToken = "claim-1"
	}
	return job, true, nil
}

func (s *autoDraftHarnessStore) CreateAutoDraftRun(ctx context.Context, mailboxID, messageID, jobID string, policyVersion int, status string, failureReason *string) (model.AutoDraftRun, error) {
	if s.createRunErr != nil {
		return model.AutoDraftRun{}, s.createRunErr
	}
	run := model.AutoDraftRun{
		ID:            fmt.Sprintf("run-%d", len(s.runs)+1),
		MailboxID:     mailboxID,
		MessageID:     messageID,
		JobID:         jobID,
		PolicyVersion: policyVersion,
		Status:        status,
		FailureReason: failureReason,
	}
	s.runs = append(s.runs, run)
	return run, nil
}

func (s *autoDraftHarnessStore) GetMailboxAgentSettings(ctx context.Context, mailboxID string) (model.MailboxAgentSettings, error) {
	idx := s.settingsCalls
	s.settingsCalls++
	if len(s.settings) == 0 {
		return model.MailboxAgentSettings{}, errors.New("missing settings")
	}
	if idx >= len(s.settings) {
		idx = len(s.settings) - 1
	}
	return s.settings[idx], nil
}

func (s *autoDraftHarnessStore) UpdateAutoDraftRunStatus(ctx context.Context, runID string, status string, failureReason *string) error {
	s.runUpdates = append(s.runUpdates, runStatusUpdate{runID: runID, status: status, failureReason: failureReason})
	return nil
}

func (s *autoDraftHarnessStore) RenewAutoDraftJobLease(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}

func (s *autoDraftHarnessStore) MarkAutoDraftJobDone(ctx context.Context, id, claimToken string) (bool, error) {
	s.doneIDs = append(s.doneIDs, id)
	return s.doneErr == nil, s.doneErr
}

func (s *autoDraftHarnessStore) MarkAutoDraftJobRetry(ctx context.Context, id, claimToken, lastErr string, retryAfter time.Duration) (bool, error) {
	s.retryIDs = append(s.retryIDs, id)
	s.retryMessage = lastErr
	s.retryAfter = retryAfter
	return s.retryErr == nil, s.retryErr
}

func TestSendFromDraftRejectsClaimsWithoutAlias(t *testing.T) {
	svc := &Service{
		Store:     &sendDraftHarnessStore{fakeStore: newFakeStore()},
		MailboxID: "m1",
	}

	_, err := svc.SendFromDraft(context.Background(), model.TokenClaims{
		TokenID:   "tok-1",
		MailboxID: ptr("m1"),
		Scopes:    map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true},
	}, "draft-1", "revision-1", nil)
	if err != store.ErrUnauthorized {
		t.Fatalf("expected unauthorized without alias, got %v", err)
	}
}

func TestFlushAutoDraftJobsBlocksStalePolicyVersion(t *testing.T) {
	h := &autoDraftHarnessStore{
		fakeStore: newFakeStore(),
		jobs:      []store.AutoDraftJob{{ID: "job-1", MessageID: "msg-1", PolicyVersion: 1}},
		settings: []model.MailboxAgentSettings{{
			MailboxID: "m1", AutoDraftEnabled: true, PolicyVersion: 2,
		}},
	}
	svc := &Service{Store: h, MailboxID: "m1", AgentPlanner: &scriptedPlanner{}}

	if err := svc.flushAutoDraftJobs(context.Background(), 1); err != nil {
		t.Fatalf("flush auto-draft jobs: %v", err)
	}
	if len(h.doneIDs) != 1 || h.doneIDs[0] != "job-1" {
		t.Fatalf("expected stale job to be retired, got done=%v", h.doneIDs)
	}
	if len(h.retryIDs) != 0 {
		t.Fatalf("expected no retry for stale policy, got %v", h.retryIDs)
	}
	if len(h.runUpdates) != 1 || h.runUpdates[0].status != "blocked" {
		t.Fatalf("expected blocked run update, got %+v", h.runUpdates)
	}
}

func TestSendFromDraftReturnsIdempotentStoreResult(t *testing.T) {
	aliasID := "alias-1"
	h := &sendDraftHarnessStore{
		fakeStore:  newFakeStore(),
		aliasEmail: "alias@example.com",
		result: store.DraftSendResult{
			Draft: model.Draft{
				ID: "draft-1",
				LatestRevision: model.DraftRevision{
					Subject: "Re: Hello",
				},
			},
			Outbound:   store.OutboundMessage{ID: "out-1"},
			Idempotent: true,
		},
	}
	svc := &Service{
		Store:     h,
		MailboxID: "m1",
	}

	result, err := svc.SendFromDraft(context.Background(), model.TokenClaims{
		TokenID:   "tok-1",
		AliasID:   &aliasID,
		MailboxID: ptr("m1"),
		Scopes:    map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true},
	}, " draft-1 ", " revision-1 ", &model.VerifiedActor{UserID: "user-1"})
	if err != nil {
		t.Fatalf("send from draft: %v", err)
	}
	if !result.Idempotent {
		t.Fatalf("expected idempotent result")
	}
	if h.sendCalls != 1 {
		t.Fatalf("expected one send call, got %d", h.sendCalls)
	}
	if h.sendInput.DraftID != "draft-1" {
		t.Fatalf("expected trimmed draft id, got %q", h.sendInput.DraftID)
	}
	if h.sendInput.ReviewedRevisionID != "revision-1" {
		t.Fatalf("expected trimmed reviewed revision id, got %q", h.sendInput.ReviewedRevisionID)
	}
	if h.sendInput.FromEmail != "alias@example.com" {
		t.Fatalf("unexpected from address: %q", h.sendInput.FromEmail)
	}
}

func TestDraftMutationsRejectSendOnlyClaimsBeforeStoreAccess(t *testing.T) {
	aliasID := "alias-1"
	claims := model.TokenClaims{
		TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"),
		Scopes: map[model.Scope]bool{model.ScopeSend: true},
	}
	svc := &Service{MailboxID: "m1"}
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "create", run: func() error {
			_, err := svc.CreateDraft(context.Background(), claims, store.CreateDraftInput{})
			return err
		}},
		{name: "revise", run: func() error {
			_, err := svc.ReviseDraft(context.Background(), claims, store.ReviseDraftInput{DraftID: "draft-1"})
			return err
		}},
		{name: "discard", run: func() error {
			_, err := svc.DiscardDraft(context.Background(), claims, "draft-1", nil)
			return err
		}},
		{name: "send", run: func() error {
			_, err := svc.SendFromDraft(context.Background(), claims, "draft-1", "revision-1", nil)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("mutation error = %v, want unauthorized", err)
			}
		})
	}
}

func TestUpdateMailboxAgentSettingsRejectsAliasScopedAdmin(t *testing.T) {
	aliasID := "alias-1"
	svc := &Service{Store: newFakeStore(), MailboxID: "m1"}
	_, err := svc.UpdateMailboxAgentSettings(context.Background(), model.TokenClaims{
		AliasID: &aliasID,
		Scopes:  map[model.Scope]bool{model.ScopeAdmin: true},
	}, store.UpdateMailboxAgentSettingsInput{})
	if !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("expected alias-scoped admin rejection, got %v", err)
	}
}

func TestRunAgentFailsWhenPlannerRequestsUnsupportedSendTool(t *testing.T) {
	h := newAgentHarnessStore()
	h.settings.EnabledTools = []string{"send_message"}
	planner := &scriptedPlanner{
		steps: []AgentPlanStep{
			{Kind: "tool", ToolName: "send_message", Arguments: json.RawMessage(`{"subject":"hi"}`)},
		},
	}
	aliasID := "alias-1"
	svc := &Service{
		Store:        h,
		MailboxID:    "m1",
		MailboxName:  "primary",
		AgentPlanner: planner,
	}
	msgID := "msg-1"

	_, err := svc.RunAgent(context.Background(), model.TokenClaims{
		TokenID:   "tok-1",
		AliasID:   &aliasID,
		MailboxID: ptr("m1"),
		Scopes:    map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true},
	}, RunAgentInput{
		MessageID: &msgID,
		Prompt:    "Try to send directly.",
	})
	if err == nil || err.Error() != `unknown tool "send_message"` {
		t.Fatalf("expected unsupported send tool failure, got %v", err)
	}
	conv := h.conversations["conv-1"]
	if conv.Status != model.ConversationStatusFailed {
		t.Fatalf("expected failed conversation, got %s", conv.Status)
	}
}

func TestFlushAutoDraftJobsMarksDisabledSettingsBlocked(t *testing.T) {
	h := &autoDraftHarnessStore{
		fakeStore: newFakeStore(),
		settings: []model.MailboxAgentSettings{
			{MailboxID: "m1", AutoDraftEnabled: true, PolicyVersion: 7},
			{MailboxID: "m1", AutoDraftEnabled: false, PolicyVersion: 7},
		},
		jobs: []store.AutoDraftJob{{
			ID:            "job-1",
			MailboxID:     "m1",
			MessageID:     "msg-1",
			PolicyVersion: 7,
		}},
	}
	svc := &Service{
		Store:        h,
		MailboxID:    "m1",
		AgentPlanner: &scriptedPlanner{},
	}

	if err := svc.flushAutoDraftJobs(context.Background(), 8); err != nil {
		t.Fatalf("flush auto draft jobs: %v", err)
	}
	if len(h.doneIDs) != 1 || h.doneIDs[0] != "job-1" {
		t.Fatalf("expected disabled job to be marked done, got %#v", h.doneIDs)
	}
	if len(h.retryIDs) != 0 {
		t.Fatalf("expected no retry ids, got %#v", h.retryIDs)
	}
	if len(h.runUpdates) != 1 || h.runUpdates[0].status != "blocked" {
		t.Fatalf("expected blocked run update, got %#v", h.runUpdates)
	}
	if h.runUpdates[0].failureReason == nil || *h.runUpdates[0].failureReason != "auto draft disabled" {
		t.Fatalf("expected disabled failure reason, got %#v", h.runUpdates[0].failureReason)
	}
}

func TestFlushAutoDraftJobsRetriesAgentFailures(t *testing.T) {
	h := &autoDraftHarnessStore{
		fakeStore: newFakeStore(),
		settings: []model.MailboxAgentSettings{
			{MailboxID: "m1", AutoDraftEnabled: true, PolicyVersion: 3, EnabledTools: []string{"create_draft"}, MaxToolSteps: 1},
			{MailboxID: "m1", AutoDraftEnabled: true, PolicyVersion: 3, EnabledTools: []string{"create_draft"}, MaxToolSteps: 1},
		},
		jobs: []store.AutoDraftJob{{
			ID:            "job-1",
			MailboxID:     "m1",
			MessageID:     "msg-1",
			PolicyVersion: 3,
			Attempts:      1,
		}},
	}
	svc := &Service{
		Store:        h,
		MailboxID:    "m1",
		MailboxName:  "primary",
		AgentPlanner: nil,
	}

	if err := svc.flushAutoDraftJobs(context.Background(), 8); err != nil {
		t.Fatalf("flush auto draft jobs: %v", err)
	}
	if len(h.doneIDs) != 0 {
		t.Fatalf("expected no done ids on retryable failure, got %#v", h.doneIDs)
	}
	if len(h.retryIDs) != 1 || h.retryIDs[0] != "job-1" {
		t.Fatalf("expected retry for failed job, got %#v", h.retryIDs)
	}
	if len(h.runUpdates) != 1 || h.runUpdates[0].status != "failed" {
		t.Fatalf("expected failed run update, got %#v", h.runUpdates)
	}
	if h.runUpdates[0].failureReason == nil || *h.runUpdates[0].failureReason != "agent planner is not configured" {
		t.Fatalf("unexpected failure reason: %#v", h.runUpdates[0].failureReason)
	}
	if h.retryMessage != "auto draft retry scheduled" {
		t.Fatalf("unexpected retry message: %q", h.retryMessage)
	}
}
