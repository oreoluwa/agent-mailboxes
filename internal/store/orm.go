package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

type mailboxRow struct {
	bun.BaseModel `bun:"table:mailboxes"`

	ID                string    `bun:"id,pk"`
	Name              string    `bun:"name"`
	Enabled           bool      `bun:"enabled"`
	IMAPHost          string    `bun:"imap_host"`
	IMAPPort          int       `bun:"imap_port"`
	IMAPTLS           bool      `bun:"imap_tls"`
	SMTPHost          string    `bun:"smtp_host"`
	SMTPPort          int       `bun:"smtp_port"`
	SMTPTLS           bool      `bun:"smtp_tls"`
	Username          string    `bun:"username"`
	PasswordEncrypted string    `bun:"password_encrypted"`
	CreatedAt         time.Time `bun:"created_at"`
}

type folderRow struct {
	bun.BaseModel `bun:"table:folders"`

	ID          string    `bun:"id,pk"`
	MailboxID   string    `bun:"mailbox_id"`
	Name        string    `bun:"name"`
	UIDValidity int64     `bun:"uidvalidity"`
	LastSeenUID int64     `bun:"last_seen_uid"`
	UpdatedAt   time.Time `bun:"updated_at"`
}

type aliasRow struct {
	bun.BaseModel `bun:"table:aliases"`

	ID        string    `bun:"id,pk"`
	MailboxID string    `bun:"mailbox_id"`
	Email     string    `bun:"email"`
	Enabled   bool      `bun:"enabled"`
	CreatedAt time.Time `bun:"created_at"`
}

type tokenRow struct {
	bun.BaseModel `bun:"table:tokens"`

	ID        string     `bun:"id,pk"`
	AliasID   *string    `bun:"alias_id"`
	Name      string     `bun:"name"`
	TokenHash string     `bun:"token_hash"`
	Scopes    []string   `bun:"scopes,array"`
	CreatedAt time.Time  `bun:"created_at"`
	RevokedAt *time.Time `bun:"revoked_at"`
}

type attachmentRow struct {
	bun.BaseModel `bun:"table:attachments"`

	ID          string    `bun:"id,pk"`
	MessageID   string    `bun:"message_id"`
	Ordinal     int       `bun:"ordinal"`
	Filename    *string   `bun:"filename"`
	MIME        *string   `bun:"mime"`
	SizeBytes   int64     `bun:"size_bytes"`
	StoragePath *string   `bun:"storage_path"`
	CreatedAt   time.Time `bun:"created_at"`
}

type messageBodyRow struct {
	bun.BaseModel `bun:"table:message_bodies"`

	MessageID     string  `bun:"message_id,pk"`
	TextPlain     *string `bun:"text_plain"`
	TextHTML      *string `bun:"text_html"`
	ExtractedText *string `bun:"extracted_text"`
}

type messageRawRow struct {
	bun.BaseModel `bun:"table:message_raw"`

	MessageID string    `bun:"message_id,pk"`
	RawRFC822 []byte    `bun:"raw_rfc822"`
	CreatedAt time.Time `bun:"created_at"`
}

type messageAckRow struct {
	bun.BaseModel `bun:"table:message_ack"`

	MessageID string    `bun:"message_id"`
	AliasID   string    `bun:"alias_id"`
	CreatedAt time.Time `bun:"created_at"`
}

type digestRow struct {
	bun.BaseModel `bun:"table:message_digest"`

	MessageID         string    `bun:"message_id,pk"`
	ModelName         string    `bun:"model_name"`
	SummaryShort      string    `bun:"summary_short"`
	SummaryBullets    []string  `bun:"summary_bullets,array"`
	Intent            string    `bun:"intent"`
	Priority          string    `bun:"priority"`
	RequiredAction    string    `bun:"required_action"`
	SuggestedNextStep string    `bun:"suggested_next_step"`
	ExtractedEntities []byte    `bun:"extracted_entities,type:jsonb"`
	Confidence        float64   `bun:"confidence"`
	CreatedAt         time.Time `bun:"created_at"`
}

type auditLogRow struct {
	bun.BaseModel `bun:"table:audit_log"`

	ID        string    `bun:"id,pk"`
	TokenID   string    `bun:"token_id"`
	Action    string    `bun:"action"`
	MessageID *string   `bun:"message_id"`
	Target    *string   `bun:"target"`
	CreatedAt time.Time `bun:"created_at"`
}

type embeddingJobRow struct {
	bun.BaseModel `bun:"table:embedding_jobs"`

	ID              string     `bun:"id,pk"`
	MessageID       string     `bun:"message_id"`
	Segment         string     `bun:"segment"`
	ChunkIndex      int        `bun:"chunk_index"`
	ModelName       string     `bun:"model_name"`
	SourceText      string     `bun:"source_text"`
	Status          string     `bun:"status"`
	ProviderBatchID *string    `bun:"provider_batch_id"`
	Attempts        int        `bun:"attempts"`
	NextAttemptAt   time.Time  `bun:"next_attempt_at"`
	LeaseUntil      *time.Time `bun:"lease_until"`
	LastError       *string    `bun:"last_error"`
	CreatedAt       time.Time  `bun:"created_at"`
	UpdatedAt       time.Time  `bun:"updated_at"`
}

type summarizeJobRow struct {
	bun.BaseModel `bun:"table:summarize_jobs"`

	ID            string     `bun:"id,pk"`
	MessageID     string     `bun:"message_id"`
	Payload       []byte     `bun:"payload,type:jsonb"`
	Status        string     `bun:"status"`
	Attempts      int        `bun:"attempts"`
	NextAttemptAt time.Time  `bun:"next_attempt_at"`
	LeaseUntil    *time.Time `bun:"lease_until"`
	LastError     *string    `bun:"last_error"`
	CreatedAt     time.Time  `bun:"created_at"`
	UpdatedAt     time.Time  `bun:"updated_at"`
}

type outboundMessageRow struct {
	bun.BaseModel `bun:"table:outbound_messages"`

	ID             string          `bun:"id,pk"`
	MailboxID      string          `bun:"mailbox_id"`
	AliasID        string          `bun:"alias_id"`
	FromEmail      string          `bun:"from_email"`
	ToEmails       json.RawMessage `bun:"to_emails,type:jsonb"`
	RawRFC822      []byte          `bun:"raw_rfc822"`
	IdempotencyKey string          `bun:"idempotency_key"`
	Status         string          `bun:"status"`
	Attempts       int             `bun:"attempts"`
	NextAttemptAt  time.Time       `bun:"next_attempt_at"`
	LeaseUntil     *time.Time      `bun:"lease_until"`
	LastError      *string         `bun:"last_error"`
	AcceptedAt     time.Time       `bun:"accepted_at"`
	SentAt         *time.Time      `bun:"sent_at"`
	CreatedAt      time.Time       `bun:"created_at"`
	UpdatedAt      time.Time       `bun:"updated_at"`
}

type listItemRow struct {
	MsgID             string    `bun:"msg_id"`
	Folder            string    `bun:"folder"`
	ReceivedAt        time.Time `bun:"received_at"`
	FromName          *string   `bun:"from_name"`
	FromEmail         string    `bun:"from_email"`
	Subject           *string   `bun:"subject"`
	Seen              bool      `bun:"seen"`
	BodyState         string    `bun:"body_state"`
	ThreadMessageID   string    `bun:"thread_message_id"`
	ThreadInReplyTo   *string   `bun:"thread_in_reply_to"`
	ThreadReferences  *string   `bun:"thread_references"`
	ModelName         string    `bun:"model_name"`
	SummaryShort      string    `bun:"summary_short"`
	SummaryBullets    []string  `bun:"summary_bullets,array"`
	Intent            string    `bun:"intent"`
	Priority          string    `bun:"priority"`
	RequiredAction    string    `bun:"required_action"`
	SuggestedNextStep string    `bun:"suggested_next_step"`
	ExtractedEntities []byte    `bun:"extracted_entities"`
	Confidence        float64   `bun:"confidence"`
	AttachmentsCount  int       `bun:"attachments_count"`
}

func newBunDB(db *sql.DB) *bun.DB {
	return bun.NewDB(db, pgdialect.New())
}

func (p *Postgres) mailboxFromRow(row mailboxRow) (Mailbox, error) {
	password, _, err := p.decryptMailboxCredential(row.ID, row.PasswordEncrypted)
	if err != nil {
		return Mailbox{}, err
	}
	return Mailbox{
		ID:      row.ID,
		Name:    row.Name,
		Enabled: row.Enabled,
		Creds: MailboxCreds{
			IMAPHost: row.IMAPHost,
			IMAPPort: row.IMAPPort,
			IMAPTLS:  row.IMAPTLS,
			SMTPHost: row.SMTPHost,
			SMTPPort: row.SMTPPort,
			SMTPTLS:  row.SMTPTLS,
			Username: row.Username,
			Password: password,
		},
		CreatedAt: row.CreatedAt,
	}, nil
}

func listItemFromRow(row listItemRow) model.ListItem {
	item := model.ListItem{
		MsgID:      row.MsgID,
		Folder:     row.Folder,
		ReceivedAt: row.ReceivedAt,
		From: model.From{
			Name:  row.FromName,
			Email: row.FromEmail,
		},
		Subject:   row.Subject,
		Seen:      row.Seen,
		BodyState: row.BodyState,
		Thread: model.Thread{
			MessageID:  row.ThreadMessageID,
			InReplyTo:  row.ThreadInReplyTo,
			References: row.ThreadReferences,
		},
		Digest: model.Digest{
			ModelName:         row.ModelName,
			SummaryShort:      row.SummaryShort,
			SummaryBullets:    row.SummaryBullets,
			Intent:            row.Intent,
			Priority:          row.Priority,
			RequiredAction:    row.RequiredAction,
			SuggestedNextStep: row.SuggestedNextStep,
			ExtractedEntities: row.ExtractedEntities,
			Confidence:        row.Confidence,
		},
		AttachmentsCount: row.AttachmentsCount,
	}
	return item
}

func embeddingJobFromRow(row embeddingJobRow) EmbeddingJob {
	return EmbeddingJob{
		ID:        row.ID,
		MessageID: row.MessageID,
		Segment: EmbeddingSegment{
			Segment:    row.Segment,
			ChunkIndex: row.ChunkIndex,
			Text:       row.SourceText,
		},
	}
}

func summarizeJobFromRow(row summarizeJobRow) SummarizeJob {
	return SummarizeJob{
		ID:        row.ID,
		MessageID: row.MessageID,
		Payload:   append([]byte(nil), row.Payload...),
	}
}
