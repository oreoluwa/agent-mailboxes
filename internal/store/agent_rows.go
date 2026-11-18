package store

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

type actorReplayRow struct {
	bun.BaseModel `bun:"table:actor_request_replays"`

	MailboxID string    `bun:"mailbox_id,pk"`
	RequestID string    `bun:"request_id,pk"`
	Nonce     string    `bun:"nonce,pk"`
	Action    string    `bun:"action"`
	TargetID  string    `bun:"target_id"`
	ExpiresAt time.Time `bun:"expires_at"`
	CreatedAt time.Time `bun:"created_at"`
}

type mailboxAgentSettingsRow struct {
	bun.BaseModel `bun:"table:mailbox_agent_settings"`

	MailboxID        string    `bun:"mailbox_id,pk"`
	SystemPrompt     string    `bun:"system_prompt"`
	EnabledTools     []string  `bun:"enabled_tools,array"`
	ModelOverride    *string   `bun:"model_override"`
	AutoDraftEnabled bool      `bun:"auto_draft_enabled"`
	MaxToolSteps     int       `bun:"max_tool_steps"`
	ToolTimeoutSec   int       `bun:"tool_timeout_sec"`
	PolicyVersion    int       `bun:"policy_version"`
	UpdatedBy        *string   `bun:"updated_by"`
	LastRequestID    *string   `bun:"last_request_id"`
	PermissionIntent *string   `bun:"permission_intent"`
	UpdatedAt        time.Time `bun:"updated_at"`
}

type agentConversationRow struct {
	bun.BaseModel `bun:"table:agent_conversations"`

	ID                 string     `bun:"id,pk"`
	MailboxID          string     `bun:"mailbox_id"`
	AliasID            *string    `bun:"alias_id"`
	MessageID          *string    `bun:"message_id"`
	ThreadRFCMessageID *string    `bun:"thread_rfc_message_id"`
	Status             string     `bun:"status"`
	CreatedBy          *string    `bun:"created_by"`
	LastDraftID        *string    `bun:"last_draft_id"`
	RunToken           *string    `bun:"run_token"`
	RunLeaseUntil      *time.Time `bun:"run_lease_until"`
	CreatedAt          time.Time  `bun:"created_at"`
	UpdatedAt          time.Time  `bun:"updated_at"`
}

type agentMessageRow struct {
	bun.BaseModel `bun:"table:agent_messages"`

	ID             string          `bun:"id,pk"`
	ConversationID string          `bun:"conversation_id"`
	Role           string          `bun:"role"`
	Content        string          `bun:"content"`
	Payload        json.RawMessage `bun:"payload,type:jsonb"`
	CreatedAt      time.Time       `bun:"created_at"`
}

type agentToolCallRow struct {
	bun.BaseModel `bun:"table:agent_tool_calls"`

	ID             string          `bun:"id,pk"`
	ConversationID string          `bun:"conversation_id"`
	ToolName       string          `bun:"tool_name"`
	Status         string          `bun:"status"`
	ArgsSummary    json.RawMessage `bun:"args_summary,type:jsonb"`
	ResultSummary  json.RawMessage `bun:"result_summary,type:jsonb"`
	PolicyDecision string          `bun:"policy_decision"`
	ErrorText      *string         `bun:"error_text"`
	StartedAt      time.Time       `bun:"started_at"`
	CompletedAt    *time.Time      `bun:"completed_at"`
}

type draftRow struct {
	bun.BaseModel `bun:"table:drafts"`

	ID                 string    `bun:"id,pk"`
	MailboxID          string    `bun:"mailbox_id"`
	AliasID            *string   `bun:"alias_id"`
	ConversationID     *string   `bun:"conversation_id"`
	SourceMessageID    *string   `bun:"source_message_id"`
	ThreadRFCMessageID *string   `bun:"thread_rfc_message_id"`
	Status             string    `bun:"status"`
	LatestRevisionID   *string   `bun:"latest_revision_id"`
	ActorUserID        *string   `bun:"actor_user_id"`
	LastRequestID      *string   `bun:"last_request_id"`
	PermissionIntent   *string   `bun:"permission_intent"`
	SendIdempotencyKey *string   `bun:"send_idempotency_key"`
	SentRevisionID     *string   `bun:"sent_revision_id"`
	CreatedAt          time.Time `bun:"created_at"`
	UpdatedAt          time.Time `bun:"updated_at"`
}

type draftRevisionRow struct {
	bun.BaseModel `bun:"table:draft_revisions"`

	ID               string          `bun:"id,pk"`
	DraftID          string          `bun:"draft_id"`
	RevisionNumber   int             `bun:"revision_number"`
	Provenance       string          `bun:"provenance"`
	Subject          string          `bun:"subject"`
	ToEmails         json.RawMessage `bun:"to_emails,type:jsonb"`
	Body             string          `bun:"body"`
	InReplyTo        *string         `bun:"in_reply_to"`
	References       *string         `bun:"references"`
	ActorUserID      *string         `bun:"actor_user_id"`
	RequestID        *string         `bun:"request_id"`
	PermissionIntent *string         `bun:"permission_intent"`
	CreatedAt        time.Time       `bun:"created_at"`
}

type autoDraftJobRow struct {
	bun.BaseModel `bun:"table:auto_draft_jobs"`

	ID            string     `bun:"id,pk"`
	MailboxID     string     `bun:"mailbox_id"`
	MessageID     string     `bun:"message_id"`
	PolicyVersion int        `bun:"policy_version"`
	Status        string     `bun:"status"`
	Attempts      int        `bun:"attempts"`
	ClaimToken    *string    `bun:"claim_token"`
	NextAttemptAt time.Time  `bun:"next_attempt_at"`
	LeaseUntil    *time.Time `bun:"lease_until"`
	LastError     *string    `bun:"last_error"`
	CreatedAt     time.Time  `bun:"created_at"`
	UpdatedAt     time.Time  `bun:"updated_at"`
}

type autoDraftRunRow struct {
	bun.BaseModel `bun:"table:auto_draft_runs"`

	ID            string    `bun:"id,pk"`
	MailboxID     string    `bun:"mailbox_id"`
	MessageID     string    `bun:"message_id"`
	JobID         string    `bun:"job_id"`
	PolicyVersion int       `bun:"policy_version"`
	Status        string    `bun:"status"`
	FailureReason *string   `bun:"failure_reason"`
	CreatedAt     time.Time `bun:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at"`
}

func mailboxAgentSettingsFromRow(row mailboxAgentSettingsRow) model.MailboxAgentSettings {
	tools := append([]string{}, row.EnabledTools...)
	return model.MailboxAgentSettings{
		MailboxID:        row.MailboxID,
		SystemPrompt:     row.SystemPrompt,
		EnabledTools:     tools,
		ModelOverride:    row.ModelOverride,
		AutoDraftEnabled: row.AutoDraftEnabled,
		MaxToolSteps:     row.MaxToolSteps,
		ToolTimeoutSec:   row.ToolTimeoutSec,
		PolicyVersion:    row.PolicyVersion,
		UpdatedBy:        row.UpdatedBy,
		LastRequestID:    row.LastRequestID,
		PermissionIntent: row.PermissionIntent,
		UpdatedAt:        row.UpdatedAt,
	}
}

func draftRevisionFromRow(row draftRevisionRow) model.DraftRevision {
	return model.DraftRevision{
		ID:               row.ID,
		DraftID:          row.DraftID,
		RevisionNumber:   row.RevisionNumber,
		Provenance:       row.Provenance,
		Subject:          row.Subject,
		To:               parseOutboundRecipients(row.ToEmails),
		Body:             row.Body,
		InReplyTo:        row.InReplyTo,
		References:       row.References,
		ActorUserID:      row.ActorUserID,
		RequestID:        row.RequestID,
		PermissionIntent: row.PermissionIntent,
		CreatedAt:        row.CreatedAt,
	}
}

func agentConversationFromRow(row agentConversationRow) model.AgentConversation {
	return model.AgentConversation{
		ID:                 row.ID,
		MailboxID:          row.MailboxID,
		AliasID:            row.AliasID,
		MessageID:          row.MessageID,
		ThreadRFCMessageID: row.ThreadRFCMessageID,
		Status:             model.ConversationStatus(row.Status),
		CreatedBy:          row.CreatedBy,
		LastDraftID:        row.LastDraftID,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}

func agentConversationTurnFromRow(row agentMessageRow) model.AgentConversationTurn {
	return model.AgentConversationTurn{
		ID:             row.ID,
		ConversationID: row.ConversationID,
		Role:           row.Role,
		Content:        row.Content,
		Payload:        append(json.RawMessage(nil), row.Payload...),
		CreatedAt:      row.CreatedAt,
	}
}

func agentToolCallFromRow(row agentToolCallRow) model.AgentToolCall {
	return model.AgentToolCall{
		ID:             row.ID,
		ConversationID: row.ConversationID,
		ToolName:       row.ToolName,
		Status:         model.ToolCallStatus(row.Status),
		ArgsSummary:    append(json.RawMessage(nil), row.ArgsSummary...),
		ResultSummary:  append(json.RawMessage(nil), row.ResultSummary...),
		PolicyDecision: row.PolicyDecision,
		ErrorText:      row.ErrorText,
		StartedAt:      row.StartedAt,
		CompletedAt:    row.CompletedAt,
	}
}

func autoDraftJobFromRow(row autoDraftJobRow) AutoDraftJob {
	claimToken := ""
	if row.ClaimToken != nil {
		claimToken = *row.ClaimToken
	}
	return AutoDraftJob{
		ID:            row.ID,
		MailboxID:     row.MailboxID,
		MessageID:     row.MessageID,
		PolicyVersion: row.PolicyVersion,
		Status:        row.Status,
		Attempts:      row.Attempts,
		ClaimToken:    claimToken,
		NextAttemptAt: row.NextAttemptAt,
		LeaseUntil:    row.LeaseUntil,
		LastError:     row.LastError,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
}

func defaultAgentTools() []string {
	return []string{
		"search_messages",
		"read_message",
		"read_body",
		"get_thread_context",
		"list_recent_messages",
		"create_draft",
		"revise_draft",
	}
}
