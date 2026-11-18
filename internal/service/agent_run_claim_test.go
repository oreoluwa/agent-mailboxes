package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type runClaimHarnessStore struct {
	*agentHarnessStore
	mu         sync.Mutex
	runToken   string
	leaseUntil time.Time
	claimDelay time.Duration
	failCtxErr error
	failCalls  int
}

func (s *runClaimHarnessStore) ClaimAgentConversationRun(_ context.Context, _ model.TokenClaims, _, _ string, runToken string, lease time.Duration) (bool, error) {
	if s.claimDelay > 0 {
		time.Sleep(s.claimDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.runToken != "" && now.Before(s.leaseUntil) {
		return false, nil
	}
	s.runToken = runToken
	s.leaseUntil = now.Add(lease)
	return true, nil
}

func (s *runClaimHarnessStore) ReleaseAgentConversationRun(_ context.Context, _ model.TokenClaims, _, _, runToken string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runToken != runToken {
		return false, nil
	}
	s.runToken = ""
	s.leaseUntil = time.Time{}
	return true, nil
}

func (s *runClaimHarnessStore) FailAgentConversationRun(ctx context.Context, _ model.TokenClaims, _, conversationID, runToken string, lastDraftID *string, failure string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCalls++
	s.failCtxErr = ctx.Err()
	if s.runToken != runToken {
		return store.ErrAgentRunConflict
	}
	conv := s.conversations[conversationID]
	conv.Status = model.ConversationStatusFailed
	if lastDraftID != nil {
		id := *lastDraftID
		conv.LastDraftID = &id
	}
	now := time.Now().UTC()
	for i := range conv.ToolCalls {
		if conv.ToolCalls[i].Status == model.ToolCallStatusStarted {
			conv.ToolCalls[i].Status = model.ToolCallStatusFailed
			conv.ToolCalls[i].ErrorText = &failure
			conv.ToolCalls[i].CompletedAt = &now
		}
	}
	s.conversations[conversationID] = conv
	return nil
}

type blockingAgentPlanner struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
	once    sync.Once
}

func (p *blockingAgentPlanner) Next(ctx context.Context, _ AgentPlanRequest) (AgentPlanStep, error) {
	p.calls.Add(1)
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
		return AgentPlanStep{Kind: "complete"}, nil
	case <-ctx.Done():
		return AgentPlanStep{}, ctx.Err()
	}
}

func runClaimTestClaims() model.TokenClaims {
	aliasID := "alias-1"
	return model.TokenClaims{
		TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"),
		Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true},
	}
}

func newRunClaimService(planner AgentPlanner) (*Service, *runClaimHarnessStore) {
	h := &runClaimHarnessStore{agentHarnessStore: newAgentHarnessStore()}
	h.conversations["conv-1"] = model.AgentConversation{
		ID: "conv-1", MailboxID: "m1", AliasID: ptr("alias-1"),
		Status: model.ConversationStatusRunning, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	return &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner}, h
}

func TestRunAgentRejectsOverlappingConversationRunBeforeSecondPlannerCall(t *testing.T) {
	planner := &blockingAgentPlanner{entered: make(chan struct{}), release: make(chan struct{})}
	svc, _ := newRunClaimService(planner)
	conversationID := "conv-1"
	firstDone := make(chan error, 1)
	go func() {
		_, err := svc.RunAgent(context.Background(), runClaimTestClaims(), RunAgentInput{ConversationID: &conversationID, Prompt: "first"})
		firstDone <- err
	}()
	select {
	case <-planner.entered:
	case <-time.After(time.Second):
		t.Fatal("first planner call did not start")
	}

	_, err := svc.RunAgent(context.Background(), runClaimTestClaims(), RunAgentInput{ConversationID: &conversationID, Prompt: "second"})
	if !errors.Is(err, store.ErrAgentRunConflict) {
		t.Fatalf("overlapping RunAgent error = %v, want conflict", err)
	}
	if got := planner.calls.Load(); got != 1 {
		t.Fatalf("planner calls = %d, want 1", got)
	}
	close(planner.release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first run did not finish")
	}
}

func TestRunAgentTimeoutReleasesClaimAndStaleTokenCannotReleaseSuccessor(t *testing.T) {
	planner := &blockingAgentPlanner{entered: make(chan struct{}), release: make(chan struct{})}
	svc, h := newRunClaimService(planner)
	svc.AgentRunTimeout = 20 * time.Millisecond
	conversationID := "conv-1"
	_, err := svc.RunAgent(context.Background(), runClaimTestClaims(), RunAgentInput{ConversationID: &conversationID, Prompt: "timeout"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed out RunAgent error = %v, want deadline exceeded", err)
	}
	if h.failCalls != 1 || h.failCtxErr != nil {
		t.Fatalf("failure cleanup calls=%d ctxErr=%v, want one call with live bounded context", h.failCalls, h.failCtxErr)
	}
	if got := h.conversations[conversationID].Status; got != model.ConversationStatusFailed {
		t.Fatalf("conversation status after canceled run = %q, want failed", got)
	}

	claimed, err := h.ClaimAgentConversationRun(context.Background(), runClaimTestClaims(), "m1", conversationID, "successor", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim after timeout = %v, %v", claimed, err)
	}
	released, err := h.ReleaseAgentConversationRun(context.Background(), runClaimTestClaims(), "m1", conversationID, "stale-token")
	if err != nil || released {
		t.Fatalf("stale release = %v, %v; want false, nil", released, err)
	}
	claimed, err = h.ClaimAgentConversationRun(context.Background(), runClaimTestClaims(), "m1", conversationID, "third", time.Minute)
	if err != nil || claimed {
		t.Fatalf("stale release unlocked successor: claimed=%v err=%v", claimed, err)
	}
}

func TestRunAgentCountsClaimLatencyAgainstDeadline(t *testing.T) {
	planner := &blockingAgentPlanner{entered: make(chan struct{}), release: make(chan struct{})}
	svc, h := newRunClaimService(planner)
	h.claimDelay = 30 * time.Millisecond
	svc.AgentRunTimeout = 10 * time.Millisecond
	conversationID := "conv-1"
	_, err := svc.RunAgent(context.Background(), runClaimTestClaims(), RunAgentInput{ConversationID: &conversationID, Prompt: "must not append"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delayed claim RunAgent error = %v, want deadline exceeded", err)
	}
	if got := planner.calls.Load(); got != 0 {
		t.Fatalf("planner calls after deadline = %d, want 0", got)
	}
	if got := len(h.conversations[conversationID].Messages); got != 0 {
		t.Fatalf("messages appended after deadline = %d, want 0", got)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runToken != "" {
		t.Fatalf("delayed successful claim was not released: %q", h.runToken)
	}
}
