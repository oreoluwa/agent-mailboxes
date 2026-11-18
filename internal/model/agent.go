package model

import (
	"encoding/json"
	"time"
)

type DraftStatus string

const (
	DraftStatusPendingReview  DraftStatus = "pending_review"
	DraftStatusReadyToSend    DraftStatus = "ready_to_send"
	DraftStatusQueued         DraftStatus = "queued"
	DraftStatusSent           DraftStatus = "sent"
	DraftStatusFailed         DraftStatus = "failed"
	DraftStatusDiscarded      DraftStatus = "discarded"
	DraftStatusSuperseded     DraftStatus = "superseded"
	DraftStatusAutoDraftBlock DraftStatus = "auto_draft_blocked"
)

type ConversationStatus string

const (
	ConversationStatusRunning   ConversationStatus = "running"
	ConversationStatusCompleted ConversationStatus = "completed"
	ConversationStatusFailed    ConversationStatus = "failed"
)

type ToolCallStatus string

const (
	ToolCallStatusStarted  ToolCallStatus = "started"
	ToolCallStatusSuccess  ToolCallStatus = "success"
	ToolCallStatusFailed   ToolCallStatus = "failed"
	ToolCallStatusRejected ToolCallStatus = "rejected"
)

type ActorEnvelope struct {
	UserID          string
	MailboxID       string
	RequestID       string
	Permission      string
	Timestamp       int64
	Nonce           string
	Action          string
	TargetID        string
	SignatureBase64 string
}

type VerifiedActor struct {
	UserID           string    `json:"user_id"`
	MailboxID        string    `json:"mailbox_id"`
	RequestID        string    `json:"request_id"`
	PermissionIntent string    `json:"permission_intent"`
	Action           string    `json:"action"`
	TargetID         string    `json:"target_id"`
	Nonce            string    `json:"nonce"`
	VerifiedAt       time.Time `json:"verified_at"`
}

type Draft struct {
	ID                 string          `json:"id"`
	MailboxID          string          `json:"mailbox_id"`
	AliasID            *string         `json:"alias_id,omitempty"`
	ConversationID     *string         `json:"conversation_id,omitempty"`
	SourceMessageID    *string         `json:"source_message_id,omitempty"`
	ThreadRFCMessageID *string         `json:"thread_rfc_message_id,omitempty"`
	Status             DraftStatus     `json:"status"`
	LatestRevisionID   string          `json:"latest_revision_id"`
	LatestRevision     DraftRevision   `json:"latest_revision"`
	Revisions          []DraftRevision `json:"revisions,omitempty"`
	ActorUserID        *string         `json:"actor_user_id,omitempty"`
	LastRequestID      *string         `json:"last_request_id,omitempty"`
	PermissionIntent   *string         `json:"permission_intent,omitempty"`
	SendIdempotencyKey *string         `json:"send_idempotency_key,omitempty"`
	SentRevisionID     *string         `json:"sent_revision_id,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

type DraftRevision struct {
	ID               string    `json:"id"`
	DraftID          string    `json:"draft_id"`
	RevisionNumber   int       `json:"revision_number"`
	Provenance       string    `json:"provenance"`
	Subject          string    `json:"subject"`
	To               []string  `json:"to"`
	Body             string    `json:"body"`
	InReplyTo        *string   `json:"in_reply_to,omitempty"`
	References       *string   `json:"references,omitempty"`
	ActorUserID      *string   `json:"actor_user_id,omitempty"`
	RequestID        *string   `json:"request_id,omitempty"`
	PermissionIntent *string   `json:"permission_intent,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type AgentConversation struct {
	ID                 string                  `json:"id"`
	MailboxID          string                  `json:"mailbox_id"`
	AliasID            *string                 `json:"alias_id,omitempty"`
	MessageID          *string                 `json:"message_id,omitempty"`
	ThreadRFCMessageID *string                 `json:"thread_rfc_message_id,omitempty"`
	Status             ConversationStatus      `json:"status"`
	CreatedBy          *string                 `json:"created_by,omitempty"`
	CreatedAt          time.Time               `json:"created_at"`
	UpdatedAt          time.Time               `json:"updated_at"`
	Messages           []AgentConversationTurn `json:"messages,omitempty"`
	ToolCalls          []AgentToolCall         `json:"tool_calls,omitempty"`
	LastDraftID        *string                 `json:"last_draft_id,omitempty"`
}

type AgentConversationTurn struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversation_id"`
	Role           string          `json:"role"`
	Content        string          `json:"content"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

type AgentToolCall struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversation_id"`
	ToolName       string          `json:"tool_name"`
	Status         ToolCallStatus  `json:"status"`
	ArgsSummary    json.RawMessage `json:"args_summary,omitempty"`
	ResultSummary  json.RawMessage `json:"result_summary,omitempty"`
	PolicyDecision string          `json:"policy_decision,omitempty"`
	ErrorText      *string         `json:"error_text,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

type MailboxAgentSettings struct {
	MailboxID        string    `json:"mailbox_id"`
	SystemPrompt     string    `json:"system_prompt"`
	EnabledTools     []string  `json:"enabled_tools"`
	ModelOverride    *string   `json:"model_override,omitempty"`
	AutoDraftEnabled bool      `json:"auto_draft_enabled"`
	MaxToolSteps     int       `json:"max_tool_steps"`
	ToolTimeoutSec   int       `json:"tool_timeout_sec"`
	PolicyVersion    int       `json:"policy_version"`
	UpdatedBy        *string   `json:"updated_by,omitempty"`
	LastRequestID    *string   `json:"last_request_id,omitempty"`
	PermissionIntent *string   `json:"permission_intent,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type MailboxAgentContext struct {
	MailboxID string `json:"mailbox_id"`
}

type AutoDraftRun struct {
	ID            string    `json:"id"`
	MailboxID     string    `json:"mailbox_id"`
	MessageID     string    `json:"message_id"`
	JobID         string    `json:"job_id"`
	PolicyVersion int       `json:"policy_version"`
	Status        string    `json:"status"`
	FailureReason *string   `json:"failure_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
