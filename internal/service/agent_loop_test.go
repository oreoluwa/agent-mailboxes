package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type scriptedPlanner struct {
	steps []AgentPlanStep
	idx   int
}

func (p *scriptedPlanner) Next(ctx context.Context, req AgentPlanRequest) (AgentPlanStep, error) {
	if p.idx >= len(p.steps) {
		return AgentPlanStep{Kind: "complete", CompletionMessage: "done"}, nil
	}
	step := p.steps[p.idx]
	p.idx++
	return step, nil
}

type agentHarnessStore struct {
	*fakeStore
	settings                model.MailboxAgentSettings
	conversations           map[string]model.AgentConversation
	drafts                  map[string]model.Draft
	toolCalls               map[string]model.AgentToolCall
	sourceMessage           model.Message
	sourceBody              model.Body
	replyTo                 string
	completeErr             error
	completedConversationID string
	completedDraftID        string
	completedSourceID       *string
	completedMessage        string
	failReloadAfterComplete bool
	finishToolErr           error
	toolMessageErr          error
}

func newAgentHarnessStore() *agentHarnessStore {
	return &agentHarnessStore{
		fakeStore: newFakeStore(),
		settings: model.MailboxAgentSettings{
			MailboxID:        "m1",
			EnabledTools:     []string{"read_message", "create_draft", "revise_draft"},
			MaxToolSteps:     4,
			ToolTimeoutSec:   45,
			PolicyVersion:    1,
			AutoDraftEnabled: false,
		},
		conversations: map[string]model.AgentConversation{},
		drafts:        map[string]model.Draft{},
		toolCalls:     map[string]model.AgentToolCall{},
		sourceMessage: model.Message{
			ID:           "msg-1",
			RFCMessageID: "<msg-1@test>",
			Subject:      ptr("Original Subject"),
			FromEmail:    "sender@example.com",
			InternalDate: time.Now().UTC(),
		},
		sourceBody: model.Body{
			MessageID:     "msg-1",
			TextPlain:     ptr("hello"),
			ExtractedText: ptr("hello"),
		},
		replyTo: "sender@example.com",
	}
}

func testActorUserID(actor *model.VerifiedActor) *string {
	if actor == nil || actor.UserID == "" {
		return nil
	}
	v := actor.UserID
	return &v
}

func testActorRequestID(actor *model.VerifiedActor) *string {
	if actor == nil || actor.RequestID == "" {
		return nil
	}
	v := actor.RequestID
	return &v
}

func testActorPermission(actor *model.VerifiedActor) *string {
	if actor == nil || actor.PermissionIntent == "" {
		return nil
	}
	v := actor.PermissionIntent
	return &v
}

func (s *agentHarnessStore) ReadMessage(ctx context.Context, claims model.TokenClaims, messageID string) (model.Message, error) {
	return s.sourceMessage, nil
}

func (s *agentHarnessStore) GetBody(ctx context.Context, claims model.TokenClaims, messageID string) (model.Body, error) {
	return s.sourceBody, nil
}

func (s *agentHarnessStore) GetMessageThreadMeta(ctx context.Context, claims model.TokenClaims, messageID string) (inReplyTo *string, references *string, subject *string, err error) {
	inReply := "<msg-1@test>"
	refs := "<msg-0@test> <msg-1@test>"
	return &inReply, &refs, s.sourceMessage.Subject, nil
}

func (s *agentHarnessStore) GetMessageReplyTo(ctx context.Context, claims model.TokenClaims, messageID string) (string, error) {
	return s.replyTo, nil
}

func (s *agentHarnessStore) CreateAgentConversation(ctx context.Context, claims model.TokenClaims, mailboxID string, sourceMessageID *string, threadRFCMessageID *string, createdBy *string) (model.AgentConversation, error) {
	if threadRFCMessageID != nil {
		for _, existing := range s.conversations {
			if existing.ThreadRFCMessageID != nil && *existing.ThreadRFCMessageID == *threadRFCMessageID {
				return existing, nil
			}
		}
	}
	id := fmt.Sprintf("conv-%d", len(s.conversations)+1)
	conv := model.AgentConversation{
		ID:                 id,
		MailboxID:          mailboxID,
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

func (s *agentHarnessStore) GetAgentConversation(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID string) (model.AgentConversation, error) {
	if s.failReloadAfterComplete && s.completedConversationID != "" {
		return model.AgentConversation{}, errors.New("reload failed")
	}
	conv := s.conversations[conversationID]
	return conv, nil
}

func (s *agentHarnessStore) CompleteAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken, draftID string, sourceMessageID *string, completionMessage string) error {
	s.completedConversationID = conversationID
	s.completedDraftID = draftID
	s.completedSourceID = normalizedOptionalID(sourceMessageID)
	s.completedMessage = completionMessage
	if s.completeErr != nil {
		return s.completeErr
	}
	conv := s.conversations[conversationID]
	conv.Status = model.ConversationStatusCompleted
	conv.LastDraftID = &draftID
	if completionMessage != "" {
		conv.Messages = append(conv.Messages, model.AgentConversationTurn{
			ID: fmt.Sprintf("turn-%d", len(conv.Messages)+1), ConversationID: conversationID,
			Role: "assistant", Content: completionMessage, CreatedAt: time.Now().UTC(),
		})
	}
	s.conversations[conversationID] = conv
	return nil
}

func (s *agentHarnessStore) FailAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken string, lastDraftID *string, failure string) error {
	conv := s.conversations[conversationID]
	if conv.Status == model.ConversationStatusCompleted {
		return store.ErrAgentRunConflict
	}
	conv.Status = model.ConversationStatusFailed
	if lastDraftID != nil {
		id := strings.TrimSpace(*lastDraftID)
		conv.LastDraftID = &id
	}
	now := time.Now().UTC()
	for i := range conv.ToolCalls {
		if conv.ToolCalls[i].Status == model.ToolCallStatusStarted {
			conv.ToolCalls[i].Status = model.ToolCallStatusFailed
			conv.ToolCalls[i].ErrorText = &failure
			conv.ToolCalls[i].CompletedAt = &now
			call := conv.ToolCalls[i]
			s.toolCalls[call.ID] = call
		}
	}
	s.conversations[conversationID] = conv
	return nil
}

func (s *agentHarnessStore) ListAgentConversations(ctx context.Context, claims model.TokenClaims, mailboxID string, options store.AgentConversationListOptions) ([]model.AgentConversation, error) {
	out := make([]model.AgentConversation, 0, len(s.conversations))
	for _, conv := range s.conversations {
		if options.ThreadRFCMessageID != nil && (conv.ThreadRFCMessageID == nil || *conv.ThreadRFCMessageID != *options.ThreadRFCMessageID) {
			continue
		}
		out = append(out, conv)
	}
	return out, nil
}

func (s *agentHarnessStore) AddAgentConversationMessage(ctx context.Context, claims model.TokenClaims, mailboxID string, conversationID, role, content string, payload json.RawMessage) (model.AgentConversationTurn, error) {
	if role == "tool" && s.toolMessageErr != nil {
		return model.AgentConversationTurn{}, s.toolMessageErr
	}
	conv := s.conversations[conversationID]
	turn := model.AgentConversationTurn{
		ID:             fmt.Sprintf("turn-%d", len(conv.Messages)+1),
		ConversationID: conversationID,
		Role:           role,
		Content:        content,
		Payload:        payload,
		CreatedAt:      time.Now().UTC(),
	}
	conv.Messages = append(conv.Messages, turn)
	conv.UpdatedAt = time.Now().UTC()
	s.conversations[conversationID] = conv
	return turn, nil
}

func (s *agentHarnessStore) StartAgentToolCall(ctx context.Context, claims model.TokenClaims, mailboxID string, conversationID, toolName string, argsSummary json.RawMessage, policyDecision string) (model.AgentToolCall, error) {
	call := model.AgentToolCall{
		ID:             fmt.Sprintf("tool-%d", len(s.toolCalls)+1),
		ConversationID: conversationID,
		ToolName:       toolName,
		Status:         model.ToolCallStatusStarted,
		ArgsSummary:    argsSummary,
		PolicyDecision: policyDecision,
		StartedAt:      time.Now().UTC(),
	}
	s.toolCalls[call.ID] = call
	conv := s.conversations[conversationID]
	conv.ToolCalls = append(conv.ToolCalls, call)
	s.conversations[conversationID] = conv
	return call, nil
}

func (s *agentHarnessStore) FinishAgentToolCall(ctx context.Context, claims model.TokenClaims, mailboxID string, toolCallID string, status model.ToolCallStatus, resultSummary json.RawMessage, errText *string) (model.AgentToolCall, error) {
	if s.finishToolErr != nil {
		return model.AgentToolCall{}, s.finishToolErr
	}
	call := s.toolCalls[toolCallID]
	call.Status = status
	call.ResultSummary = resultSummary
	call.ErrorText = errText
	now := time.Now().UTC()
	call.CompletedAt = &now
	s.toolCalls[toolCallID] = call
	conv := s.conversations[call.ConversationID]
	for i := range conv.ToolCalls {
		if conv.ToolCalls[i].ID == call.ID {
			conv.ToolCalls[i] = call
		}
	}
	s.conversations[call.ConversationID] = conv
	return call, nil
}

func (s *agentHarnessStore) SetAgentConversationStatus(ctx context.Context, claims model.TokenClaims, mailboxID string, conversationID string, status model.ConversationStatus, lastDraftID *string) error {
	conv := s.conversations[conversationID]
	conv.Status = status
	conv.LastDraftID = lastDraftID
	s.conversations[conversationID] = conv
	return nil
}

func (s *agentHarnessStore) GetMailboxAgentSettings(ctx context.Context, mailboxID string) (model.MailboxAgentSettings, error) {
	return s.settings, nil
}

func (s *agentHarnessStore) CreateDraft(ctx context.Context, claims model.TokenClaims, input store.CreateDraftInput) (model.Draft, error) {
	id := fmt.Sprintf("draft-%d", len(s.drafts)+1)
	revision := model.DraftRevision{
		ID:               "rev-1",
		DraftID:          id,
		RevisionNumber:   1,
		Provenance:       input.Provenance,
		Subject:          input.Subject,
		To:               input.To,
		Body:             input.Body,
		InReplyTo:        input.InReplyTo,
		References:       input.References,
		ActorUserID:      testActorUserID(input.Actor),
		RequestID:        testActorRequestID(input.Actor),
		PermissionIntent: testActorPermission(input.Actor),
		CreatedAt:        time.Now().UTC(),
	}
	draft := model.Draft{
		ID:               id,
		MailboxID:        input.MailboxID,
		AliasID:          claims.AliasID,
		ConversationID:   input.ConversationID,
		SourceMessageID:  input.SourceMessageID,
		Status:           model.DraftStatusPendingReview,
		LatestRevisionID: revision.ID,
		LatestRevision:   revision,
		Revisions:        []model.DraftRevision{revision},
		ActorUserID:      testActorUserID(input.Actor),
		LastRequestID:    testActorRequestID(input.Actor),
		PermissionIntent: testActorPermission(input.Actor),
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	s.drafts[id] = draft
	return draft, nil
}

func (s *agentHarnessStore) ReviseDraft(ctx context.Context, claims model.TokenClaims, input store.ReviseDraftInput) (model.Draft, error) {
	draft := s.drafts[input.DraftID]
	revision := model.DraftRevision{
		ID:               fmt.Sprintf("rev-%d", len(draft.Revisions)+1),
		DraftID:          input.DraftID,
		RevisionNumber:   len(draft.Revisions) + 1,
		Provenance:       input.Provenance,
		Subject:          input.Subject,
		To:               input.To,
		Body:             input.Body,
		InReplyTo:        input.InReplyTo,
		References:       input.References,
		ActorUserID:      testActorUserID(input.Actor),
		RequestID:        testActorRequestID(input.Actor),
		PermissionIntent: testActorPermission(input.Actor),
		CreatedAt:        time.Now().UTC(),
	}
	draft.Revisions = append(draft.Revisions, revision)
	draft.LatestRevision = revision
	draft.LatestRevisionID = revision.ID
	draft.ActorUserID = testActorUserID(input.Actor)
	draft.LastRequestID = testActorRequestID(input.Actor)
	draft.PermissionIntent = testActorPermission(input.Actor)
	draft.UpdatedAt = time.Now().UTC()
	s.drafts[input.DraftID] = draft
	return draft, nil
}

func (s *agentHarnessStore) GetDraft(ctx context.Context, claims model.TokenClaims, mailboxID, draftID string) (model.Draft, error) {
	return s.drafts[draftID], nil
}

func TestRunAgentCreatesReviewedDraftAndPersistsLedger(t *testing.T) {
	h := newAgentHarnessStore()
	planner := &scriptedPlanner{
		steps: []AgentPlanStep{
			{Kind: "tool", ToolName: "read_message", Arguments: json.RawMessage(`{"message_id":"msg-1"}`)},
			{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"Thanks for reaching out. We will review and reply shortly."}`)},
			{Kind: "complete", CompletionMessage: "Draft is ready for review."},
		},
	}
	aliasID := "alias-1"
	svc := &Service{
		Store:        h,
		MailboxID:    "m1",
		MailboxName:  "primary",
		AgentPlanner: planner,
	}
	claims := model.TokenClaims{
		TokenID:   "tok-1",
		AliasID:   &aliasID,
		MailboxID: ptr("m1"),
		Scopes:    map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true},
	}
	msgID := "msg-1"
	actor := &model.VerifiedActor{
		UserID:           "user-1",
		MailboxID:        "m1",
		RequestID:        "req-1",
		PermissionIntent: "send",
		Action:           "run_agent",
		TargetID:         msgID,
		Nonce:            "nonce-1",
		VerifiedAt:       time.Now().UTC(),
	}
	result, err := svc.RunAgent(context.Background(), claims, RunAgentInput{
		MessageID: &msgID,
		Prompt:    "Create a helpful reply.",
		Actor:     actor,
	})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if result.Draft == nil {
		t.Fatalf("expected draft result")
	}
	if got := result.Draft.LatestRevision.Subject; got != "Re: Original Subject" {
		t.Fatalf("unexpected draft subject: %q", got)
	}
	if got := result.Draft.LatestRevision.To; len(got) != 1 || got[0] != "sender@example.com" {
		t.Fatalf("unexpected recipients: %#v", got)
	}
	if result.Conversation.Status != model.ConversationStatusCompleted {
		t.Fatalf("expected completed conversation, got %s", result.Conversation.Status)
	}
	if result.Conversation.CreatedBy == nil || *result.Conversation.CreatedBy != "user-1" {
		t.Fatalf("expected verified actor to own conversation, got %+v", result.Conversation.CreatedBy)
	}
	if len(result.Conversation.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(result.Conversation.ToolCalls))
	}
	if result.Draft.ActorUserID == nil || *result.Draft.ActorUserID != "user-1" {
		t.Fatalf("expected verified actor on draft, got %+v", result.Draft.ActorUserID)
	}
	if result.Draft.LastRequestID == nil || *result.Draft.LastRequestID != "req-1" {
		t.Fatalf("expected request metadata on draft, got %+v", result.Draft.LastRequestID)
	}
	if result.Draft.LatestRevision.ActorUserID == nil || *result.Draft.LatestRevision.ActorUserID != "user-1" {
		t.Fatalf("expected verified actor on draft revision, got %+v", result.Draft.LatestRevision.ActorUserID)
	}
}

func TestRunAgentWithoutPlannerReturnsUnavailableBeforeStoreAccess(t *testing.T) {
	svc := &Service{AgentPlanner: nil}
	_, err := svc.RunAgent(context.Background(), model.TokenClaims{
		Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true},
	}, RunAgentInput{Prompt: "draft a reply"})
	if err == nil || err.Error() != "agent planner is not configured" {
		t.Fatalf("RunAgent() err = %v, want planner unavailable", err)
	}
}

func TestRunAgentRejectsSendOnlyClaimsBeforePlannerOrStoreAccess(t *testing.T) {
	svc := &Service{AgentPlanner: &scriptedPlanner{steps: []AgentPlanStep{{Kind: "complete"}}}}
	_, err := svc.RunAgent(context.Background(), model.TokenClaims{
		Scopes: map[model.Scope]bool{model.ScopeSend: true},
	}, RunAgentInput{Prompt: "draft a reply"})
	if err != store.ErrUnauthorized {
		t.Fatalf("RunAgent() err = %v, want unauthorized", err)
	}
}

func TestRunAgentRejectsDisallowedTool(t *testing.T) {
	h := newAgentHarnessStore()
	h.settings.EnabledTools = []string{"read_message"}
	planner := &scriptedPlanner{
		steps: []AgentPlanStep{
			{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"Nope"}`)},
		},
	}
	aliasID := "alias-1"
	svc := &Service{
		Store:        h,
		MailboxID:    "m1",
		MailboxName:  "primary",
		AgentPlanner: planner,
	}
	claims := model.TokenClaims{
		TokenID:   "tok-1",
		AliasID:   &aliasID,
		MailboxID: ptr("m1"),
		Scopes:    map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true},
	}
	msgID := "msg-1"
	_, err := svc.RunAgent(context.Background(), claims, RunAgentInput{
		MessageID: &msgID,
		Prompt:    "Create a reply.",
		Actor: &model.VerifiedActor{
			UserID:           "user-1",
			MailboxID:        "m1",
			RequestID:        "req-1",
			PermissionIntent: "send",
			Action:           "run_agent",
			TargetID:         msgID,
			Nonce:            "nonce-1",
		},
	})
	if err == nil || err.Error() != `tool "create_draft" is not allowed` {
		t.Fatalf("expected allowlist rejection, got %v", err)
	}
}

func TestRunAgentEmptyToolPolicyDeniesAllTools(t *testing.T) {
	h := newAgentHarnessStore()
	h.settings.EnabledTools = []string{}
	planner := &scriptedPlanner{steps: []AgentPlanStep{{
		Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"Nope"}`),
	}}}
	aliasID := "alias-1"
	svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner}
	claims := model.TokenClaims{TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}

	_, err := svc.RunAgent(context.Background(), claims, RunAgentInput{Prompt: "Create a reply."})
	if err == nil || err.Error() != `tool "create_draft" is not allowed` {
		t.Fatalf("RunAgent() err = %v, want empty-policy rejection", err)
	}
	if len(h.toolCalls) != 0 || len(h.drafts) != 0 {
		t.Fatalf("empty policy mutated tool calls/drafts: %d/%d", len(h.toolCalls), len(h.drafts))
	}
}

func TestRunAgentAllowsCompletionAfterToolBudgetIsExhausted(t *testing.T) {
	h := newAgentHarnessStore()
	h.settings.EnabledTools = []string{"create_draft"}
	h.settings.MaxToolSteps = 1
	planner := &scriptedPlanner{steps: []AgentPlanStep{
		{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"Ready"}`)},
		{Kind: "complete", CompletionMessage: "Draft is ready."},
	}}
	aliasID := "alias-1"
	svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner}
	claims := model.TokenClaims{TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}

	result, err := svc.RunAgent(context.Background(), claims, RunAgentInput{Prompt: "Create a reply."})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if result.Draft == nil || result.Conversation.Status != model.ConversationStatusCompleted {
		t.Fatalf("RunAgent() = %+v, want completed draft", result)
	}
	if len(h.toolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(h.toolCalls))
	}
}

func TestRunAgentRejectsToolBeyondBudgetWithoutLosingDraft(t *testing.T) {
	h := newAgentHarnessStore()
	h.settings.EnabledTools = []string{"create_draft", "revise_draft"}
	h.settings.MaxToolSteps = 1
	planner := &scriptedPlanner{steps: []AgentPlanStep{
		{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"Ready"}`)},
		{Kind: "tool", ToolName: "revise_draft", Arguments: json.RawMessage(`{"body":"Too late"}`)},
	}}
	aliasID := "alias-1"
	svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner}
	claims := model.TokenClaims{TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}

	_, err := svc.RunAgent(context.Background(), claims, RunAgentInput{Prompt: "Create a reply."})
	if err == nil || err.Error() != "agent exceeded max tool steps" {
		t.Fatalf("RunAgent() err = %v, want tool budget rejection", err)
	}
	if len(h.toolCalls) != 1 || len(h.drafts) != 1 {
		t.Fatalf("over-budget tool executed: tool calls/drafts = %d/%d", len(h.toolCalls), len(h.drafts))
	}
	conv := h.conversations["conv-1"]
	if conv.Status != model.ConversationStatusFailed || conv.LastDraftID == nil || *conv.LastDraftID != "draft-1" {
		t.Fatalf("failed conversation lost draft: %+v", conv)
	}
}

func TestRunAgentReusesConversationForMessage(t *testing.T) {
	h := newAgentHarnessStore()
	planner := &scriptedPlanner{steps: []AgentPlanStep{
		{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"First"}`)},
		{Kind: "complete", CompletionMessage: "done"},
		{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"Second"}`)},
		{Kind: "complete", CompletionMessage: "done again"},
	}}
	aliasID := "alias-1"
	svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner}
	claims := model.TokenClaims{TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}
	messageID := "msg-1"

	first, err := svc.RunAgent(context.Background(), claims, RunAgentInput{MessageID: &messageID, Prompt: "draft"})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	rootReference := "<msg-1@test>"
	h.sourceMessage = model.Message{
		ID: "msg-2", RFCMessageID: "<msg-2@test>", References: &rootReference,
		Subject: ptr("Re: Original Subject"), FromEmail: "sender@example.com", InternalDate: time.Now().UTC(),
	}
	secondMessageID := "msg-2"
	second, err := svc.RunAgent(context.Background(), claims, RunAgentInput{MessageID: &secondMessageID, Prompt: "revise"})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.Conversation.ID != second.Conversation.ID {
		t.Fatalf("expected conversation reuse, got %s then %s", first.Conversation.ID, second.Conversation.ID)
	}
	if len(h.conversations) != 1 {
		t.Fatalf("expected one conversation, got %d", len(h.conversations))
	}
	if first.Draft.ID == second.Draft.ID || second.Draft.SourceMessageID == nil || *second.Draft.SourceMessageID != secondMessageID {
		t.Fatalf("second run draft = %+v, want a new draft for %s", second.Draft, secondMessageID)
	}
}

func TestRunAgentRejectsCompletionWithSentDraftFromEarlierMessage(t *testing.T) {
	h := newAgentHarnessStore()
	planner := &scriptedPlanner{steps: []AgentPlanStep{
		{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"First"}`)},
		{Kind: "complete", CompletionMessage: "done"},
		{Kind: "complete", CompletionMessage: "stale completion"},
	}}
	aliasID := "alias-1"
	svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner}
	claims := model.TokenClaims{TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}
	firstMessageID := "msg-1"

	first, err := svc.RunAgent(context.Background(), claims, RunAgentInput{MessageID: &firstMessageID, Prompt: "draft"})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	stale := h.drafts[first.Draft.ID]
	stale.Status = model.DraftStatusSent
	h.drafts[stale.ID] = stale
	rootReference := "<msg-1@test>"
	h.sourceMessage = model.Message{
		ID: "msg-2", RFCMessageID: "<msg-2@test>", References: &rootReference,
		Subject: ptr("Re: Original Subject"), FromEmail: "sender@example.com", InternalDate: time.Now().UTC(),
	}
	secondMessageID := "msg-2"

	_, err = svc.RunAgent(context.Background(), claims, RunAgentInput{MessageID: &secondMessageID, Prompt: "continue"})
	if err == nil || err.Error() != "agent completed without an editable draft for the current message" {
		t.Fatalf("second run err = %v, want current-message draft rejection", err)
	}
	conv := h.conversations[first.Conversation.ID]
	if conv.LastDraftID == nil || *conv.LastDraftID != stale.ID || conv.Status != model.ConversationStatusFailed {
		t.Fatalf("failed conversation did not preserve prior draft linkage: %+v", conv)
	}
}

func TestExecuteAgentReviseDraftRejectsAnotherConversationOrSource(t *testing.T) {
	h := newAgentHarnessStore()
	aliasID := "alias-1"
	claims := model.TokenClaims{TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeSend: true}}
	svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary"}
	conversationID := "conv-current"
	currentMessageID := "msg-current"

	for _, tc := range []struct {
		name           string
		conversationID string
		sourceID       string
	}{
		{name: "other conversation", conversationID: "conv-other", sourceID: currentMessageID},
		{name: "other source", conversationID: conversationID, sourceID: "msg-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draftID := "draft-" + tc.name
			h.drafts[draftID] = model.Draft{
				ID: draftID, MailboxID: "m1", AliasID: &aliasID,
				ConversationID: &tc.conversationID, SourceMessageID: &tc.sourceID,
				Status:    model.DraftStatusPendingReview,
				Revisions: []model.DraftRevision{{ID: "rev-1", DraftID: draftID, RevisionNumber: 1}},
			}
			args := json.RawMessage(fmt.Sprintf(`{"draft_id":%q,"to":["recipient@example.test"],"body":"changed"}`, draftID))
			_, _, err := svc.executeAgentTool(context.Background(), claims, conversationID, &currentMessageID, nil, nil, "revise_draft", args)
			if err == nil || err.Error() != "revise_draft target must be an editable draft for the current conversation and message" {
				t.Fatalf("executeAgentTool() err = %v", err)
			}
			if got := len(h.drafts[draftID].Revisions); got != 1 {
				t.Fatalf("revision count = %d, want no mutation", got)
			}
		})
	}
}

type agentDraftEventRecorder struct {
	events []MailboxEvent
}

func (r *agentDraftEventRecorder) Publish(event MailboxEvent) {
	r.events = append(r.events, event)
}

func TestRunAgentPublishesOwnerScopedDraftEvents(t *testing.T) {
	h := newAgentHarnessStore()
	planner := &scriptedPlanner{steps: []AgentPlanStep{
		{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"First"}`)},
		{Kind: "tool", ToolName: "revise_draft", Arguments: json.RawMessage(`{"body":"Second"}`)},
		{Kind: "complete", CompletionMessage: "done"},
	}}
	aliasID := "alias-1"
	recorder := &agentDraftEventRecorder{}
	svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner, Events: recorder}
	claims := model.TokenClaims{TokenID: "auto-draft", AliasID: &aliasID, MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}
	h.settings.AutoDraftEnabled = true
	messageID := "msg-1"

	result, err := svc.RunAgent(context.Background(), claims, RunAgentInput{MessageID: &messageID, Prompt: "draft"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(recorder.events) != 2 {
		t.Fatalf("events = %#v, want create and revise", recorder.events)
	}
	for i, wantType := range []string{"draft_created", "draft_revised"} {
		got := recorder.events[i]
		if got.EventType != wantType || got.OwningAliasID != aliasID || got.MessageID != result.Draft.ID || len(got.Audience) != 0 {
			t.Fatalf("event %d = %+v, want %s scoped to %s", i, got, wantType, aliasID)
		}
	}
}

func TestRunAgentPublishesAdminOwnedDraftEventsAsAdminOnly(t *testing.T) {
	h := newAgentHarnessStore()
	planner := &scriptedPlanner{steps: []AgentPlanStep{
		{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"First"}`)},
		{Kind: "complete", CompletionMessage: "done"},
	}}
	recorder := &agentDraftEventRecorder{}
	svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner, Events: recorder}
	claims := model.TokenClaims{TokenID: "admin", MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeAdmin: true}}

	result, err := svc.RunAgent(context.Background(), claims, RunAgentInput{Prompt: "draft"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("events = %#v, want one admin-only draft event", recorder.events)
	}
	event := recorder.events[0]
	if event.EventType != "draft_created" || event.MessageID != result.Draft.ID || event.OwningAliasID != "" || len(event.Audience) != 0 {
		t.Fatalf("admin draft event = %+v, want unscoped admin-only event", event)
	}
}

func TestRunAgentFailsAndCleansUpWhenSuccessfulToolTraceWriteFails(t *testing.T) {
	for _, tc := range []struct {
		name           string
		configure      func(*agentHarnessStore)
		wantToolStatus model.ToolCallStatus
	}{
		{
			name: "finish tool call",
			configure: func(h *agentHarnessStore) {
				h.finishToolErr = errors.New("finish trace failed")
			},
			wantToolStatus: model.ToolCallStatusFailed,
		},
		{
			name: "append tool result",
			configure: func(h *agentHarnessStore) {
				h.toolMessageErr = errors.New("tool message trace failed")
			},
			wantToolStatus: model.ToolCallStatusSuccess,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgentHarnessStore()
			tc.configure(h)
			planner := &scriptedPlanner{steps: []AgentPlanStep{
				{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"Ready"}`)},
				{Kind: "complete", CompletionMessage: "done"},
			}}
			aliasID := "alias-1"
			svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner}
			claims := model.TokenClaims{TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}

			_, err := svc.RunAgent(context.Background(), claims, RunAgentInput{Prompt: "draft"})
			if err == nil {
				t.Fatal("RunAgent() unexpectedly succeeded")
			}
			conv := h.conversations["conv-1"]
			if conv.Status != model.ConversationStatusFailed || conv.LastDraftID == nil || *conv.LastDraftID != "draft-1" {
				t.Fatalf("conversation after trace failure = %+v, want failed with latest draft", conv)
			}
			if got := h.toolCalls["tool-1"].Status; got != tc.wantToolStatus {
				t.Fatalf("tool status after trace failure = %q, want %q", got, tc.wantToolStatus)
			}
		})
	}
}

func TestRunAgentPreservesDraftAndFailsConversationWhenAtomicCompletionFails(t *testing.T) {
	h := newAgentHarnessStore()
	h.completeErr = errors.New("atomic completion failed")
	planner := &scriptedPlanner{steps: []AgentPlanStep{
		{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"Ready"}`)},
		{Kind: "complete", CompletionMessage: "Draft is ready."},
	}}
	aliasID := "alias-1"
	svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner}
	claims := model.TokenClaims{TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}
	messageID := "msg-1"

	_, err := svc.RunAgent(context.Background(), claims, RunAgentInput{MessageID: &messageID, Prompt: "draft"})
	if err == nil || err.Error() != "atomic completion failed" {
		t.Fatalf("RunAgent() err = %v", err)
	}
	conv := h.conversations["conv-1"]
	if conv.Status != model.ConversationStatusFailed || conv.LastDraftID == nil || *conv.LastDraftID != "draft-1" {
		t.Fatalf("failed completion conversation = %+v, want failed with draft linkage", conv)
	}
	if h.completedDraftID != "draft-1" || h.completedSourceID == nil || *h.completedSourceID != messageID || h.completedMessage != "Draft is ready." {
		t.Fatalf("completion attempt = draft %q source %v message %q", h.completedDraftID, h.completedSourceID, h.completedMessage)
	}
}

func TestRunAgentPropagatesFinalConversationReloadFailure(t *testing.T) {
	h := newAgentHarnessStore()
	h.failReloadAfterComplete = true
	planner := &scriptedPlanner{steps: []AgentPlanStep{
		{Kind: "tool", ToolName: "create_draft", Arguments: json.RawMessage(`{"body":"Ready"}`)},
		{Kind: "complete", CompletionMessage: "Draft is ready."},
	}}
	aliasID := "alias-1"
	svc := &Service{Store: h, MailboxID: "m1", MailboxName: "primary", AgentPlanner: planner}
	claims := model.TokenClaims{TokenID: "tok-1", AliasID: &aliasID, MailboxID: ptr("m1"), Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}

	_, err := svc.RunAgent(context.Background(), claims, RunAgentInput{Prompt: "draft"})
	if err == nil || err.Error() != "reload failed" {
		t.Fatalf("RunAgent() err = %v, want final reload failure", err)
	}
	conv := h.conversations["conv-1"]
	if conv.Status != model.ConversationStatusCompleted || conv.LastDraftID == nil || *conv.LastDraftID != "draft-1" {
		t.Fatalf("atomic completion was not retained: %+v", conv)
	}
}
