package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/uptrace/bun"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/router"
)

// toValidUTF8 replaces invalid UTF-8 sequences so PostgreSQL (UTF-8) accepts the string.
// Email/IMAP data can be Latin-1 or Windows-1252 (e.g. 0xa0 = NBSP), which causes
// "invalid byte sequence for encoding UTF8" (SQLSTATE 22021).
func toValidUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, " ")
}

func sanitizeOptionalString(s *string) *string {
	if s == nil {
		return nil
	}
	out := toValidUTF8(*s)
	return &out
}

func sanitizeOptionalStringToValue(s *string) string {
	if s == nil {
		return ""
	}
	return toValidUTF8(*s)
}

func synthesizeRawMessage(rawHeaders string, msg model.Message, body, contentType string) []byte {
	headers := normalizeSynthesizedHeaders(rawHeaders, msg)
	if strings.TrimSpace(contentType) == "" {
		contentType = "text/plain; charset=utf-8"
	}
	headers += "\r\nMIME-Version: 1.0"
	headers += "\r\nContent-Type: " + contentType
	return []byte(headers + "\r\n\r\n" + toValidUTF8(body))
}

func normalizeSynthesizedHeaders(rawHeaders string, msg model.Message) string {
	parsed, err := mail.ReadMessage(strings.NewReader(strings.TrimSpace(rawHeaders) + "\r\n\r\n"))
	if err != nil {
		var b strings.Builder
		fmt.Fprintf(&b, "From: %s\r\n", msg.FromEmail)
		fmt.Fprintf(&b, "Subject: %s\r\n", toValidUTF8(derefString(msg.Subject)))
		fmt.Fprintf(&b, "Date: %s\r\n", msg.InternalDate.Format(time.RFC1123Z))
		if strings.TrimSpace(msg.RFCMessageID) != "" {
			fmt.Fprintf(&b, "Message-ID: %s\r\n", msg.RFCMessageID)
		}
		return strings.TrimRight(b.String(), "\r\n")
	}
	var b strings.Builder
	excluded := map[string]struct{}{
		"mime-version":              {},
		"content-type":              {},
		"content-transfer-encoding": {},
		"content-disposition":       {},
	}
	for key, values := range parsed.Header {
		if _, skip := excluded[strings.ToLower(strings.TrimSpace(key))]; skip {
			continue
		}
		for _, value := range values {
			fmt.Fprintf(&b, "%s: %s\r\n", key, value)
		}
	}
	if strings.TrimSpace(parsed.Header.Get("From")) == "" && strings.TrimSpace(msg.FromEmail) != "" {
		fmt.Fprintf(&b, "From: %s\r\n", msg.FromEmail)
	}
	if strings.TrimSpace(parsed.Header.Get("Subject")) == "" && msg.Subject != nil {
		fmt.Fprintf(&b, "Subject: %s\r\n", toValidUTF8(derefString(msg.Subject)))
	}
	if strings.TrimSpace(parsed.Header.Get("Date")) == "" && !msg.InternalDate.IsZero() {
		fmt.Fprintf(&b, "Date: %s\r\n", msg.InternalDate.Format(time.RFC1123Z))
	}
	if strings.TrimSpace(parsed.Header.Get("Message-ID")) == "" && strings.TrimSpace(msg.RFCMessageID) != "" {
		fmt.Fprintf(&b, "Message-ID: %s\r\n", msg.RFCMessageID)
	}
	return strings.TrimRight(b.String(), "\r\n")
}

var ErrNotFound = errors.New("not found")
var ErrUnauthorized = errors.New("unauthorized")
var ErrAgentRunConflict = errors.New("agent conversation already has an active run")
var ErrAgentCredentialExists = errors.New("an active credential already exists for this mailbox alias and name")

type Store interface {
	CreateMailbox(ctx context.Context, name string, creds MailboxCreds) (Mailbox, error)
	OnboardMailbox(ctx context.Context, input OnboardMailboxInput) (OnboardMailboxResult, error)
	ProvisionMailboxAgent(ctx context.Context, input ProvisionMailboxAgentInput) (ProvisionMailboxAgentResult, error)
	ListMailboxes(ctx context.Context) ([]Mailbox, error)
	GetMailboxByName(ctx context.Context, name string) (Mailbox, error)
	SetMailboxEnabled(ctx context.Context, name string, enabled bool) error
	GetFolderCheckpoint(ctx context.Context, mailboxID, folder string) (uidValidity uint32, lastUID uint32, folderID string, err error)
	UpsertFolderCheckpoint(ctx context.Context, mailboxID, folder string, uidValidity uint32, lastUID uint32) (folderID string, err error)
	ListAliases(ctx context.Context, mailboxID string) ([]router.Alias, error)
	AddAlias(ctx context.Context, mailboxID, email string) error
	MintToken(ctx context.Context, aliasID *string, name string, tokenHash string, scopes []string) (string, error)
	RevokeToken(ctx context.Context, tokenID string) error
	AuthenticateToken(ctx context.Context, tokenHash string) (model.TokenClaims, error)
	InsertMessage(ctx context.Context, mailboxID, folderID string, msg model.ParsedEmail, audience []string) (messageID string, inserted bool, err error)
	UpdateAttachmentStorage(ctx context.Context, messageID string, ordinal int, storagePath string) error
	SaveDigest(ctx context.Context, messageID string, digest model.Digest) error
	ListFolders(ctx context.Context, claims model.TokenClaims) ([]model.FolderSummary, error)
	ListInbox(ctx context.Context, claims model.TokenClaims, folder string, unreadOnly bool, limit int) ([]model.ListItem, error)
	ReadMessage(ctx context.Context, claims model.TokenClaims, messageID string) (model.Message, error)
	GetBody(ctx context.Context, claims model.TokenClaims, messageID string) (model.Body, error)
	GetRawMessage(ctx context.Context, claims model.TokenClaims, messageID string) ([]byte, error)
	AckMessage(ctx context.Context, claims model.TokenClaims, messageID string) error
	UnackMessage(ctx context.Context, claims model.TokenClaims, messageID string) error
	QueueOutboundMessage(ctx context.Context, mailboxID, aliasID, from string, to []string, raw []byte, idempotencyKey string) (OutboundMessage, bool, error)
	ClaimOutboundMessages(ctx context.Context, mailboxID string, limit int, lease time.Duration) ([]OutboundMessage, error)
	MarkOutboundMessagesSent(ctx context.Context, ids []string) error
	MarkOutboundMessagesRetry(ctx context.Context, ids []string, lastErr string, retryAfter time.Duration) error
	MarkOutboundMessagesDead(ctx context.Context, ids []string, lastErr string) error
	SaveMessageEmbeddings(ctx context.Context, messageID, modelName string, segments []EmbeddingSegment, embeddings [][]float32) error
	QueueEmbeddingJobs(ctx context.Context, messageID, modelName string, segments []EmbeddingSegment) error
	ClaimEmbeddingJobs(ctx context.Context, mailboxID, modelName string, limit int, lease time.Duration) ([]EmbeddingJob, error)
	MarkEmbeddingJobsSubmitted(ctx context.Context, ids []string, batchID string) error
	ListEmbeddingSubmittedBatchIDs(ctx context.Context, mailboxID, modelName string, limit int) ([]string, error)
	GetEmbeddingJobsByBatch(ctx context.Context, mailboxID, batchID string) ([]EmbeddingJob, error)
	GetEmbeddingJobsByIDs(ctx context.Context, mailboxID string, ids []string) ([]EmbeddingJob, error)
	MarkEmbeddingJobsDone(ctx context.Context, ids []string) error
	MarkEmbeddingJobsRetry(ctx context.Context, ids []string, lastErr string, retryAfter time.Duration) error
	QueueSummarizeJob(ctx context.Context, messageID string, payload []byte, lastErr string) error
	ClaimSummarizeJobs(ctx context.Context, mailboxID string, limit int, lease time.Duration) ([]SummarizeJob, error)
	MarkSummarizeJobsDone(ctx context.Context, ids []string) error
	MarkSummarizeJobsRetry(ctx context.Context, ids []string, lastErr string, retryAfter time.Duration) error
	Search(ctx context.Context, claims model.TokenClaims, query, folder string, limit int) ([]model.ListItem, error)
	SearchByEmbedding(ctx context.Context, claims model.TokenClaims, embedding []float32, folder string, limit int) ([]model.ListItem, error)
	SearchStructured(ctx context.Context, claims model.TokenClaims, req model.SearchRequest) ([]model.ListItem, error)
	SearchStructuredByEmbedding(ctx context.Context, claims model.TokenClaims, req model.SearchRequest, embedding []float32) ([]model.ListItem, error)
	RecordAudit(ctx context.Context, tokenID, action string, messageID *string, target *string) error
	GetAliasByEmail(ctx context.Context, mailboxID, email string) (string, error)
	GetAliasEmailByID(ctx context.Context, aliasID string) (string, error)
	GetMailboxIDByAliasID(ctx context.Context, aliasID string) (string, error)
	GetThreadContextForMessage(ctx context.Context, claims model.TokenClaims, mailboxID, messageID string) ([]model.ThreadContextMessage, error)
	GetMessageThreadMeta(ctx context.Context, claims model.TokenClaims, messageID string) (inReplyTo *string, references *string, subject *string, err error)
	GetMessageReplyTo(ctx context.Context, claims model.TokenClaims, messageID string) (string, error)
	ListDrafts(ctx context.Context, claims model.TokenClaims, mailboxID string, options DraftListOptions) ([]model.Draft, error)
	GetDraft(ctx context.Context, claims model.TokenClaims, mailboxID, draftID string) (model.Draft, error)
	CreateDraft(ctx context.Context, claims model.TokenClaims, input CreateDraftInput) (model.Draft, error)
	ReviseDraft(ctx context.Context, claims model.TokenClaims, input ReviseDraftInput) (model.Draft, error)
	DiscardDraft(ctx context.Context, claims model.TokenClaims, mailboxID, draftID string, actor *model.VerifiedActor) (model.Draft, error)
	SendDraft(ctx context.Context, claims model.TokenClaims, input SendDraftInput) (DraftSendResult, error)
	CreateAgentConversation(ctx context.Context, claims model.TokenClaims, mailboxID string, sourceMessageID *string, threadRFCMessageID *string, createdBy *string) (model.AgentConversation, error)
	GetAgentConversation(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID string) (model.AgentConversation, error)
	ClaimAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken string, lease time.Duration) (bool, error)
	ReleaseAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken string) (bool, error)
	CompleteAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken, draftID string, sourceMessageID *string, completionMessage string) error
	FailAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken string, lastDraftID *string, failure string) error
	ListAgentConversations(ctx context.Context, claims model.TokenClaims, mailboxID string, options AgentConversationListOptions) ([]model.AgentConversation, error)
	AddAgentConversationMessage(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, role, content string, payload json.RawMessage) (model.AgentConversationTurn, error)
	StartAgentToolCall(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, toolName string, argsSummary json.RawMessage, policyDecision string) (model.AgentToolCall, error)
	FinishAgentToolCall(ctx context.Context, claims model.TokenClaims, mailboxID, toolCallID string, status model.ToolCallStatus, resultSummary json.RawMessage, errText *string) (model.AgentToolCall, error)
	SetAgentConversationStatus(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID string, status model.ConversationStatus, lastDraftID *string) error
	GetMailboxAgentSettings(ctx context.Context, mailboxID string) (model.MailboxAgentSettings, error)
	UpdateMailboxAgentSettings(ctx context.Context, mailboxID string, settings UpdateMailboxAgentSettingsInput) (model.MailboxAgentSettings, error)
	RecordActorReplay(ctx context.Context, mailboxID string, requestID string, nonce string, action string, targetID string, expiresAt time.Time) error
	CompletedAutoDraftAliases(ctx context.Context, jobID string) ([]string, error)
	CompleteAutoDraftAlias(ctx context.Context, jobID, aliasID, claimToken string) (bool, error)
	QueueAutoDraftJob(ctx context.Context, mailboxID, messageID string, policyVersion int) (AutoDraftJob, bool, error)
	ClaimAutoDraftJob(ctx context.Context, mailboxID string, lease time.Duration) (AutoDraftJob, bool, error)
	RenewAutoDraftJobLease(ctx context.Context, jobID, claimToken string, lease time.Duration) (bool, error)
	MarkAutoDraftJobDone(ctx context.Context, jobID, claimToken string) (bool, error)
	MarkAutoDraftJobRetry(ctx context.Context, jobID, claimToken, lastErr string, retryAfter time.Duration) (bool, error)
	CreateAutoDraftRun(ctx context.Context, mailboxID, messageID, jobID string, policyVersion int, status string, failureReason *string) (model.AutoDraftRun, error)
	UpdateAutoDraftRunStatus(ctx context.Context, runID string, status string, failureReason *string) error
	Status(ctx context.Context, mailboxID string) (Status, error)
}

type MailboxCreds struct {
	IMAPHost string `json:"imap_host"`
	IMAPPort int    `json:"imap_port"`
	IMAPTLS  bool   `json:"imap_tls"`
	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"`
	SMTPTLS  bool   `json:"smtp_tls"`
	Username string `json:"username"`
	Password string `json:"-"`
}

type Mailbox struct {
	ID        string
	Name      string
	Enabled   bool
	Creds     MailboxCreds
	CreatedAt time.Time
}

type OnboardMailboxInput struct {
	Name       string
	Creds      MailboxCreds
	AliasEmail string
	TokenName  string
	TokenHash  string
	Scopes     []string
}

type OnboardMailboxResult struct {
	Mailbox Mailbox
	AliasID string
	TokenID string
}

type ProvisionMailboxAgentInput struct {
	MailboxName string
	AliasEmail  string
	TokenName   string
	TokenHash   string
	Scopes      []string
	Replace     bool
}

type ProvisionMailboxAgentResult struct {
	MailboxID   string
	MailboxName string
	AliasID     string
	AliasEmail  string
	TokenID     string
}

type Status struct {
	MessageCount int
	DigestCount  int
	AliasCount   int
	LastSeenUID  int64
}

type EmbeddingSegment struct {
	Segment    string
	ChunkIndex int
	Text       string
}

type EmbeddingJob struct {
	ID        string
	MessageID string
	Segment   EmbeddingSegment
}

type SummarizeJob struct {
	ID        string
	MessageID string
	Payload   []byte
}

type OutboundMessage struct {
	ID             string
	MailboxID      string
	AliasID        string
	FromEmail      string
	To             []string
	RawRFC822      []byte
	IdempotencyKey string
	Status         string
	Attempts       int
	LastError      *string
	AcceptedAt     time.Time
	SentAt         *time.Time
}

type CreateDraftInput struct {
	MailboxID          string
	ConversationID     *string
	SourceMessageID    *string
	ThreadRFCMessageID *string
	Subject            string
	To                 []string
	Body               string
	InReplyTo          *string
	References         *string
	Provenance         string
	Actor              *model.VerifiedActor
}

type DraftListOptions struct {
	SourceMessageID *string
	Status          model.DraftStatus
	Limit           int
}

type AgentConversationListOptions struct {
	SourceMessageID    *string
	ThreadRFCMessageID *string
	Limit              int
}

type ReviseDraftInput struct {
	MailboxID  string
	DraftID    string
	Subject    string
	To         []string
	Body       string
	InReplyTo  *string
	References *string
	Provenance string
	Actor      *model.VerifiedActor
}

type SendDraftInput struct {
	MailboxID          string
	DraftID            string
	AliasID            string
	FromEmail          string
	ReviewedRevisionID string
	Actor              *model.VerifiedActor
}

type DraftSendResult struct {
	Draft      model.Draft
	Outbound   OutboundMessage
	Idempotent bool
}

type UpdateMailboxAgentSettingsInput struct {
	SystemPrompt     string
	EnabledTools     []string
	ModelOverride    *string
	AutoDraftEnabled bool
	MaxToolSteps     int
	ToolTimeoutSec   int
	Actor            *model.VerifiedActor
}

type AutoDraftJob struct {
	ID            string
	MailboxID     string
	MessageID     string
	PolicyVersion int
	Status        string
	Attempts      int
	ClaimToken    string
	NextAttemptAt time.Time
	LeaseUntil    *time.Time
	LastError     *string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Postgres struct {
	DB               *sql.DB
	BunDB            *bun.DB
	credentialCipher *mailboxCredentialCipher
}

func NewPostgres(db *sql.DB) *Postgres { return &Postgres{DB: db, BunDB: newBunDB(db)} }

func (p *Postgres) bun() *bun.DB {
	if p.BunDB == nil && p.DB != nil {
		p.BunDB = newBunDB(p.DB)
	}
	return p.BunDB
}

func (p *Postgres) CreateMailbox(ctx context.Context, name string, creds MailboxCreds) (Mailbox, error) {
	id := uuid.NewString()
	createdAt := time.Now().UTC()
	encryptedPassword, err := p.encryptMailboxCredential(id, creds.Password)
	if err != nil {
		return Mailbox{}, err
	}
	row := mailboxRow{
		ID:                id,
		Name:              name,
		Enabled:           true,
		IMAPHost:          creds.IMAPHost,
		IMAPPort:          creds.IMAPPort,
		IMAPTLS:           creds.IMAPTLS,
		SMTPHost:          creds.SMTPHost,
		SMTPPort:          creds.SMTPPort,
		SMTPTLS:           creds.SMTPTLS,
		Username:          creds.Username,
		PasswordEncrypted: encryptedPassword,
		CreatedAt:         createdAt,
	}
	_, err = p.bun().NewInsert().Model(&row).Exec(ctx)
	if err != nil {
		return Mailbox{}, err
	}
	return p.mailboxFromRow(row)
}

// OnboardMailbox atomically creates the physical mailbox, its first alias, and
// the alias-scoped credential. Provider authentication is validated by the API
// before this transaction begins.
func (p *Postgres) OnboardMailbox(ctx context.Context, input OnboardMailboxInput) (OnboardMailboxResult, error) {
	mailboxID := uuid.NewString()
	aliasID := uuid.NewString()
	tokenID := uuid.NewString()
	createdAt := time.Now().UTC()
	encryptedPassword, err := p.encryptMailboxCredential(mailboxID, input.Creds.Password)
	if err != nil {
		return OnboardMailboxResult{}, err
	}
	mailbox := mailboxRow{
		ID: mailboxID, Name: strings.TrimSpace(input.Name), Enabled: true,
		IMAPHost: strings.TrimSpace(input.Creds.IMAPHost), IMAPPort: input.Creds.IMAPPort, IMAPTLS: input.Creds.IMAPTLS,
		SMTPHost: strings.TrimSpace(input.Creds.SMTPHost), SMTPPort: input.Creds.SMTPPort, SMTPTLS: input.Creds.SMTPTLS,
		Username: strings.TrimSpace(input.Creds.Username), PasswordEncrypted: encryptedPassword, CreatedAt: createdAt,
	}
	alias := aliasRow{ID: aliasID, MailboxID: mailboxID, Email: strings.ToLower(strings.TrimSpace(input.AliasEmail)), Enabled: true, CreatedAt: createdAt}
	token := tokenRow{ID: tokenID, AliasID: &aliasID, Name: strings.TrimSpace(input.TokenName), TokenHash: input.TokenHash, Scopes: append([]string(nil), input.Scopes...), CreatedAt: createdAt}

	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return OnboardMailboxResult{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailboxes
(id,name,enabled,imap_host,imap_port,imap_tls,smtp_host,smtp_port,smtp_tls,username,password_encrypted,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		mailbox.ID, mailbox.Name, mailbox.Enabled, mailbox.IMAPHost, mailbox.IMAPPort, mailbox.IMAPTLS,
		mailbox.SMTPHost, mailbox.SMTPPort, mailbox.SMTPTLS, mailbox.Username, mailbox.PasswordEncrypted, mailbox.CreatedAt); err != nil {
		return OnboardMailboxResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO aliases(id,mailbox_id,email,enabled,created_at) VALUES($1,$2,$3,$4,$5)`,
		alias.ID, alias.MailboxID, alias.Email, alias.Enabled, alias.CreatedAt); err != nil {
		return OnboardMailboxResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tokens(id,alias_id,name,token_hash,scopes,created_at) VALUES($1,$2,$3,$4,$5,$6)`,
		token.ID, token.AliasID, token.Name, token.TokenHash, pq.Array(token.Scopes), token.CreatedAt); err != nil {
		return OnboardMailboxResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OnboardMailboxResult{}, err
	}
	return OnboardMailboxResult{
		Mailbox: Mailbox{
			ID: mailbox.ID, Name: mailbox.Name, Enabled: mailbox.Enabled, CreatedAt: mailbox.CreatedAt,
			Creds: MailboxCreds{
				IMAPHost: mailbox.IMAPHost, IMAPPort: mailbox.IMAPPort, IMAPTLS: mailbox.IMAPTLS,
				SMTPHost: mailbox.SMTPHost, SMTPPort: mailbox.SMTPPort, SMTPTLS: mailbox.SMTPTLS,
				Username: mailbox.Username, Password: input.Creds.Password,
			},
		},
		AliasID: aliasID, TokenID: tokenID,
	}, nil
}

// ProvisionMailboxAgent creates an alias credential for an existing enabled
// mailbox. Replacement revokes only active credentials with the exact alias and
// token name, and the whole operation commits atomically.
func (p *Postgres) ProvisionMailboxAgent(ctx context.Context, input ProvisionMailboxAgentInput) (ProvisionMailboxAgentResult, error) {
	mailboxName := strings.TrimSpace(input.MailboxName)
	aliasEmail := strings.ToLower(strings.TrimSpace(input.AliasEmail))
	tokenName := strings.TrimSpace(input.TokenName)
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return ProvisionMailboxAgentResult{}, err
	}
	defer tx.Rollback()

	var mailboxID string
	if err := tx.QueryRowContext(ctx, `SELECT id::text FROM mailboxes WHERE name=$1 AND enabled=TRUE FOR UPDATE`, mailboxName).Scan(&mailboxID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProvisionMailboxAgentResult{}, ErrNotFound
		}
		return ProvisionMailboxAgentResult{}, err
	}
	var aliasID string
	var aliasEnabled bool
	err = tx.QueryRowContext(ctx, `SELECT id::text,enabled FROM aliases WHERE mailbox_id=$1 AND email=$2 FOR UPDATE`, mailboxID, aliasEmail).Scan(&aliasID, &aliasEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		aliasID = uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO aliases(id,mailbox_id,email,enabled,created_at) VALUES($1,$2,$3,TRUE,$4)`, aliasID, mailboxID, aliasEmail, time.Now().UTC()); err != nil {
			return ProvisionMailboxAgentResult{}, err
		}
	} else if err != nil {
		return ProvisionMailboxAgentResult{}, err
	} else if !aliasEnabled {
		return ProvisionMailboxAgentResult{}, fmt.Errorf("%w: alias is disabled", ErrUnauthorized)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id::text FROM tokens WHERE alias_id=$1 AND name=$2 AND revoked_at IS NULL FOR UPDATE`, aliasID, tokenName)
	if err != nil {
		return ProvisionMailboxAgentResult{}, err
	}
	var activeTokenIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return ProvisionMailboxAgentResult{}, err
		}
		activeTokenIDs = append(activeTokenIDs, id)
	}
	if err := rows.Close(); err != nil {
		return ProvisionMailboxAgentResult{}, err
	}
	if err := rows.Err(); err != nil {
		return ProvisionMailboxAgentResult{}, err
	}
	if len(activeTokenIDs) > 0 && !input.Replace {
		return ProvisionMailboxAgentResult{}, ErrAgentCredentialExists
	}
	if input.Replace && len(activeTokenIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE tokens SET revoked_at=now() WHERE alias_id=$1 AND name=$2 AND revoked_at IS NULL`, aliasID, tokenName); err != nil {
			return ProvisionMailboxAgentResult{}, err
		}
	}
	tokenID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO tokens(id,alias_id,name,token_hash,scopes,created_at) VALUES($1,$2,$3,$4,$5,$6)`,
		tokenID, aliasID, tokenName, input.TokenHash, pq.Array(input.Scopes), time.Now().UTC()); err != nil {
		return ProvisionMailboxAgentResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProvisionMailboxAgentResult{}, err
	}
	return ProvisionMailboxAgentResult{
		MailboxID: mailboxID, MailboxName: mailboxName, AliasID: aliasID, AliasEmail: aliasEmail, TokenID: tokenID,
	}, nil
}

func (p *Postgres) ListMailboxes(ctx context.Context) ([]Mailbox, error) {
	var rows []mailboxRow
	if err := p.bun().NewSelect().Model(&rows).Order("created_at ASC", "name ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]Mailbox, 0, len(rows))
	for _, row := range rows {
		mailbox, err := p.mailboxFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("load mailbox %s credentials: %w", row.ID, err)
		}
		out = append(out, mailbox)
	}
	return out, nil
}

func (p *Postgres) GetMailboxByName(ctx context.Context, name string) (Mailbox, error) {
	var row mailboxRow
	err := p.bun().NewSelect().Model(&row).Where("name = ?", name).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, ErrNotFound
	}
	if err != nil {
		return Mailbox{}, err
	}
	mailbox, err := p.mailboxFromRow(row)
	if err != nil {
		return Mailbox{}, fmt.Errorf("load mailbox %s credentials: %w", row.ID, err)
	}
	return mailbox, nil
}

func (p *Postgres) SetMailboxEnabled(ctx context.Context, name string, enabled bool) error {
	res, err := p.bun().NewUpdate().Model((*mailboxRow)(nil)).Set("enabled = ?", enabled).Where("name = ?", strings.TrimSpace(name)).Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) GetFolderCheckpoint(ctx context.Context, mailboxID, folder string) (uint32, uint32, string, error) {
	folder = toValidUTF8(folder)
	var row folderRow
	err := p.bun().NewSelect().Model(&row).Column("id", "uidvalidity", "last_seen_uid").Where("mailbox_id = ?", mailboxID).Where("name = ?", folder).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, "", nil
	}
	if err != nil {
		return 0, 0, "", err
	}
	return uint32(row.UIDValidity), uint32(row.LastSeenUID), row.ID, nil
}

func (p *Postgres) UpsertFolderCheckpoint(ctx context.Context, mailboxID, folder string, uidValidity uint32, lastUID uint32) (string, error) {
	folder = toValidUTF8(folder)
	row := folderRow{
		ID:          uuid.NewString(),
		MailboxID:   mailboxID,
		Name:        folder,
		UIDValidity: int64(uidValidity),
		LastSeenUID: int64(lastUID),
		UpdatedAt:   time.Now().UTC(),
	}
	_, err := p.bun().NewInsert().
		Model(&row).
		On("CONFLICT (mailbox_id, name) DO UPDATE").
		Set("last_seen_uid = CASE WHEN folder_row.uidvalidity = EXCLUDED.uidvalidity THEN GREATEST(folder_row.last_seen_uid, EXCLUDED.last_seen_uid) ELSE EXCLUDED.last_seen_uid END").
		Set("uidvalidity = EXCLUDED.uidvalidity").
		Set("updated_at = now()").
		Returning("id").
		Exec(ctx)
	return row.ID, err
}

func (p *Postgres) ListAliases(ctx context.Context, mailboxID string) ([]router.Alias, error) {
	var rows []aliasRow
	if err := p.bun().NewSelect().Model(&rows).Column("id", "email", "enabled").Where("mailbox_id = ?", mailboxID).Order("email ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]router.Alias, 0, len(rows))
	for _, row := range rows {
		out = append(out, router.Alias{ID: row.ID, Email: row.Email, Enabled: row.Enabled})
	}
	return out, nil
}

func (p *Postgres) AddAlias(ctx context.Context, mailboxID, email string) error {
	row := aliasRow{
		ID:        uuid.NewString(),
		MailboxID: mailboxID,
		Email:     strings.ToLower(strings.TrimSpace(email)),
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
	}
	_, err := p.bun().NewInsert().Model(&row).On("CONFLICT (mailbox_id, email) DO NOTHING").Exec(ctx)
	return err
}

func (p *Postgres) MintToken(ctx context.Context, aliasID *string, name string, tokenHash string, scopes []string) (string, error) {
	id := uuid.NewString()
	row := tokenRow{
		ID:        id,
		AliasID:   aliasID,
		Name:      name,
		TokenHash: tokenHash,
		Scopes:    scopes,
		CreatedAt: time.Now().UTC(),
	}
	_, err := p.bun().NewInsert().Model(&row).Exec(ctx)
	return id, err
}

func (p *Postgres) RevokeToken(ctx context.Context, tokenID string) error {
	res, err := p.bun().NewUpdate().Model((*tokenRow)(nil)).Set("revoked_at = now()").Where("id = ?", tokenID).Where("revoked_at IS NULL").Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) AuthenticateToken(ctx context.Context, tokenHash string) (model.TokenClaims, error) {
	var id string
	var aliasID sql.NullString
	var scopes []string
	err := p.DB.QueryRowContext(ctx, `SELECT t.id::text, t.alias_id::text, t.scopes
FROM tokens t
LEFT JOIN aliases a ON a.id=t.alias_id
LEFT JOIN mailboxes m ON m.id=a.mailbox_id
WHERE t.token_hash=$1
  AND t.revoked_at IS NULL
  AND (t.alias_id IS NULL OR (a.enabled=true AND m.enabled=true))`, tokenHash).Scan(&id, &aliasID, pq.Array(&scopes))
	if errors.Is(err, sql.ErrNoRows) {
		return model.TokenClaims{}, ErrUnauthorized
	}
	if err != nil {
		return model.TokenClaims{}, err
	}
	m := map[model.Scope]bool{}
	for _, s := range scopes {
		m[model.Scope(s)] = true
	}
	var aid *string
	if aliasID.Valid {
		aid = &aliasID.String
	}
	return model.TokenClaims{TokenID: id, AliasID: aid, Scopes: m}, nil
}

func (p *Postgres) InsertMessage(ctx context.Context, mailboxID, folderID string, msg model.ParsedEmail, audience []string) (string, bool, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	id := uuid.NewString()
	fromName := sanitizeOptionalString(msg.FromName)
	subject := sanitizeOptionalString(msg.Subject)
	snippet := sanitizeOptionalString(msg.Snippet)
	inReplyTo := sanitizeOptionalString(msg.InReplyTo)
	references := sanitizeOptionalString(msg.References)
	res, err := tx.ExecContext(ctx, `INSERT INTO messages (id,mailbox_id,folder_id,imap_uid,uidvalidity,message_id,internal_date,sent_date,from_email,from_name,subject,snippet,raw_headers,has_attachments,spam_flag,body_state,in_reply_to,"references",created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'fetched',$16,$17,now())
ON CONFLICT (folder_id,uidvalidity,imap_uid) DO NOTHING`, id, mailboxID, folderID, msg.IMAPUID, msg.IMAPUIDValidity, msg.RFCMessageID, msg.InternalDate, msg.SentDate, msg.FromEmail, fromName, subject, snippet, toValidUTF8(msg.RawHeaders), msg.HasAttachment, msg.SpamFlag, inReplyTo, references)
	if err != nil {
		return "", false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT id::text FROM messages WHERE folder_id=$1 AND uidvalidity=$2 AND imap_uid=$3`, folderID, msg.IMAPUIDValidity, msg.IMAPUID).Scan(&existing)
		if err != nil {
			return "", false, err
		}
		if err := tx.Commit(); err != nil {
			return "", false, err
		}
		return existing, false, nil
	}

	for _, r := range msg.Recipients {
		_, err := tx.ExecContext(ctx, `INSERT INTO message_recipients (message_id,type,email) VALUES ($1,$2,$3)`, id, r.Type, strings.ToLower(r.Email))
		if err != nil {
			return "", false, err
		}
	}
	for _, aliasID := range audience {
		_, err := tx.ExecContext(ctx, `INSERT INTO message_audience (message_id,alias_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, aliasID)
		if err != nil {
			return "", false, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO message_bodies (message_id,text_plain,text_html,extracted_text,fetched_at) VALUES ($1,$2,$3,$4,now())`, id, sanitizeOptionalStringToValue(msg.TextPlain), sanitizeOptionalStringToValue(msg.TextHTML), toValidUTF8(msg.ExtractedText))
	if err != nil {
		return "", false, err
	}
	if len(msg.RawRFC822) > 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO message_raw (message_id,raw_rfc822,created_at) VALUES ($1,$2,now()) ON CONFLICT (message_id) DO NOTHING`, id, msg.RawRFC822)
		if err != nil {
			return "", false, err
		}
	}
	for idx, a := range msg.Attachments {
		_, err := tx.ExecContext(ctx, `INSERT INTO attachments (id,message_id,ordinal,filename,mime,size_bytes,storage_path,created_at) VALUES ($1,$2,$3,$4,$5,$6,NULL,now())`, uuid.NewString(), id, idx, toValidUTF8(a.Filename), a.MIME, a.SizeBytes)
		if err != nil {
			return "", false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return id, true, nil
}

func (p *Postgres) UpdateAttachmentStorage(ctx context.Context, messageID string, ordinal int, storagePath string) error {
	if ordinal < 0 {
		return fmt.Errorf("invalid attachment ordinal: %d", ordinal)
	}
	res, err := p.bun().NewUpdate().Model((*attachmentRow)(nil)).Set("storage_path = ?", storagePath).Where("message_id = ?", messageID).Where("ordinal = ?", ordinal).Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) SaveDigest(ctx context.Context, messageID string, digest model.Digest) error {
	bullets := make([]string, len(digest.SummaryBullets))
	for i, s := range digest.SummaryBullets {
		bullets[i] = toValidUTF8(s)
	}
	entities := digest.ExtractedEntities
	if len(entities) > 0 {
		entities = json.RawMessage(toValidUTF8(string(entities)))
	}
	row := digestRow{
		MessageID:         messageID,
		ModelName:         toValidUTF8(digest.ModelName),
		SummaryShort:      toValidUTF8(digest.SummaryShort),
		SummaryBullets:    bullets,
		Intent:            toValidUTF8(digest.Intent),
		Priority:          toValidUTF8(digest.Priority),
		RequiredAction:    toValidUTF8(digest.RequiredAction),
		SuggestedNextStep: toValidUTF8(digest.SuggestedNextStep),
		ExtractedEntities: entities,
		Confidence:        digest.Confidence,
		CreatedAt:         time.Now().UTC(),
	}
	_, err := p.bun().NewInsert().
		Model(&row).
		On("CONFLICT (message_id) DO UPDATE").
		Set("model_name = EXCLUDED.model_name").
		Set("summary_short = EXCLUDED.summary_short").
		Set("summary_bullets = EXCLUDED.summary_bullets").
		Set("intent = EXCLUDED.intent").
		Set("priority = EXCLUDED.priority").
		Set("required_action = EXCLUDED.required_action").
		Set("suggested_next_step = EXCLUDED.suggested_next_step").
		Set("extracted_entities = EXCLUDED.extracted_entities").
		Set("confidence = EXCLUDED.confidence").
		Set("created_at = now()").
		Exec(ctx)
	return err
}

func (p *Postgres) SaveMessageEmbeddings(ctx context.Context, messageID, modelName string, segments []EmbeddingSegment, embeddings [][]float32) error {
	if len(segments) != len(embeddings) {
		return fmt.Errorf("segments and embeddings size mismatch")
	}
	if len(segments) == 0 {
		return nil
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, seg := range segments {
		if len(embeddings[i]) == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_embeddings (message_id,segment,chunk_index,model_name,source_text,embedding,created_at)
VALUES ($1,$2,$3,$4,$5,$6::vector,now())
ON CONFLICT (message_id,segment,chunk_index) DO UPDATE SET model_name=EXCLUDED.model_name,source_text=EXCLUDED.source_text,embedding=EXCLUDED.embedding,created_at=now()`,
			messageID,
			toValidUTF8(seg.Segment),
			seg.ChunkIndex,
			toValidUTF8(modelName),
			toValidUTF8(seg.Text),
			vectorLiteral(embeddings[i]),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (p *Postgres) QueueEmbeddingJobs(ctx context.Context, messageID, modelName string, segments []EmbeddingSegment) error {
	if len(segments) == 0 {
		return nil
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, seg := range segments {
		if strings.TrimSpace(seg.Text) == "" {
			continue
		}
		now := time.Now().UTC()
		row := embeddingJobRow{
			ID:            uuid.NewString(),
			MessageID:     messageID,
			Segment:       toValidUTF8(seg.Segment),
			ChunkIndex:    seg.ChunkIndex,
			ModelName:     toValidUTF8(modelName),
			SourceText:    toValidUTF8(seg.Text),
			Status:        "pending",
			Attempts:      0,
			NextAttemptAt: now,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		_, err := p.bun().NewInsert().
			Model(&row).
			Conn(tx).
			On("CONFLICT (message_id, segment, chunk_index, model_name) DO UPDATE").
			Set("source_text = EXCLUDED.source_text").
			Set("status = 'pending'").
			Set("provider_batch_id = NULL").
			Set("next_attempt_at = now()").
			Set("lease_until = NULL").
			Set("updated_at = now()").
			Exec(ctx)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (p *Postgres) ClaimEmbeddingJobs(ctx context.Context, mailboxID, modelName string, limit int, lease time.Duration) ([]EmbeddingJob, error) {
	if limit <= 0 {
		limit = 96
	}
	if lease <= 0 {
		lease = 10 * time.Minute
	}
	rows, err := p.DB.QueryContext(ctx, `WITH claim AS (
SELECT j.id
FROM embedding_jobs j
JOIN messages m ON m.id=j.message_id
JOIN mailboxes mb ON mb.id=m.mailbox_id
WHERE j.model_name=$1
  AND m.mailbox_id=$2
  AND mb.enabled=true
  AND j.next_attempt_at<=now()
  AND (j.status='pending' OR (j.status='processing' AND (j.lease_until IS NULL OR j.lease_until < now())))
ORDER BY j.next_attempt_at ASC, j.created_at ASC
FOR UPDATE SKIP LOCKED
LIMIT $3
)
UPDATE embedding_jobs j
SET status='processing', lease_until=now()+($4::bigint * interval '1 second'), updated_at=now()
FROM claim
WHERE j.id=claim.id
RETURNING j.id::text,j.message_id::text,j.segment,j.chunk_index,j.source_text`,
		modelName, mailboxID, limit, int64(lease.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EmbeddingJob{}
	for rows.Next() {
		var job EmbeddingJob
		if err := rows.Scan(&job.ID, &job.MessageID, &job.Segment.Segment, &job.Segment.ChunkIndex, &job.Segment.Text); err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func (p *Postgres) MarkEmbeddingJobsDone(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.bun().NewUpdate().
		Model((*embeddingJobRow)(nil)).
		Set("status = 'done'").
		Set("provider_batch_id = NULL").
		Set("lease_until = NULL").
		Set("last_error = NULL").
		Set("updated_at = now()").
		Where("id IN (?)", bun.In(ids)).
		Exec(ctx)
	return err
}

func (p *Postgres) MarkEmbeddingJobsRetry(ctx context.Context, ids []string, lastErr string, retryAfter time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	if retryAfter <= 0 {
		retryAfter = 5 * time.Minute
	}
	_, err := p.bun().NewUpdate().
		Model((*embeddingJobRow)(nil)).
		Set("status = 'pending'").
		Set("provider_batch_id = NULL").
		Set("attempts = attempts + 1").
		Set("next_attempt_at = now() + (? * interval '1 second')", int64(retryAfter.Seconds())).
		Set("lease_until = NULL").
		Set("last_error = ?", toValidUTF8(lastErr)).
		Set("updated_at = now()").
		Where("id IN (?)", bun.In(ids)).
		Exec(ctx)
	return err
}

func (p *Postgres) QueueSummarizeJob(ctx context.Context, messageID string, payload []byte, lastErr string) error {
	if strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("message id is required")
	}
	if len(payload) == 0 {
		return fmt.Errorf("summarize payload is required")
	}
	now := time.Now().UTC()
	row := summarizeJobRow{
		ID:            uuid.NewString(),
		MessageID:     messageID,
		Payload:       append([]byte(nil), payload...),
		Status:        "pending",
		Attempts:      0,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if trimmed := strings.TrimSpace(lastErr); trimmed != "" {
		sanitized := toValidUTF8(trimmed)
		row.LastError = &sanitized
	}
	_, err := p.bun().NewInsert().
		Model(&row).
		On("CONFLICT (message_id) DO UPDATE").
		Set("payload = EXCLUDED.payload").
		Set("status = 'pending'").
		Set("next_attempt_at = now()").
		Set("lease_until = NULL").
		Set("last_error = EXCLUDED.last_error").
		Set("updated_at = now()").
		Exec(ctx)
	return err
}

func (p *Postgres) ClaimSummarizeJobs(ctx context.Context, mailboxID string, limit int, lease time.Duration) ([]SummarizeJob, error) {
	if limit <= 0 {
		limit = 32
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	rows, err := p.DB.QueryContext(ctx, `WITH claim AS (
SELECT j.id
FROM summarize_jobs j
JOIN messages m ON m.id=j.message_id
JOIN mailboxes mb ON mb.id=m.mailbox_id
WHERE j.next_attempt_at<=now()
  AND m.mailbox_id=$1
  AND mb.enabled=true
  AND (j.status='pending' OR (j.status='processing' AND (j.lease_until IS NULL OR j.lease_until < now())))
ORDER BY j.next_attempt_at ASC, j.created_at ASC
FOR UPDATE SKIP LOCKED
LIMIT $2
)
UPDATE summarize_jobs j
SET status='processing', lease_until=now()+($3::bigint * interval '1 second'), updated_at=now()
FROM claim
WHERE j.id=claim.id
RETURNING j.id::text,j.message_id::text,j.payload::text`,
		mailboxID, limit, int64(lease.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SummarizeJob{}
	for rows.Next() {
		var job SummarizeJob
		var payload string
		if err := rows.Scan(&job.ID, &job.MessageID, &payload); err != nil {
			return nil, err
		}
		job.Payload = []byte(payload)
		out = append(out, job)
	}
	return out, rows.Err()
}

func (p *Postgres) MarkSummarizeJobsDone(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.bun().NewUpdate().
		Model((*summarizeJobRow)(nil)).
		Set("status = 'done'").
		Set("lease_until = NULL").
		Set("last_error = NULL").
		Set("updated_at = now()").
		Where("id IN (?)", bun.In(ids)).
		Exec(ctx)
	return err
}

func (p *Postgres) MarkSummarizeJobsRetry(ctx context.Context, ids []string, lastErr string, retryAfter time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	if retryAfter <= 0 {
		retryAfter = 2 * time.Minute
	}
	_, err := p.bun().NewUpdate().
		Model((*summarizeJobRow)(nil)).
		Set("status = 'pending'").
		Set("attempts = attempts + 1").
		Set("next_attempt_at = now() + (? * interval '1 second')", int64(retryAfter.Seconds())).
		Set("lease_until = NULL").
		Set("last_error = ?", toValidUTF8(lastErr)).
		Set("updated_at = now()").
		Where("id IN (?)", bun.In(ids)).
		Exec(ctx)
	return err
}

func (p *Postgres) MarkEmbeddingJobsSubmitted(ctx context.Context, ids []string, batchID string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.bun().NewUpdate().
		Model((*embeddingJobRow)(nil)).
		Set("status = 'submitted'").
		Set("provider_batch_id = ?", toValidUTF8(batchID)).
		Set("lease_until = NULL").
		Set("updated_at = now()").
		Where("id IN (?)", bun.In(ids)).
		Exec(ctx)
	return err
}

func (p *Postgres) ListEmbeddingSubmittedBatchIDs(ctx context.Context, mailboxID, modelName string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT j.provider_batch_id
FROM embedding_jobs j
JOIN messages m ON m.id=j.message_id
JOIN mailboxes mb ON mb.id=m.mailbox_id
WHERE j.model_name=$1
  AND m.mailbox_id=$2
  AND mb.enabled=true
  AND j.status='submitted'
  AND j.provider_batch_id IS NOT NULL
GROUP BY j.provider_batch_id
ORDER BY min(j.updated_at) ASC
LIMIT $3`, modelName, mailboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (p *Postgres) GetEmbeddingJobsByBatch(ctx context.Context, mailboxID, batchID string) ([]EmbeddingJob, error) {
	rows, err := p.DB.QueryContext(ctx, `SELECT j.id::text,j.message_id::text,j.segment,j.chunk_index,j.source_text
FROM embedding_jobs j
JOIN messages m ON m.id=j.message_id
WHERE j.provider_batch_id=$1 AND j.status='submitted' AND m.mailbox_id=$2`, batchID, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EmbeddingJob{}
	for rows.Next() {
		var j EmbeddingJob
		if err := rows.Scan(&j.ID, &j.MessageID, &j.Segment.Segment, &j.Segment.ChunkIndex, &j.Segment.Text); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (p *Postgres) GetEmbeddingJobsByIDs(ctx context.Context, mailboxID string, ids []string) ([]EmbeddingJob, error) {
	if len(ids) == 0 {
		return []EmbeddingJob{}, nil
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT j.id::text,j.message_id::text,j.segment,j.chunk_index,j.source_text
FROM embedding_jobs j
JOIN messages m ON m.id=j.message_id
WHERE j.id = ANY($1::uuid[]) AND m.mailbox_id=$2`, pq.Array(ids), mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EmbeddingJob{}
	for rows.Next() {
		var j EmbeddingJob
		if err := rows.Scan(&j.ID, &j.MessageID, &j.Segment.Segment, &j.Segment.ChunkIndex, &j.Segment.Text); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func inboxFolderNames(folder string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(folder)) {
	case "", "inbox":
		return []string{"inbox"}, nil
	case "junk", "spam":
		return []string{"junk", "spam", "junk e-mail", "junk email", "bulk mail", "bulk"}, nil
	case "sent":
		return []string{"sent", "sent items", "sent mail"}, nil
	case "all":
		return nil, nil
	default:
		// Any other folder name is passed through literally so folders discovered via
		// ListFolders (e.g. Drafts, Archive, or any IMAP-native folder name) remain
		// browsable rather than rejected.
		name := strings.ToLower(strings.TrimSpace(folder))
		if name == "" {
			return []string{"inbox"}, nil
		}
		return []string{name}, nil
	}
}

func searchFolderNames(folder string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(folder)) {
	case "", "all":
		return nil, nil
	case "inbox":
		return []string{"inbox"}, nil
	case "junk", "spam":
		return []string{"junk", "spam", "junk e-mail", "junk email", "bulk mail", "bulk"}, nil
	case "sent":
		return []string{"sent", "sent items", "sent mail"}, nil
	default:
		return nil, fmt.Errorf("invalid folder %q (allowed: all, inbox, junk|spam, sent)", folder)
	}
}

var searchStopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "at": {}, "be": {}, "for": {}, "from": {}, "in": {}, "is": {}, "it": {}, "mail": {}, "mails": {}, "message": {}, "messages": {}, "my": {}, "of": {}, "on": {}, "or": {}, "that": {}, "the": {}, "to": {}, "was": {}, "were": {}, "with": {}, "email": {}, "emails": {}, "where": {}, "what": {}, "which": {}, "who": {}, "why": {}, "how": {}, "junk": {}, "spam": {}, "inbox": {}, "sent": {},
}

func searchTokens(query string) []string {
	parts := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, p := range parts {
		if len(p) < 2 {
			continue
		}
		if _, skip := searchStopwords[p]; skip {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func addSearchFilters(qb *strings.Builder, args *[]any, folderNames []string, query string) {
	if len(folderNames) > 0 {
		*args = append(*args, pq.Array(folderNames))
		fmt.Fprintf(qb, " AND lower(f.name) = ANY($%d)", len(*args))
	}
	tokens := searchTokens(query)
	if len(tokens) == 0 {
		return
	}
	const haystack = "LOWER(COALESCE(m.subject,'') || ' ' || COALESCE(m.from_email::text,'') || ' ' || COALESCE(mb.extracted_text,'') || ' ' || COALESCE(md.summary_short,''))"
	for _, tok := range tokens {
		*args = append(*args, "%"+tok+"%")
		fmt.Fprintf(qb, " AND %s LIKE $%d", haystack, len(*args))
	}
}

func listBaseSelect(db *bun.DB, aliasID string, unreadOnly bool, folderNames []string) *bun.SelectQuery {
	q := db.NewSelect().
		TableExpr("messages AS m").
		ColumnExpr("m.id AS msg_id").
		ColumnExpr("f.name AS folder").
		ColumnExpr("m.internal_date AS received_at").
		ColumnExpr("m.from_name AS from_name").
		ColumnExpr("m.from_email AS from_email").
		ColumnExpr("m.subject AS subject").
		ColumnExpr("(ma2.message_id IS NOT NULL) AS seen").
		ColumnExpr("m.body_state AS body_state").
		ColumnExpr("m.message_id AS thread_message_id").
		ColumnExpr("m.in_reply_to AS thread_in_reply_to").
		ColumnExpr("m.\"references\" AS thread_references").
		ColumnExpr("COALESCE(md.model_name, '') AS model_name").
		ColumnExpr("COALESCE(md.summary_short, '') AS summary_short").
		ColumnExpr("COALESCE(md.summary_bullets, ARRAY[]::text[]) AS summary_bullets").
		ColumnExpr("COALESCE(md.intent, '') AS intent").
		ColumnExpr("COALESCE(md.priority, '') AS priority").
		ColumnExpr("COALESCE(md.required_action, '') AS required_action").
		ColumnExpr("COALESCE(md.suggested_next_step, '') AS suggested_next_step").
		ColumnExpr("COALESCE(md.extracted_entities, '{}'::jsonb) AS extracted_entities").
		ColumnExpr("COALESCE(md.confidence, 0) AS confidence").
		ColumnExpr("(SELECT count(*) FROM attachments AS a WHERE a.message_id = m.id)::int AS attachments_count").
		Join("JOIN folders AS f ON f.id = m.folder_id").
		Join("JOIN message_audience AS aud ON aud.message_id = m.id").
		Join("LEFT JOIN message_digest AS md ON md.message_id = m.id").
		Join("LEFT JOIN message_ack AS ma2 ON ma2.message_id = m.id AND ma2.alias_id = aud.alias_id").
		Where("aud.alias_id = ?", aliasID)
	if unreadOnly {
		q = q.Where("ma2.message_id IS NULL")
	}
	if len(folderNames) > 0 {
		q = q.Where("lower(f.name) IN (?)", bun.In(folderNames))
	}
	return q
}

func addSearchFiltersSelect(q *bun.SelectQuery, folderNames []string, query string) *bun.SelectQuery {
	if len(folderNames) > 0 {
		q = q.Where("lower(f.name) IN (?)", bun.In(folderNames))
	}
	tokens := searchTokens(query)
	if len(tokens) == 0 {
		return q
	}
	const haystack = "LOWER(COALESCE(m.subject,'') || ' ' || COALESCE(m.from_email::text,'') || ' ' || COALESCE(mb.extracted_text,'') || ' ' || COALESCE(md.summary_short,''))"
	for _, tok := range tokens {
		q = q.Where(haystack+" LIKE ?", "%"+tok+"%")
	}
	return q
}

type searchExprBuilder struct {
	args []any
}

func (b *searchExprBuilder) add(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

func (p *Postgres) structuredSearchAuthFilter(claims model.TokenClaims, b *searchExprBuilder) (string, error) {
	if claims.AliasID == nil {
		if !claims.IsAdmin() || claims.MailboxID == nil {
			return "", ErrUnauthorized
		}
		return "m.mailbox_id = " + b.add(*claims.MailboxID), nil
	}
	return "aud.alias_id = " + b.add(*claims.AliasID), nil
}

func appendIfLike(parts []string, field string, values []string, b *searchExprBuilder) []string {
	for _, raw := range values {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s ILIKE %s", field, b.add("%"+trimmed+"%")))
	}
	return parts
}

func appendRecipientExists(parts []string, recipientType string, values []string, b *searchExprBuilder) []string {
	for _, raw := range values {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf(`EXISTS (
SELECT 1
FROM message_recipients mrp
WHERE mrp.message_id = m.id
  AND lower(mrp.type) = %s
  AND mrp.email::text ILIKE %s
)`, b.add(strings.ToLower(recipientType)), b.add("%"+trimmed+"%")))
	}
	return parts
}

func answeredClause() string {
	return `EXISTS (
SELECT 1
FROM messages sm
JOIN folders sf ON sf.id = sm.folder_id
WHERE sm.mailbox_id = m.mailbox_id
  AND lower(sf.name) = 'sent'
  AND (
    (NULLIF(sm.in_reply_to, '') IS NOT NULL AND NULLIF(sm.in_reply_to, '') = NULLIF(m.message_id, ''))
    OR (
      NULLIF(m.message_id, '') IS NOT NULL
      AND COALESCE(sm."references", '') ILIKE '%' || NULLIF(m.message_id, '') || '%'
    )
  )
)`
}

func buildIntentCondition(intent model.SearchIntent, b *searchExprBuilder) string {
	parts := []string{}
	const haystack = "LOWER(COALESCE(m.subject,'') || ' ' || COALESCE(m.from_name,'') || ' ' || COALESCE(m.from_email::text,'') || ' ' || COALESCE(mb.extracted_text,'') || ' ' || COALESCE(md.summary_short,''))"
	textTerms := searchTokens(strings.TrimSpace(intent.Text))
	for _, tok := range textTerms {
		parts = append(parts, fmt.Sprintf("%s LIKE %s", haystack, b.add("%"+tok+"%")))
	}
	for _, raw := range intent.BodyTerms {
		for _, tok := range searchTokens(raw) {
			parts = append(parts, fmt.Sprintf("LOWER(COALESCE(mb.extracted_text,'')) LIKE %s", b.add("%"+tok+"%")))
		}
	}
	for _, raw := range intent.HeaderTerms.Subject {
		for _, tok := range searchTokens(raw) {
			parts = append(parts, fmt.Sprintf("LOWER(COALESCE(m.subject,'')) LIKE %s", b.add("%"+tok+"%")))
		}
	}
	for _, raw := range intent.HeaderTerms.From {
		for _, tok := range searchTokens(raw) {
			parts = append(parts, fmt.Sprintf("LOWER(COALESCE(m.from_name,'') || ' ' || COALESCE(m.from_email::text,'')) LIKE %s", b.add("%"+tok+"%")))
		}
	}
	for _, raw := range intent.HeaderTerms.To {
		for _, tok := range searchTokens(raw) {
			parts = append(parts, fmt.Sprintf(`EXISTS (
SELECT 1
FROM message_recipients mrp
WHERE mrp.message_id = m.id
  AND lower(mrp.type) = %s
  AND LOWER(COALESCE(mrp.email::text,'')) LIKE %s
)`, b.add("to"), b.add("%"+tok+"%")))
		}
	}
	return strings.Join(parts, " AND ")
}

func buildStructuredFilterCondition(filters model.SearchFilters, b *searchExprBuilder) (string, error) {
	parts := []string{}
	parts = appendIfLike(parts, "COALESCE(m.subject,'')", filters.Headers.Subject, b)
	parts = appendIfLike(parts, "COALESCE(m.from_name,'') || ' ' || COALESCE(m.from_email::text,'')", filters.Headers.From, b)
	parts = appendRecipientExists(parts, "to", filters.Headers.To, b)
	parts = appendIfLike(parts, "COALESCE(m.message_id,'')", filters.Headers.MessageID, b)
	keys := make([]string, 0, len(filters.Headers.Generic))
	for key := range filters.Headers.Generic {
		keys = append(keys, strings.ToLower(strings.TrimSpace(key)))
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, raw := range filters.Headers.Generic[key] {
			trimmed := strings.TrimSpace(raw)
			if trimmed == "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("LOWER(COALESCE(m.raw_headers,'')) LIKE %s", b.add("%"+strings.ToLower(key)+":%"+strings.ToLower(trimmed)+"%")))
		}
	}
	for _, flag := range filters.Flags.With {
		switch strings.ToLower(strings.TrimSpace(flag)) {
		case "seen":
			parts = append(parts, "ma2.message_id IS NOT NULL")
		case "answered":
			parts = append(parts, answeredClause())
		case "", "recent":
		default:
			return "", fmt.Errorf("unsupported search flag %q", flag)
		}
	}
	for _, flag := range filters.Flags.Without {
		switch strings.ToLower(strings.TrimSpace(flag)) {
		case "seen":
			parts = append(parts, "ma2.message_id IS NULL")
		case "answered":
			parts = append(parts, "NOT "+answeredClause())
		case "", "recent":
		default:
			return "", fmt.Errorf("unsupported search flag %q", flag)
		}
	}
	if filters.Date.InternalSince != nil {
		parts = append(parts, "m.internal_date >= "+b.add(*filters.Date.InternalSince))
	}
	if filters.Date.InternalBefore != nil {
		parts = append(parts, "m.internal_date < "+b.add(*filters.Date.InternalBefore))
	}
	if filters.Date.SentSince != nil {
		parts = append(parts, "m.sent_date >= "+b.add(*filters.Date.SentSince))
	}
	if filters.Date.SentBefore != nil {
		parts = append(parts, "m.sent_date < "+b.add(*filters.Date.SentBefore))
	}
	if filters.Size.Larger != nil {
		parts = append(parts, "COALESCE(octet_length(mr.raw_rfc822), 0) > "+b.add(*filters.Size.Larger))
	}
	if filters.Size.Smaller != nil {
		parts = append(parts, "COALESCE(octet_length(mr.raw_rfc822), 0) < "+b.add(*filters.Size.Smaller))
	}
	for _, not := range filters.Logic.Not {
		clause, err := buildStructuredSubqueryCondition(not, b)
		if err != nil {
			return "", err
		}
		if clause != "" {
			parts = append(parts, "NOT ("+clause+")")
		}
	}
	for _, orClause := range filters.Logic.Or {
		orParts := []string{}
		for _, sub := range orClause.Any {
			clause, err := buildStructuredSubqueryCondition(sub, b)
			if err != nil {
				return "", err
			}
			if clause != "" {
				orParts = append(orParts, "("+clause+")")
			}
		}
		if len(orParts) > 0 {
			parts = append(parts, "("+strings.Join(orParts, " OR ")+")")
		}
	}
	return strings.Join(parts, " AND "), nil
}

func buildStructuredSubqueryCondition(sub model.SearchSubquery, b *searchExprBuilder) (string, error) {
	parts := []string{}
	filterClause, err := buildStructuredFilterCondition(sub.Filters, b)
	if err != nil {
		return "", err
	}
	if filterClause != "" {
		parts = append(parts, filterClause)
	}
	intentClause := buildIntentCondition(sub.Intent, b)
	if intentClause != "" {
		parts = append(parts, intentClause)
	}
	return strings.Join(parts, " AND "), nil
}

func (p *Postgres) buildStructuredWhere(claims model.TokenClaims, req model.SearchRequest, includeIntent bool) (string, []any, error) {
	return p.buildStructuredWhereWithBase(claims, req, includeIntent, nil)
}

func (p *Postgres) buildStructuredWhereWithBase(claims model.TokenClaims, req model.SearchRequest, includeIntent bool, prefixArgs []any) (string, []any, error) {
	b := &searchExprBuilder{args: append([]any(nil), prefixArgs...)}
	parts := []string{}
	authClause, err := p.structuredSearchAuthFilter(claims, b)
	if err != nil {
		return "", nil, err
	}
	parts = append(parts, authClause)
	if folderNames, err := searchFolderNames(req.Folder); err != nil {
		return "", nil, err
	} else if len(folderNames) > 0 {
		parts = append(parts, "lower(f.name) = ANY("+b.add(pq.Array(folderNames))+")")
	}
	filterClause, err := buildStructuredFilterCondition(req.Filters, b)
	if err != nil {
		return "", nil, err
	}
	if filterClause != "" {
		parts = append(parts, filterClause)
	}
	if includeIntent {
		if intentClause := buildIntentCondition(req.Intent, b); intentClause != "" {
			parts = append(parts, intentClause)
		}
	}
	return strings.Join(parts, " AND "), b.args, nil
}

func scanListItemRows(rows []listItemRow) []model.ListItem {
	out := make([]model.ListItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, listItemFromRow(row))
	}
	return out
}

func (p *Postgres) ListInbox(ctx context.Context, claims model.TokenClaims, folder string, unreadOnly bool, limit int) ([]model.ListItem, error) {
	if claims.AliasID == nil && !claims.IsAdmin() {
		return nil, ErrUnauthorized
	}
	if limit <= 0 {
		limit = 20
	}
	if claims.IsAdmin() {
		return nil, fmt.Errorf("admin token must use alias-scoped token for inbox")
	}
	folderNames, err := inboxFolderNames(folder)
	if err != nil {
		return nil, err
	}
	var rows []listItemRow
	err = listBaseSelect(p.bun(), *claims.AliasID, unreadOnly, folderNames).
		OrderExpr("m.internal_date DESC").
		Limit(limit).
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	return scanListItemRows(rows), nil
}

func (p *Postgres) ListFolders(ctx context.Context, claims model.TokenClaims) ([]model.FolderSummary, error) {
	if claims.AliasID == nil && !claims.IsAdmin() {
		return nil, ErrUnauthorized
	}
	if claims.IsAdmin() {
		return nil, fmt.Errorf("admin token must use alias-scoped token for folders")
	}
	type folderCountRow struct {
		Name   string `bun:"name"`
		Total  int    `bun:"total"`
		Unread int    `bun:"unread"`
	}
	var rows []folderCountRow
	err := p.bun().NewSelect().
		TableExpr("messages AS m").
		ColumnExpr("f.name AS name").
		ColumnExpr("count(*)::int AS total").
		ColumnExpr("count(*) FILTER (WHERE ma2.message_id IS NULL)::int AS unread").
		Join("JOIN folders AS f ON f.id = m.folder_id").
		Join("JOIN message_audience AS aud ON aud.message_id = m.id").
		Join("LEFT JOIN message_ack AS ma2 ON ma2.message_id = m.id AND ma2.alias_id = aud.alias_id").
		Where("aud.alias_id = ?", *claims.AliasID).
		GroupExpr("f.name").
		OrderExpr("f.name ASC").
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]model.FolderSummary, 0, len(rows))
	for _, row := range rows {
		role, display := classifyFolder(row.Name)
		out = append(out, model.FolderSummary{
			Name:        row.Name,
			DisplayName: display,
			Role:        role,
			Total:       row.Total,
			Unread:      row.Unread,
		})
	}
	return out, nil
}

// classifyFolder maps a raw IMAP folder name to a stable role (used for icon/ordering in
// clients) and a human-friendly display name. Unrecognized folders keep their own name and
// role "other" so they remain browsable rather than hidden.
func classifyFolder(name string) (role string, display string) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "inbox":
		return "inbox", "Inbox"
	case "sent", "sent items", "sent mail":
		return "sent", "Sent"
	case "drafts", "draft":
		return "drafts", "Drafts"
	case "junk", "spam", "junk e-mail", "junk email", "bulk mail", "bulk":
		return "junk", "Junk"
	case "trash", "deleted items", "deleted messages":
		return "trash", "Trash"
	case "archive", "all mail":
		return "archive", "Archive"
	default:
		return "other", name
	}
}

func (p *Postgres) mustAliasVisible(ctx context.Context, claims model.TokenClaims, messageID string) error {
	if claims.IsAdmin() {
		return nil
	}
	if claims.AliasID == nil {
		return ErrUnauthorized
	}
	var ok bool
	err := p.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM message_audience WHERE message_id=$1 AND alias_id=$2)`, messageID, *claims.AliasID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrUnauthorized
	}
	return nil
}

func (p *Postgres) ReadMessage(ctx context.Context, claims model.TokenClaims, messageID string) (model.Message, error) {
	if err := p.mustAliasVisible(ctx, claims, messageID); err != nil {
		return model.Message{}, err
	}
	type readMessageRow struct {
		ID                string     `bun:"id"`
		InternalDate      time.Time  `bun:"internal_date"`
		SentDate          *time.Time `bun:"sent_date"`
		FromEmail         string     `bun:"from_email"`
		FromName          *string    `bun:"from_name"`
		Subject           *string    `bun:"subject"`
		Snippet           *string    `bun:"snippet"`
		BodyState         string     `bun:"body_state"`
		RFCMessageID      string     `bun:"rfc_message_id"`
		InReplyTo         *string    `bun:"in_reply_to"`
		References        *string    `bun:"references"`
		DigestMessageID   *string    `bun:"digest_message_id"`
		ModelName         string     `bun:"model_name"`
		SummaryShort      string     `bun:"summary_short"`
		SummaryBullets    []string   `bun:"summary_bullets,array"`
		Intent            string     `bun:"intent"`
		Priority          string     `bun:"priority"`
		RequiredAction    string     `bun:"required_action"`
		SuggestedNextStep string     `bun:"suggested_next_step"`
		ExtractedEntities []byte     `bun:"extracted_entities"`
		Confidence        float64    `bun:"confidence"`
	}
	var row readMessageRow
	err := p.bun().NewSelect().
		TableExpr("messages AS m").
		ColumnExpr("m.id AS id").
		ColumnExpr("m.internal_date AS internal_date").
		ColumnExpr("m.sent_date AS sent_date").
		ColumnExpr("m.from_email AS from_email").
		ColumnExpr("m.from_name AS from_name").
		ColumnExpr("m.subject AS subject").
		ColumnExpr("m.snippet AS snippet").
		ColumnExpr("m.body_state AS body_state").
		ColumnExpr("m.message_id AS rfc_message_id").
		ColumnExpr("m.in_reply_to AS in_reply_to").
		ColumnExpr("m.\"references\" AS references").
		ColumnExpr("md.message_id AS digest_message_id").
		ColumnExpr("COALESCE(md.model_name, '') AS model_name").
		ColumnExpr("COALESCE(md.summary_short, '') AS summary_short").
		ColumnExpr("COALESCE(md.summary_bullets, ARRAY[]::text[]) AS summary_bullets").
		ColumnExpr("COALESCE(md.intent, '') AS intent").
		ColumnExpr("COALESCE(md.priority, '') AS priority").
		ColumnExpr("COALESCE(md.required_action, '') AS required_action").
		ColumnExpr("COALESCE(md.suggested_next_step, '') AS suggested_next_step").
		ColumnExpr("COALESCE(md.extracted_entities, '{}'::jsonb) AS extracted_entities").
		ColumnExpr("COALESCE(md.confidence, 0) AS confidence").
		Join("LEFT JOIN message_digest AS md ON md.message_id = m.id").
		Where("m.id = ?", messageID).
		Limit(1).
		Scan(ctx, &row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Message{}, ErrNotFound
		}
		return model.Message{}, err
	}
	var m model.Message
	m.ID = row.ID
	m.InternalDate = row.InternalDate
	m.SentDate = row.SentDate
	m.FromEmail = row.FromEmail
	m.FromName = row.FromName
	m.Subject = row.Subject
	m.Snippet = row.Snippet
	m.BodyState = row.BodyState
	m.RFCMessageID = row.RFCMessageID
	m.InReplyTo = row.InReplyTo
	m.References = row.References
	if row.DigestMessageID != nil {
		m.Digest = &model.Digest{
			ModelName:         row.ModelName,
			SummaryShort:      row.SummaryShort,
			SummaryBullets:    row.SummaryBullets,
			Intent:            row.Intent,
			Priority:          row.Priority,
			RequiredAction:    row.RequiredAction,
			SuggestedNextStep: row.SuggestedNextStep,
			ExtractedEntities: row.ExtractedEntities,
			Confidence:        row.Confidence,
		}
	}
	m.Thread = model.Thread{MessageID: m.RFCMessageID, InReplyTo: m.InReplyTo, References: m.References}
	m.From = model.From{Name: m.FromName, Email: m.FromEmail}

	var attachments []attachmentRow
	if err := p.bun().NewSelect().
		Model(&attachments).
		Column("id", "filename", "mime", "size_bytes", "storage_path", "created_at").
		Where("message_id = ?", messageID).
		Order("ordinal ASC", "created_at ASC").
		Scan(ctx); err != nil {
		return model.Message{}, err
	}
	for _, row := range attachments {
		m.Attachments = append(m.Attachments, model.Attachment{
			ID:        row.ID,
			MessageID: row.MessageID,
			Filename:  derefString(row.Filename),
			MIME:      derefString(row.MIME),
			SizeBytes: row.SizeBytes,
			Storage:   row.StoragePath,
			CreatedAt: row.CreatedAt,
		})
	}
	return m, nil
}

func (p *Postgres) GetBody(ctx context.Context, claims model.TokenClaims, messageID string) (model.Body, error) {
	if err := p.mustAliasVisible(ctx, claims, messageID); err != nil {
		return model.Body{}, err
	}
	var row messageBodyRow
	err := p.bun().NewSelect().
		Model(&row).
		Column("message_id", "text_plain", "text_html", "extracted_text").
		Where("message_id = ?", messageID).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Body{}, ErrNotFound
		}
		return model.Body{}, err
	}
	return model.Body{
		MessageID:     row.MessageID,
		TextPlain:     row.TextPlain,
		TextHTML:      row.TextHTML,
		ExtractedText: row.ExtractedText,
	}, nil
}

func (p *Postgres) GetRawMessage(ctx context.Context, claims model.TokenClaims, messageID string) ([]byte, error) {
	if err := p.mustAliasVisible(ctx, claims, messageID); err != nil {
		return nil, err
	}
	var stored messageRawRow
	err := p.bun().NewSelect().
		Model(&stored).
		Column("raw_rfc822").
		Where("message_id = ?", messageID).
		Limit(1).
		Scan(ctx)
	if err == nil && len(stored.RawRFC822) > 0 {
		return stored.RawRFC822, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	msg, err := p.ReadMessage(ctx, claims, messageID)
	if err != nil {
		return nil, err
	}
	body, err := p.GetBody(ctx, claims, messageID)
	if err != nil {
		return nil, err
	}
	type rawRow struct {
		RawHeaders string `bun:"raw_headers"`
	}
	var row rawRow
	err = p.bun().NewSelect().
		TableExpr("messages AS m").
		ColumnExpr("m.raw_headers AS raw_headers").
		Where("m.id = ?", messageID).
		Limit(1).
		Scan(ctx, &row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	text := ""
	contentType := "text/plain; charset=utf-8"
	if body.TextPlain != nil && strings.TrimSpace(*body.TextPlain) != "" {
		text = *body.TextPlain
	} else if body.ExtractedText != nil && strings.TrimSpace(*body.ExtractedText) != "" {
		text = *body.ExtractedText
	} else if body.TextHTML != nil && strings.TrimSpace(*body.TextHTML) != "" {
		text = *body.TextHTML
		contentType = "text/html; charset=utf-8"
	}
	return synthesizeRawMessage(row.RawHeaders, msg, text, contentType), nil
}

func (p *Postgres) AckMessage(ctx context.Context, claims model.TokenClaims, messageID string) error {
	if claims.AliasID == nil {
		return ErrUnauthorized
	}
	if err := p.mustAliasVisible(ctx, claims, messageID); err != nil {
		return err
	}
	row := messageAckRow{
		MessageID: messageID,
		AliasID:   *claims.AliasID,
		CreatedAt: time.Now().UTC(),
	}
	_, err := p.bun().NewInsert().Model(&row).On("CONFLICT DO NOTHING").Exec(ctx)
	return err
}

func (p *Postgres) UnackMessage(ctx context.Context, claims model.TokenClaims, messageID string) error {
	if claims.AliasID == nil {
		return ErrUnauthorized
	}
	if err := p.mustAliasVisible(ctx, claims, messageID); err != nil {
		return err
	}
	_, err := p.bun().NewDelete().
		Model((*messageAckRow)(nil)).
		Where("message_id = ?", messageID).
		Where("alias_id = ?", *claims.AliasID).
		Exec(ctx)
	return err
}

func sanitizeRecipients(to []string) []string {
	out := make([]string, 0, len(to))
	for _, item := range to {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		out = append(out, toValidUTF8(trimmed))
	}
	return out
}

func parseOutboundRecipients(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

func outboundFromRow(row outboundMessageRow) OutboundMessage {
	return OutboundMessage{
		ID:             row.ID,
		MailboxID:      row.MailboxID,
		AliasID:        row.AliasID,
		FromEmail:      row.FromEmail,
		To:             parseOutboundRecipients(row.ToEmails),
		RawRFC822:      append([]byte(nil), row.RawRFC822...),
		IdempotencyKey: row.IdempotencyKey,
		Status:         row.Status,
		Attempts:       row.Attempts,
		LastError:      row.LastError,
		AcceptedAt:     row.AcceptedAt,
		SentAt:         row.SentAt,
	}
}

func (p *Postgres) QueueOutboundMessage(ctx context.Context, mailboxID, aliasID, from string, to []string, raw []byte, idempotencyKey string) (OutboundMessage, bool, error) {
	if strings.TrimSpace(mailboxID) == "" || strings.TrimSpace(aliasID) == "" {
		return OutboundMessage{}, false, fmt.Errorf("mailbox and alias are required")
	}
	if strings.TrimSpace(from) == "" {
		return OutboundMessage{}, false, fmt.Errorf("from is required")
	}
	recipients := sanitizeRecipients(to)
	if len(recipients) == 0 {
		return OutboundMessage{}, false, fmt.Errorf("at least one recipient is required")
	}
	if len(raw) == 0 {
		return OutboundMessage{}, false, fmt.Errorf("raw message is required")
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		key = uuid.NewString()
	}
	toJSON, err := json.Marshal(recipients)
	if err != nil {
		return OutboundMessage{}, false, err
	}
	now := time.Now().UTC()
	id := uuid.NewString()
	row := outboundMessageRow{
		ID:             id,
		MailboxID:      mailboxID,
		AliasID:        aliasID,
		FromEmail:      toValidUTF8(strings.TrimSpace(from)),
		ToEmails:       toJSON,
		RawRFC822:      append([]byte(nil), raw...),
		IdempotencyKey: toValidUTF8(key),
		Status:         "pending",
		Attempts:       0,
		NextAttemptAt:  now,
		AcceptedAt:     now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	_, err = p.bun().NewInsert().
		Model(&row).
		On("CONFLICT (mailbox_id, idempotency_key) DO UPDATE").
		Set("updated_at = now()").
		Exec(ctx)
	if err != nil {
		return OutboundMessage{}, false, err
	}

	var stored outboundMessageRow
	err = p.bun().NewSelect().
		Model(&stored).
		Where("mailbox_id = ?", mailboxID).
		Where("idempotency_key = ?", key).
		Limit(1).
		Scan(ctx)
	if err != nil {
		return OutboundMessage{}, false, err
	}
	return outboundFromRow(stored), stored.ID == id, nil
}

func (p *Postgres) ClaimOutboundMessages(ctx context.Context, mailboxID string, limit int, lease time.Duration) ([]OutboundMessage, error) {
	if limit <= 0 {
		limit = 32
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	rows, err := p.DB.QueryContext(ctx, `WITH claim AS (
SELECT j.id
FROM outbound_messages j
JOIN mailboxes mb ON mb.id=j.mailbox_id
WHERE j.mailbox_id=$1
  AND mb.enabled=true
  AND j.next_attempt_at<=now()
  AND (j.status='pending' OR (j.status='processing' AND (j.lease_until IS NULL OR j.lease_until < now())))
ORDER BY j.next_attempt_at ASC, j.created_at ASC
FOR UPDATE SKIP LOCKED
LIMIT $2
)
UPDATE outbound_messages j
SET status='processing', lease_until=now()+($3::bigint * interval '1 second'), updated_at=now()
FROM claim
WHERE j.id=claim.id
RETURNING j.id::text,j.mailbox_id::text,j.alias_id::text,j.from_email,j.to_emails::text,j.raw_rfc822,j.idempotency_key,j.status,j.attempts,j.last_error,j.accepted_at,j.sent_at`,
		mailboxID, limit, int64(lease.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboundMessage{}
	for rows.Next() {
		var job OutboundMessage
		var toJSON string
		if err := rows.Scan(&job.ID, &job.MailboxID, &job.AliasID, &job.FromEmail, &toJSON, &job.RawRFC822, &job.IdempotencyKey, &job.Status, &job.Attempts, &job.LastError, &job.AcceptedAt, &job.SentAt); err != nil {
			return nil, err
		}
		job.To = parseOutboundRecipients([]byte(toJSON))
		out = append(out, job)
	}
	return out, rows.Err()
}

func (p *Postgres) MarkOutboundMessagesSent(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE outbound_messages
		SET status='sent', lease_until=NULL, last_error=NULL, sent_at=COALESCE(sent_at, now()), updated_at=now()
		WHERE id=ANY($1)`, pq.Array(ids)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts d
		SET status=$2, updated_at=now()
		FROM outbound_messages o
		WHERE o.id=ANY($1) AND o.status='sent'
		  AND d.mailbox_id=o.mailbox_id AND d.send_idempotency_key=o.idempotency_key
		  AND d.status=$3`, pq.Array(ids), string(model.DraftStatusSent), string(model.DraftStatusQueued)); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *Postgres) MarkOutboundMessagesRetry(ctx context.Context, ids []string, lastErr string, retryAfter time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	if retryAfter <= 0 {
		retryAfter = time.Minute
	}
	_, err := p.bun().NewUpdate().
		Model((*outboundMessageRow)(nil)).
		Set("status = 'pending'").
		Set("attempts = attempts + 1").
		Set("next_attempt_at = now() + (? * interval '1 second')", int64(retryAfter.Seconds())).
		Set("lease_until = NULL").
		Set("last_error = ?", toValidUTF8(lastErr)).
		Set("updated_at = now()").
		Where("id IN (?)", bun.In(ids)).
		Exec(ctx)
	return err
}

func (p *Postgres) MarkOutboundMessagesDead(ctx context.Context, ids []string, lastErr string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE outbound_messages
		SET status='dead', attempts=attempts+1, lease_until=NULL, last_error=$2, updated_at=now()
		WHERE id=ANY($1)`, pq.Array(ids), toValidUTF8(lastErr)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts d
		SET status=$2, updated_at=now()
		FROM outbound_messages o
		WHERE o.id=ANY($1) AND o.status='dead'
		  AND d.mailbox_id=o.mailbox_id AND d.send_idempotency_key=o.idempotency_key
		  AND d.status=$3`, pq.Array(ids), string(model.DraftStatusFailed), string(model.DraftStatusQueued)); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *Postgres) Search(ctx context.Context, claims model.TokenClaims, query, folder string, limit int) ([]model.ListItem, error) {
	req := model.SearchRequest{
		Folder: folder,
		Limit:  limit,
		Intent: model.SearchIntent{Text: query},
		Mode:   model.SearchModeKeyword,
	}
	return p.SearchStructured(ctx, claims, req)
}

func (p *Postgres) SearchStructured(ctx context.Context, claims model.TokenClaims, req model.SearchRequest) ([]model.ListItem, error) {
	if req.Limit <= 0 {
		req.Limit = 50
	}
	where, args, err := p.buildStructuredWhere(claims, req, true)
	if err != nil {
		return nil, err
	}
	qb := strings.Builder{}
	qb.WriteString(`SELECT m.id::text,f.name,m.internal_date,m.from_name,m.from_email,m.subject,
(ma2.message_id IS NOT NULL) AS seen,m.body_state,m.message_id,m.in_reply_to,m."references",
COALESCE(md.model_name,''),COALESCE(md.summary_short,''),COALESCE(md.summary_bullets,ARRAY[]::text[]),COALESCE(md.intent,''),COALESCE(md.priority,''),COALESCE(md.required_action,''),COALESCE(md.suggested_next_step,''),COALESCE(md.extracted_entities,'{}'::jsonb),COALESCE(md.confidence,0),
(SELECT count(*) FROM attachments a WHERE a.message_id=m.id)::int
FROM messages m
JOIN folders f ON f.id=m.folder_id
`)
	if claims.AliasID == nil {
		qb.WriteString(`LEFT JOIN message_audience aud ON FALSE`)
	} else {
		qb.WriteString(`JOIN message_audience aud ON aud.message_id=m.id`)
	}
	qb.WriteString(`
LEFT JOIN message_digest md ON md.message_id=m.id
LEFT JOIN message_bodies mb ON mb.message_id=m.id
LEFT JOIN message_ack ma2 ON ma2.message_id=m.id AND`)
	if claims.AliasID == nil {
		qb.WriteString(` FALSE`)
	} else {
		qb.WriteString(` ma2.alias_id = aud.alias_id`)
	}
	qb.WriteString(`
LEFT JOIN message_raw mr ON mr.message_id=m.id
WHERE ` + where + ` ORDER BY m.internal_date DESC, m.id DESC`)
	args = append(args, req.Limit)
	fmt.Fprintf(&qb, " LIMIT $%d", len(args))
	rows, err := p.DB.QueryContext(ctx, qb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanListItems(rows)
}

func (p *Postgres) SearchByEmbedding(ctx context.Context, claims model.TokenClaims, embedding []float32, folder string, limit int) ([]model.ListItem, error) {
	req := model.SearchRequest{
		Folder: folder,
		Limit:  limit,
		Mode:   model.SearchModeSemantic,
	}
	return p.SearchStructuredByEmbedding(ctx, claims, req, embedding)
}

func (p *Postgres) SearchStructuredByEmbedding(ctx context.Context, claims model.TokenClaims, req model.SearchRequest, embedding []float32) ([]model.ListItem, error) {
	if req.Limit <= 0 {
		req.Limit = 50
	}
	if len(embedding) == 0 {
		return []model.ListItem{}, nil
	}
	where, args, err := p.buildStructuredWhereWithBase(claims, req, false, []any{vectorLiteral(embedding)})
	if err != nil {
		return nil, err
	}
	qb := strings.Builder{}
	qb.WriteString(`WITH ranked AS (
SELECT me.message_id, MIN(me.embedding <=> $1::vector) AS dist
FROM message_embeddings me
GROUP BY me.message_id
)
SELECT m.id::text,f.name,m.internal_date,m.from_name,m.from_email,m.subject,(ma2.message_id IS NOT NULL) AS seen,m.body_state,m.message_id,m.in_reply_to,m."references",
COALESCE(md.model_name,''),COALESCE(md.summary_short,''),COALESCE(md.summary_bullets,ARRAY[]::text[]),COALESCE(md.intent,''),COALESCE(md.priority,''),COALESCE(md.required_action,''),COALESCE(md.suggested_next_step,''),COALESCE(md.extracted_entities,'{}'::jsonb),COALESCE(md.confidence,0),
(SELECT count(*) FROM attachments a WHERE a.message_id=m.id)::int
FROM ranked r
JOIN messages m ON m.id=r.message_id
JOIN folders f ON f.id=m.folder_id
`)
	if claims.AliasID == nil {
		qb.WriteString(`LEFT JOIN message_audience aud ON FALSE`)
	} else {
		qb.WriteString(`JOIN message_audience aud ON aud.message_id=m.id`)
	}
	qb.WriteString(`
LEFT JOIN message_digest md ON md.message_id=m.id
LEFT JOIN message_bodies mb ON mb.message_id=m.id
LEFT JOIN message_ack ma2 ON ma2.message_id=m.id AND`)
	if claims.AliasID == nil {
		qb.WriteString(` FALSE`)
	} else {
		qb.WriteString(` ma2.alias_id = aud.alias_id`)
	}
	qb.WriteString(`
LEFT JOIN message_raw mr ON mr.message_id=m.id
WHERE ` + where)
	args = append(args, req.Limit)
	fmt.Fprintf(&qb, " ORDER BY r.dist ASC, m.internal_date DESC, m.id DESC LIMIT $%d", len(args))
	rows, err := p.DB.QueryContext(ctx, qb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanListItems(rows)
}

func (p *Postgres) searchAdminMailbox(ctx context.Context, mailboxID, query string, folderNames []string, limit int) ([]model.ListItem, error) {
	var rows []listItemRow
	err := addSearchFiltersSelect(
		p.bun().NewSelect().
			TableExpr("messages AS m").
			ColumnExpr("m.id AS msg_id").
			ColumnExpr("f.name AS folder").
			ColumnExpr("m.internal_date AS received_at").
			ColumnExpr("m.from_name AS from_name").
			ColumnExpr("m.from_email AS from_email").
			ColumnExpr("m.subject AS subject").
			ColumnExpr("m.body_state AS body_state").
			ColumnExpr("m.message_id AS thread_message_id").
			ColumnExpr("m.in_reply_to AS thread_in_reply_to").
			ColumnExpr("m.\"references\" AS thread_references").
			ColumnExpr("COALESCE(md.model_name, '') AS model_name").
			ColumnExpr("COALESCE(md.summary_short, '') AS summary_short").
			ColumnExpr("COALESCE(md.summary_bullets, ARRAY[]::text[]) AS summary_bullets").
			ColumnExpr("COALESCE(md.intent, '') AS intent").
			ColumnExpr("COALESCE(md.priority, '') AS priority").
			ColumnExpr("COALESCE(md.required_action, '') AS required_action").
			ColumnExpr("COALESCE(md.suggested_next_step, '') AS suggested_next_step").
			ColumnExpr("COALESCE(md.extracted_entities, '{}'::jsonb) AS extracted_entities").
			ColumnExpr("COALESCE(md.confidence, 0) AS confidence").
			ColumnExpr("(SELECT count(*) FROM attachments AS a WHERE a.message_id = m.id)::int AS attachments_count").
			Join("JOIN folders AS f ON f.id = m.folder_id").
			Join("LEFT JOIN message_digest AS md ON md.message_id = m.id").
			Join("LEFT JOIN message_bodies AS mb ON mb.message_id = m.id").
			Where("m.mailbox_id = ?", mailboxID),
		folderNames,
		query,
	).
		OrderExpr("m.internal_date DESC, m.id DESC").
		Limit(limit).
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	return scanListItemRows(rows), nil
}

func (p *Postgres) searchByEmbeddingAdminMailbox(ctx context.Context, mailboxID string, embedding []float32, folderNames []string, limit int) ([]model.ListItem, error) {
	qb := strings.Builder{}
	qb.WriteString(`WITH ranked AS (
SELECT me.message_id, MIN(me.embedding <=> $2::vector) AS dist
FROM message_embeddings me
GROUP BY me.message_id
)
SELECT m.id::text,f.name,m.internal_date,m.from_name,m.from_email,m.subject,m.body_state,m.message_id,m.in_reply_to,m."references",
COALESCE(md.model_name,''),COALESCE(md.summary_short,''),COALESCE(md.summary_bullets,ARRAY[]::text[]),COALESCE(md.intent,''),COALESCE(md.priority,''),COALESCE(md.required_action,''),COALESCE(md.suggested_next_step,''),COALESCE(md.extracted_entities,'{}'::jsonb),COALESCE(md.confidence,0),
(SELECT count(*) FROM attachments a WHERE a.message_id=m.id)::int
FROM ranked r
JOIN messages m ON m.id=r.message_id
JOIN folders f ON f.id=m.folder_id
LEFT JOIN message_digest md ON md.message_id=m.id
WHERE m.mailbox_id=$1`)
	args := []any{mailboxID, vectorLiteral(embedding)}
	if len(folderNames) > 0 {
		args = append(args, pq.Array(folderNames))
		fmt.Fprintf(&qb, " AND lower(f.name) = ANY($%d)", len(args))
	}
	args = append(args, limit)
	fmt.Fprintf(&qb, " ORDER BY r.dist ASC, m.internal_date DESC, m.id DESC LIMIT $%d", len(args))
	rows, err := p.DB.QueryContext(ctx, qb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanListItems(rows)
}

func scanListItems(rows *sql.Rows) ([]model.ListItem, error) {
	var out []model.ListItem
	for rows.Next() {
		var item model.ListItem
		var folderName string
		var fromName sql.NullString
		var subj sql.NullString
		var seen bool
		var inReply sql.NullString
		var refs sql.NullString
		var bullets []string
		var entities []byte
		if err := rows.Scan(&item.MsgID, &folderName, &item.ReceivedAt, &fromName, &item.From.Email, &subj, &seen, &item.BodyState, &item.Thread.MessageID, &inReply, &refs,
			&item.Digest.ModelName, &item.Digest.SummaryShort, pq.Array(&bullets), &item.Digest.Intent, &item.Digest.Priority, &item.Digest.RequiredAction, &item.Digest.SuggestedNextStep, &entities, &item.Digest.Confidence,
			&item.AttachmentsCount); err != nil {
			return nil, err
		}
		item.Folder = folderName
		item.Seen = seen
		if fromName.Valid {
			item.From.Name = &fromName.String
		}
		if subj.Valid {
			item.Subject = &subj.String
		}
		if inReply.Valid {
			item.Thread.InReplyTo = &inReply.String
		}
		if refs.Valid {
			item.Thread.References = &refs.String
		}
		item.Digest.SummaryBullets = bullets
		item.Digest.ExtractedEntities = entities
		out = append(out, item)
	}
	return out, rows.Err()
}

func vectorLiteral(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.Grow(len(v) * 12)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func (p *Postgres) RecordAudit(ctx context.Context, tokenID, action string, messageID *string, target *string) error {
	row := auditLogRow{
		ID:        uuid.NewString(),
		TokenID:   tokenID,
		Action:    action,
		MessageID: messageID,
		Target:    target,
		CreatedAt: time.Now().UTC(),
	}
	_, err := p.bun().NewInsert().Model(&row).Exec(ctx)
	return err
}

func (p *Postgres) GetAliasByEmail(ctx context.Context, mailboxID, email string) (string, error) {
	var row aliasRow
	err := p.bun().NewSelect().Model(&row).Column("id").Where("mailbox_id = ?", mailboxID).Where("email = ?", strings.ToLower(strings.TrimSpace(email))).Where("enabled = TRUE").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return row.ID, err
}

func (p *Postgres) GetAliasEmailByID(ctx context.Context, aliasID string) (string, error) {
	var row aliasRow
	err := p.bun().NewSelect().Model(&row).Column("email").Where("id = ?", aliasID).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return row.Email, err
}

func (p *Postgres) GetMailboxIDByAliasID(ctx context.Context, aliasID string) (string, error) {
	var row aliasRow
	err := p.bun().NewSelect().Model(&row).Column("mailbox_id").Where("id = ?", aliasID).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return row.MailboxID, err
}

func (p *Postgres) GetThreadContextForMessage(ctx context.Context, claims model.TokenClaims, mailboxID, messageID string) ([]model.ThreadContextMessage, error) {
	if claims.MailboxID != nil && *claims.MailboxID != mailboxID {
		return nil, ErrUnauthorized
	}

	accessMode := "shared"
	var aliasID any
	switch {
	case claims.AliasID != nil:
		if err := p.mustAliasVisible(ctx, model.TokenClaims{AliasID: claims.AliasID}, messageID); err != nil {
			return nil, err
		}
		accessMode = "alias"
		aliasID = *claims.AliasID
	case claims.IsAdmin():
		accessMode = "admin"
	case claims.TokenID != "" || claims.MailboxID == nil || *claims.MailboxID != mailboxID:
		return nil, ErrUnauthorized
	}

	rows, err := p.DB.QueryContext(ctx, `WITH RECURSIVE
target AS (
	SELECT m.id, m.message_id, m.in_reply_to, m."references"
	FROM messages m
	WHERE m.mailbox_id=$1 AND m.id=$2::uuid
	  AND (
		$3 = 'admin'
		OR ($3 = 'alias' AND EXISTS (
			SELECT 1 FROM message_audience target_aud
			WHERE target_aud.message_id=m.id AND target_aud.alias_id=$4
		))
		OR ($3 = 'shared' AND EXISTS (
			SELECT 1 FROM message_audience target_aud
			WHERE target_aud.message_id=m.id
		))
	  )
),
seed(mid) AS (
	SELECT NULLIF(message_id, '') FROM target WHERE NULLIF(message_id, '') IS NOT NULL
	UNION
	SELECT NULLIF(in_reply_to, '') FROM target WHERE NULLIF(in_reply_to, '') IS NOT NULL
	UNION
	SELECT DISTINCT NULLIF(trim(r), '')
	FROM target t, regexp_split_to_table(COALESCE(t."references", ''), E'\\s+') AS r
	WHERE NULLIF(trim(r), '') IS NOT NULL
),
thread_ids(mid) AS (
	SELECT mid FROM seed
	UNION
	SELECT DISTINCT link.mid
	FROM messages m
	JOIN thread_ids ti
	  ON m.in_reply_to = ti.mid
	  OR m.message_id = ti.mid
	CROSS JOIN LATERAL (
		VALUES (NULLIF(m.message_id, '')),
		       (NULLIF(m.in_reply_to, ''))
	) AS link(mid)
	WHERE m.mailbox_id=$1
	  AND link.mid IS NOT NULL
)
SELECT
	m.id::text,
	COALESCE(m.message_id, ''),
	m.internal_date,
	COALESCE(m.from_email, ''),
	m.from_name,
	m.subject,
	mb.extracted_text,
	md.summary_short
FROM messages m
LEFT JOIN message_bodies mb ON mb.message_id=m.id
LEFT JOIN message_digest md ON md.message_id=m.id
WHERE m.mailbox_id=$1
  AND (
	m.id=$2::uuid
	OR (NULLIF(m.message_id, '') IS NOT NULL AND m.message_id IN (SELECT mid FROM thread_ids))
  )
  AND (
	$3 = 'admin'
	OR ($3 = 'alias' AND EXISTS (
		SELECT 1 FROM message_audience candidate_aud
		WHERE candidate_aud.message_id=m.id AND candidate_aud.alias_id=$4
	))
	OR ($3 = 'shared' AND NOT EXISTS (
		SELECT 1
		FROM message_audience source_aud
		WHERE source_aud.message_id=$2::uuid
		  AND NOT EXISTS (
			SELECT 1 FROM message_audience candidate_aud
			WHERE candidate_aud.message_id=m.id
			  AND candidate_aud.alias_id=source_aud.alias_id
		  )
	))
  )
ORDER BY m.internal_date ASC, m.created_at ASC`, mailboxID, messageID, accessMode, aliasID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.ThreadContextMessage{}
	for rows.Next() {
		var item model.ThreadContextMessage
		var fromName sql.NullString
		var subject sql.NullString
		var extracted sql.NullString
		var digest sql.NullString
		if err := rows.Scan(&item.MessageID, &item.RFCMessageID, &item.InternalDate, &item.FromEmail, &fromName, &subject, &extracted, &digest); err != nil {
			return nil, err
		}
		if fromName.Valid {
			item.FromName = &fromName.String
		}
		if subject.Valid {
			item.Subject = &subject.String
		}
		if extracted.Valid {
			item.ExtractedText = &extracted.String
		}
		if digest.Valid {
			item.DigestSummary = &digest.String
		}
		item.IsCurrent = item.MessageID == messageID
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

func (p *Postgres) GetMessageThreadMeta(ctx context.Context, claims model.TokenClaims, messageID string) (*string, *string, *string, error) {
	if err := p.mustAliasVisible(ctx, claims, messageID); err != nil {
		return nil, nil, nil, err
	}
	type threadMetaRow struct {
		RFCMessageID *string `bun:"rfc_message_id"`
		InReplyTo    *string `bun:"in_reply_to"`
		References   *string `bun:"references"`
		Subject      *string `bun:"subject"`
	}
	var row threadMetaRow
	err := p.bun().NewSelect().
		TableExpr("messages").
		ColumnExpr("message_id AS rfc_message_id").
		ColumnExpr("in_reply_to AS in_reply_to").
		ColumnExpr("\"references\" AS references").
		ColumnExpr("subject AS subject").
		Where("id = ?", messageID).
		Limit(1).
		Scan(ctx, &row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil, ErrNotFound
		}
		return nil, nil, nil, err
	}
	inReplyTo, references := buildReplyThreadMeta(row.RFCMessageID, row.InReplyTo, row.References)
	return inReplyTo, references, row.Subject, nil
}

func buildReplyThreadMeta(rfcMessageID, parentInReplyTo, parentReferences *string) (*string, *string) {
	messageID := ""
	if rfcMessageID != nil {
		messageID = strings.TrimSpace(*rfcMessageID)
	}

	referenceFields := []string{}
	if parentReferences != nil {
		referenceFields = strings.Fields(*parentReferences)
	}
	if len(referenceFields) == 0 && parentInReplyTo != nil {
		referenceFields = strings.Fields(*parentInReplyTo)
	}

	seen := make(map[string]struct{}, len(referenceFields)+1)
	references := make([]string, 0, len(referenceFields)+1)
	for _, reference := range referenceFields {
		if _, ok := seen[reference]; ok {
			continue
		}
		seen[reference] = struct{}{}
		references = append(references, reference)
	}
	if messageID != "" {
		if _, ok := seen[messageID]; !ok {
			references = append(references, messageID)
		}
	}

	var outgoingInReplyTo *string
	if messageID != "" {
		outgoingInReplyTo = &messageID
	}
	if len(references) == 0 {
		return outgoingInReplyTo, nil
	}
	referenceChain := strings.Join(references, " ")
	return outgoingInReplyTo, &referenceChain
}

func (p *Postgres) GetMessageReplyTo(ctx context.Context, claims model.TokenClaims, messageID string) (string, error) {
	if err := p.mustAliasVisible(ctx, claims, messageID); err != nil {
		return "", err
	}
	type replyToRow struct {
		FromEmail string `bun:"from_email"`
	}
	var row replyToRow
	err := p.bun().NewSelect().
		TableExpr("messages").
		ColumnExpr("from_email AS from_email").
		Where("id = ?", messageID).
		Limit(1).
		Scan(ctx, &row)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return row.FromEmail, err
}

func (p *Postgres) Status(ctx context.Context, mailboxID string) (Status, error) {
	var s Status
	if err := p.bun().NewSelect().TableExpr("messages").ColumnExpr("count(*)").Where("mailbox_id = ?", mailboxID).Scan(ctx, &s.MessageCount); err != nil {
		return s, err
	}
	if err := p.bun().NewSelect().TableExpr("message_digest AS md").Join("JOIN messages AS m ON m.id = md.message_id").ColumnExpr("count(*)").Where("m.mailbox_id = ?", mailboxID).Scan(ctx, &s.DigestCount); err != nil {
		return s, err
	}
	if err := p.bun().NewSelect().TableExpr("aliases").ColumnExpr("count(*)").Where("mailbox_id = ?", mailboxID).Scan(ctx, &s.AliasCount); err != nil {
		return s, err
	}
	if err := p.bun().NewSelect().TableExpr("folders").ColumnExpr("COALESCE(max(last_seen_uid),0)").Where("mailbox_id = ?", mailboxID).Scan(ctx, &s.LastSeenUID); err != nil {
		return s, err
	}
	return s, nil
}

func DecodeEntities(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func ptr(s string) *string { return &s }

var _ Store = (*Postgres)(nil)

func Now() time.Time { return time.Now().UTC() }
