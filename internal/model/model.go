package model

import (
	"encoding/json"
	"time"
)

type Scope string

const (
	ScopeRead  Scope = "read"
	ScopeSend  Scope = "send"
	ScopeAdmin Scope = "admin"
)

type TokenClaims struct {
	TokenID   string
	AliasID   *string
	MailboxID *string
	Scopes    map[Scope]bool
}

func (c TokenClaims) Has(scope Scope) bool { return c.Scopes[scope] }

func (c TokenClaims) IsAdmin() bool { return c.Has(ScopeAdmin) }

type Recipient struct {
	Type  string `json:"type"`
	Email string `json:"email"`
}

type Attachment struct {
	ID        string    `json:"id,omitempty"`
	MessageID string    `json:"message_id,omitempty"`
	Filename  string    `json:"filename"`
	MIME      string    `json:"mime"`
	SizeBytes int64     `json:"size_bytes"`
	Storage   *string   `json:"storage_path,omitempty"`
	Content   []byte    `json:"-"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type Digest struct {
	MessageID         string          `json:"message_id,omitempty"`
	ModelName         string          `json:"model_name"`
	SummaryShort      string          `json:"summary_short"`
	SummaryBullets    []string        `json:"summary_bullets"`
	Intent            string          `json:"intent"`
	Priority          string          `json:"priority"`
	RequiredAction    string          `json:"required_action"`
	SuggestedNextStep string          `json:"suggested_next_step"`
	ExtractedEntities json.RawMessage `json:"extracted_entities"`
	Confidence        float64         `json:"confidence"`
	CreatedAt         time.Time       `json:"created_at,omitempty"`
}

type Message struct {
	ID             string       `json:"msg_id"`
	MailboxID      string       `json:"-"`
	FolderID       string       `json:"-"`
	ImapUID        int64        `json:"-"`
	RFCMessageID   string       `json:"rfc_message_id,omitempty"`
	InternalDate   time.Time    `json:"received_at"`
	SentDate       *time.Time   `json:"sent_date,omitempty"`
	FromEmail      string       `json:"from_email"`
	FromName       *string      `json:"from_name,omitempty"`
	Subject        *string      `json:"subject,omitempty"`
	Snippet        *string      `json:"snippet,omitempty"`
	RawHeaders     string       `json:"-"`
	HasAttachments bool         `json:"-"`
	SpamFlag       bool         `json:"-"`
	BodyState      string       `json:"body_state"`
	InReplyTo      *string      `json:"in_reply_to,omitempty"`
	References     *string      `json:"references,omitempty"`
	Digest         *Digest      `json:"digest,omitempty"`
	Attachments    []Attachment `json:"attachments,omitempty"`
	Thread         Thread       `json:"thread"`
	From           From         `json:"from"`
}

type From struct {
	Name  *string `json:"name,omitempty"`
	Email string  `json:"email"`
}

type Thread struct {
	MessageID  string  `json:"message_id"`
	InReplyTo  *string `json:"in_reply_to,omitempty"`
	References *string `json:"references,omitempty"`
}

type FolderSummary struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Total       int    `json:"total"`
	Unread      int    `json:"unread"`
}

type ListItem struct {
	MsgID            string    `json:"msg_id"`
	Folder           string    `json:"folder"`
	ReceivedAt       time.Time `json:"received_at"`
	From             From      `json:"from"`
	Subject          *string   `json:"subject,omitempty"`
	Seen             bool      `json:"seen"`
	Digest           Digest    `json:"digest"`
	AttachmentsCount int       `json:"attachments_count"`
	BodyState        string    `json:"body_state"`
	Thread           Thread    `json:"thread"`
}

type SearchMode string

const (
	SearchModeHybrid   SearchMode = "hybrid"
	SearchModeKeyword  SearchMode = "keyword"
	SearchModeSemantic SearchMode = "semantic"
)

type SearchRequest struct {
	Folder     string        `json:"folder,omitempty"`
	Limit      int           `json:"limit,omitempty"`
	Filters    SearchFilters `json:"filters,omitempty"`
	Intent     SearchIntent  `json:"intent,omitempty"`
	Mode       SearchMode    `json:"mode,omitempty"`
	ParserMode string        `json:"parser_mode,omitempty"`
}

type SearchFilters struct {
	Headers SearchHeaderFilters `json:"headers,omitempty"`
	Flags   SearchFlagFilters   `json:"flags,omitempty"`
	Date    SearchDateFilters   `json:"date,omitempty"`
	Size    SearchSizeFilters   `json:"size,omitempty"`
	Logic   SearchLogic         `json:"logic,omitempty"`
}

type SearchHeaderFilters struct {
	Subject   []string            `json:"subject,omitempty"`
	From      []string            `json:"from,omitempty"`
	To        []string            `json:"to,omitempty"`
	MessageID []string            `json:"message_id,omitempty"`
	Generic   map[string][]string `json:"generic,omitempty"`
}

type SearchFlagFilters struct {
	With    []string `json:"with,omitempty"`
	Without []string `json:"without,omitempty"`
}

type SearchDateFilters struct {
	InternalSince  *time.Time            `json:"internal_since,omitempty"`
	InternalBefore *time.Time            `json:"internal_before,omitempty"`
	SentSince      *time.Time            `json:"sent_since,omitempty"`
	SentBefore     *time.Time            `json:"sent_before,omitempty"`
	Field          string                `json:"field,omitempty"`
	Range          *SearchDateRange      `json:"range,omitempty"`
	Temporal       *SearchTemporalFilter `json:"temporal,omitempty"`
}

type SearchDateRange struct {
	Start        *SearchTemporalPoint `json:"start,omitempty"`
	End          *SearchTemporalPoint `json:"end,omitempty"`
	EndInclusive bool                 `json:"end_inclusive,omitempty"`
}

type SearchTemporalPoint struct {
	Type      string `json:"type,omitempty"`
	Value     string `json:"value,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Date      string `json:"date,omitempty"`
	Direction string `json:"direction,omitempty"`
	Amount    int    `json:"amount,omitempty"`
	Unit      string `json:"unit,omitempty"`
	Weekday   string `json:"weekday,omitempty"`
	Period    string `json:"period,omitempty"`
	Boundary  string `json:"boundary,omitempty"`
	Snap      string `json:"snap,omitempty"`
	Timezone  string `json:"timezone,omitempty"`
}

type SearchTemporalFilter struct {
	Kind     string `json:"kind,omitempty"`
	Value    string `json:"value,omitempty"`
	Amount   int    `json:"amount,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Field    string `json:"field,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Date     string `json:"date,omitempty"`
}

type SearchSizeFilters struct {
	Larger  *int64 `json:"larger,omitempty"`
	Smaller *int64 `json:"smaller,omitempty"`
}

type SearchLogic struct {
	Not []SearchSubquery `json:"not,omitempty"`
	Or  []SearchOrClause `json:"or,omitempty"`
}

type SearchSubquery struct {
	Filters SearchFilters `json:"filters,omitempty"`
	Intent  SearchIntent  `json:"intent,omitempty"`
}

type SearchOrClause struct {
	Any []SearchSubquery `json:"any,omitempty"`
}

type SearchIntent struct {
	Text        string             `json:"text,omitempty"`
	BodyTerms   []string           `json:"body_terms,omitempty"`
	HeaderTerms SearchHeaderIntent `json:"header_terms,omitempty"`
}

type SearchHeaderIntent struct {
	Subject []string `json:"subject,omitempty"`
	From    []string `json:"from,omitempty"`
	To      []string `json:"to,omitempty"`
}

type Body struct {
	MessageID     string  `json:"message_id"`
	TextPlain     *string `json:"text_plain,omitempty"`
	TextHTML      *string `json:"text_html,omitempty"`
	ExtractedText *string `json:"extracted_text,omitempty"`
}

type ParsedEmail struct {
	IMAPUID         int64
	IMAPUIDValidity int64
	RawRFC822       []byte
	RFCMessageID    string
	InternalDate    time.Time
	SentDate        *time.Time
	FromEmail       string
	FromName        *string
	Subject         *string
	RawHeaders      string
	InReplyTo       *string
	References      *string
	Recipients      []Recipient
	TextPlain       *string
	TextHTML        *string
	ExtractedText   string
	Snippet         *string
	Attachments     []Attachment
	HasAttachment   bool
	SpamFlag        bool
}

type ThreadContextMessage struct {
	MessageID     string
	RFCMessageID  string
	InternalDate  time.Time
	FromEmail     string
	FromName      *string
	Subject       *string
	ExtractedText *string
	DigestSummary *string
	IsCurrent     bool
}

func (r SearchRequest) NormalizedMode() SearchMode {
	switch r.Mode {
	case SearchModeKeyword, SearchModeSemantic:
		return r.Mode
	default:
		return SearchModeHybrid
	}
}
