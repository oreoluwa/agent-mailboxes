package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

func (f *fakeStore) ListDrafts(ctx context.Context, claims model.TokenClaims, mailboxID string, options store.DraftListOptions) ([]model.Draft, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeStore) GetDraft(ctx context.Context, claims model.TokenClaims, mailboxID, draftID string) (model.Draft, error) {
	return model.Draft{}, errors.New("not implemented")
}

func (f *fakeStore) CreateDraft(ctx context.Context, claims model.TokenClaims, input store.CreateDraftInput) (model.Draft, error) {
	return model.Draft{}, errors.New("not implemented")
}

func (f *fakeStore) ReviseDraft(ctx context.Context, claims model.TokenClaims, input store.ReviseDraftInput) (model.Draft, error) {
	return model.Draft{}, errors.New("not implemented")
}

func (f *fakeStore) DiscardDraft(ctx context.Context, claims model.TokenClaims, mailboxID, draftID string, actor *model.VerifiedActor) (model.Draft, error) {
	return model.Draft{}, errors.New("not implemented")
}

func (f *fakeStore) SendDraft(ctx context.Context, claims model.TokenClaims, input store.SendDraftInput) (store.DraftSendResult, error) {
	return store.DraftSendResult{}, errors.New("not implemented")
}

func (f *fakeStore) CreateAgentConversation(ctx context.Context, claims model.TokenClaims, mailboxID string, sourceMessageID *string, threadRFCMessageID *string, createdBy *string) (model.AgentConversation, error) {
	return model.AgentConversation{}, errors.New("not implemented")
}

func (f *fakeStore) GetAgentConversation(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID string) (model.AgentConversation, error) {
	return model.AgentConversation{}, errors.New("not implemented")
}

func (f *fakeStore) ClaimAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken string, lease time.Duration) (bool, error) {
	return true, nil
}

func (f *fakeStore) ReleaseAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken string) (bool, error) {
	return true, nil
}

func (f *fakeStore) CompleteAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken, draftID string, sourceMessageID *string, completionMessage string) error {
	return errors.New("not implemented")
}

func (f *fakeStore) FailAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken string, lastDraftID *string, failure string) error {
	return nil
}

func (f *fakeStore) ListAgentConversations(ctx context.Context, claims model.TokenClaims, mailboxID string, options store.AgentConversationListOptions) ([]model.AgentConversation, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeStore) AddAgentConversationMessage(ctx context.Context, claims model.TokenClaims, mailboxID string, conversationID, role, content string, payload json.RawMessage) (model.AgentConversationTurn, error) {
	return model.AgentConversationTurn{}, errors.New("not implemented")
}

func (f *fakeStore) StartAgentToolCall(ctx context.Context, claims model.TokenClaims, mailboxID string, conversationID, toolName string, argsSummary json.RawMessage, policyDecision string) (model.AgentToolCall, error) {
	return model.AgentToolCall{}, errors.New("not implemented")
}

func (f *fakeStore) FinishAgentToolCall(ctx context.Context, claims model.TokenClaims, mailboxID string, toolCallID string, status model.ToolCallStatus, resultSummary json.RawMessage, errText *string) (model.AgentToolCall, error) {
	return model.AgentToolCall{}, errors.New("not implemented")
}

func (f *fakeStore) SetAgentConversationStatus(ctx context.Context, claims model.TokenClaims, mailboxID string, conversationID string, status model.ConversationStatus, lastDraftID *string) error {
	return nil
}

func (f *fakeStore) GetMailboxAgentSettings(ctx context.Context, mailboxID string) (model.MailboxAgentSettings, error) {
	return model.MailboxAgentSettings{
		MailboxID:        mailboxID,
		EnabledTools:     []string{"search_messages", "read_message", "read_body", "get_thread_context", "list_recent_messages", "create_draft", "revise_draft"},
		MaxToolSteps:     6,
		ToolTimeoutSec:   45,
		PolicyVersion:    1,
		AutoDraftEnabled: false,
	}, nil
}

func (f *fakeStore) UpdateMailboxAgentSettings(ctx context.Context, mailboxID string, settings store.UpdateMailboxAgentSettingsInput) (model.MailboxAgentSettings, error) {
	return model.MailboxAgentSettings{}, errors.New("not implemented")
}

func (f *fakeStore) RecordActorReplay(ctx context.Context, mailboxID string, requestID string, nonce string, action string, targetID string, expiresAt time.Time) error {
	return nil
}

func (f *fakeStore) QueueAutoDraftJob(ctx context.Context, mailboxID, messageID string, policyVersion int) (store.AutoDraftJob, bool, error) {
	return store.AutoDraftJob{}, true, nil
}

func (f *fakeStore) ClaimAutoDraftJob(ctx context.Context, mailboxID string, lease time.Duration) (store.AutoDraftJob, bool, error) {
	return store.AutoDraftJob{}, false, nil
}

func (f *fakeStore) RenewAutoDraftJobLease(ctx context.Context, jobID, claimToken string, lease time.Duration) (bool, error) {
	return true, nil
}

func (f *fakeStore) MarkAutoDraftJobDone(ctx context.Context, jobID, claimToken string) (bool, error) {
	return true, nil
}

func (f *fakeStore) MarkAutoDraftJobRetry(ctx context.Context, jobID, claimToken, lastErr string, retryAfter time.Duration) (bool, error) {
	return true, nil
}

func (f *fakeStore) CreateAutoDraftRun(ctx context.Context, mailboxID, messageID, jobID string, policyVersion int, status string, failureReason *string) (model.AutoDraftRun, error) {
	return model.AutoDraftRun{}, nil
}

func (f *fakeStore) UpdateAutoDraftRunStatus(ctx context.Context, runID string, status string, failureReason *string) error {
	return nil
}

func (f *fakeStore) CompletedAutoDraftAliases(ctx context.Context, jobID string) ([]string, error) {
	out := []string{}
	for id := range f.autoDraftCompleted[jobID] {
		out = append(out, id)
	}
	return out, nil
}
func (f *fakeStore) CompleteAutoDraftAlias(ctx context.Context, jobID, aliasID, claimToken string) (bool, error) {
	if f.autoDraftCompleted == nil {
		f.autoDraftCompleted = map[string]map[string]bool{}
	}
	if f.autoDraftCompleted[jobID] == nil {
		f.autoDraftCompleted[jobID] = map[string]bool{}
	}
	f.autoDraftCompleted[jobID][aliasID] = true
	return true, nil
}
