package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/uptrace/bun"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func (p *Postgres) mustMailboxVisible(ctx context.Context, claims model.TokenClaims, mailboxID string) error {
	mailboxID = strings.TrimSpace(mailboxID)
	if mailboxID == "" {
		return ErrUnauthorized
	}
	if claims.AliasID == nil {
		if claims.IsAdmin() && (claims.MailboxID == nil || *claims.MailboxID == "" || *claims.MailboxID == mailboxID) {
			return nil
		}
		return ErrUnauthorized
	}
	var ok bool
	err := p.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM aliases WHERE id=$1 AND mailbox_id=$2 AND enabled=true)`, *claims.AliasID, mailboxID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrUnauthorized
	}
	return nil
}

func scopedAliasID(claims model.TokenClaims) (*string, error) {
	if claims.AliasID != nil && strings.TrimSpace(*claims.AliasID) != "" {
		aliasID := strings.TrimSpace(*claims.AliasID)
		return &aliasID, nil
	}
	if claims.IsAdmin() {
		return nil, nil
	}
	return nil, ErrUnauthorized
}

func ownedByClaims(claims model.TokenClaims, aliasID *string) bool {
	if claims.AliasID != nil && strings.TrimSpace(*claims.AliasID) != "" {
		return aliasID != nil && *aliasID == strings.TrimSpace(*claims.AliasID)
	}
	return claims.IsAdmin()
}

func (p *Postgres) mustSourceMessageVisible(ctx context.Context, claims model.TokenClaims, mailboxID, messageID string) error {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ErrNotFound
	}
	var ok bool
	if claims.AliasID != nil && strings.TrimSpace(*claims.AliasID) != "" {
		err := p.DB.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM messages m
			JOIN message_audience ma ON ma.message_id=m.id
			WHERE m.id=$1 AND m.mailbox_id=$2 AND ma.alias_id=$3
		)`, messageID, mailboxID, strings.TrimSpace(*claims.AliasID)).Scan(&ok)
		if err != nil {
			return err
		}
	} else if claims.IsAdmin() {
		err := p.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=$1 AND mailbox_id=$2)`, messageID, mailboxID).Scan(&ok)
		if err != nil {
			return err
		}
	} else {
		return ErrUnauthorized
	}
	if !ok {
		return ErrUnauthorized
	}
	return nil
}

func actorUserID(actor *model.VerifiedActor) *string {
	if actor == nil || strings.TrimSpace(actor.UserID) == "" {
		return nil
	}
	v := strings.TrimSpace(actor.UserID)
	return &v
}

func actorRequestID(actor *model.VerifiedActor) *string {
	if actor == nil || strings.TrimSpace(actor.RequestID) == "" {
		return nil
	}
	v := strings.TrimSpace(actor.RequestID)
	return &v
}

func actorPermission(actor *model.VerifiedActor) *string {
	if actor == nil || strings.TrimSpace(actor.PermissionIntent) == "" {
		return nil
	}
	v := strings.TrimSpace(actor.PermissionIntent)
	return &v
}

func (p *Postgres) loadDraft(ctx context.Context, draftID string) (draftRow, []draftRevisionRow, error) {
	var row draftRow
	err := p.bun().NewSelect().Model(&row).Where("id = ?", draftID).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return draftRow{}, nil, ErrNotFound
	}
	if err != nil {
		return draftRow{}, nil, err
	}
	var revRows []draftRevisionRow
	if err := p.bun().NewSelect().Model(&revRows).Where("draft_id = ?", draftID).Order("revision_number ASC").Scan(ctx); err != nil {
		return draftRow{}, nil, err
	}
	return row, revRows, nil
}

func draftFromRows(row draftRow, revRows []draftRevisionRow) (model.Draft, error) {
	if len(revRows) == 0 {
		return model.Draft{}, fmt.Errorf("draft %s has no revisions", row.ID)
	}
	revs := make([]model.DraftRevision, 0, len(revRows))
	for _, rr := range revRows {
		revs = append(revs, draftRevisionFromRow(rr))
	}
	latest := revs[len(revs)-1]
	latestID := latest.ID
	if row.LatestRevisionID != nil && *row.LatestRevisionID != "" {
		latestID = *row.LatestRevisionID
		for _, rev := range revs {
			if rev.ID == latestID {
				latest = rev
				break
			}
		}
	}
	return model.Draft{
		ID:                 row.ID,
		MailboxID:          row.MailboxID,
		AliasID:            row.AliasID,
		ConversationID:     row.ConversationID,
		SourceMessageID:    row.SourceMessageID,
		ThreadRFCMessageID: row.ThreadRFCMessageID,
		Status:             model.DraftStatus(row.Status),
		LatestRevisionID:   latestID,
		LatestRevision:     latest,
		Revisions:          revs,
		ActorUserID:        row.ActorUserID,
		LastRequestID:      row.LastRequestID,
		PermissionIntent:   row.PermissionIntent,
		SendIdempotencyKey: row.SendIdempotencyKey,
		SentRevisionID:     row.SentRevisionID,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}, nil
}

const (
	defaultDraftListLimit = 100
	maxDraftListLimit     = 200
)

func normalizeDraftListLimit(limit int) int {
	if limit <= 0 {
		return defaultDraftListLimit
	}
	if limit > maxDraftListLimit {
		return maxDraftListLimit
	}
	return limit
}

func (p *Postgres) ListDrafts(ctx context.Context, claims model.TokenClaims, mailboxID string, options DraftListOptions) ([]model.Draft, error) {
	if err := p.mustMailboxVisible(ctx, claims, mailboxID); err != nil {
		return nil, err
	}
	query := p.bun().NewSelect().Model((*draftRow)(nil)).Where("mailbox_id = ?", mailboxID).Order("updated_at DESC")
	if claims.AliasID != nil && strings.TrimSpace(*claims.AliasID) != "" {
		query = query.Where("alias_id = ?", strings.TrimSpace(*claims.AliasID))
	}
	if options.SourceMessageID != nil && strings.TrimSpace(*options.SourceMessageID) != "" {
		query = query.Where("source_message_id = ?", strings.TrimSpace(*options.SourceMessageID))
	}
	if status := strings.TrimSpace(string(options.Status)); status != "" {
		query = query.Where("status = ?", status)
	}
	query = query.Limit(normalizeDraftListLimit(options.Limit))
	var rows []draftRow
	if err := query.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	latestRevisionIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.LatestRevisionID != nil && strings.TrimSpace(*row.LatestRevisionID) != "" {
			latestRevisionIDs = append(latestRevisionIDs, strings.TrimSpace(*row.LatestRevisionID))
		}
	}
	var revisionRows []draftRevisionRow
	if len(latestRevisionIDs) > 0 {
		if err := p.bun().NewSelect().Model(&revisionRows).Where("id IN (?)", bun.In(latestRevisionIDs)).Scan(ctx); err != nil {
			return nil, err
		}
	}
	revisionsByID := make(map[string]draftRevisionRow, len(revisionRows))
	for _, revision := range revisionRows {
		revisionsByID[revision.ID] = revision
	}
	out := make([]model.Draft, 0, len(rows))
	for _, row := range rows {
		if row.LatestRevisionID == nil || strings.TrimSpace(*row.LatestRevisionID) == "" {
			return nil, fmt.Errorf("draft %s has no latest revision", row.ID)
		}
		revision, ok := revisionsByID[strings.TrimSpace(*row.LatestRevisionID)]
		if !ok {
			return nil, fmt.Errorf("draft %s latest revision %s not found", row.ID, strings.TrimSpace(*row.LatestRevisionID))
		}
		draft, err := draftFromRows(row, []draftRevisionRow{revision})
		if err != nil {
			return nil, err
		}
		out = append(out, draft)
	}
	return out, nil
}

func (p *Postgres) conversationContainsSourceMessage(ctx context.Context, claims model.TokenClaims, mailboxID string, conversation agentConversationRow, sourceMessageID string) (bool, error) {
	sourceMessageID = strings.TrimSpace(sourceMessageID)
	if sourceMessageID == "" {
		return false, nil
	}
	if conversation.MessageID != nil && sourceMessageID == strings.TrimSpace(*conversation.MessageID) {
		return true, nil
	}
	thread, err := p.GetThreadContextForMessage(ctx, claims, mailboxID, sourceMessageID)
	if err != nil {
		return false, err
	}
	conversationMessageID := ""
	if conversation.MessageID != nil {
		conversationMessageID = strings.TrimSpace(*conversation.MessageID)
	}
	conversationThreadID := ""
	if conversation.ThreadRFCMessageID != nil {
		conversationThreadID = strings.TrimSpace(*conversation.ThreadRFCMessageID)
	}
	for _, message := range thread {
		if conversationMessageID != "" && message.MessageID == conversationMessageID {
			return true, nil
		}
		if conversationThreadID != "" && strings.TrimSpace(message.RFCMessageID) == conversationThreadID {
			return true, nil
		}
	}
	return false, nil
}

func (p *Postgres) GetDraft(ctx context.Context, claims model.TokenClaims, mailboxID, draftID string) (model.Draft, error) {
	if err := p.mustMailboxVisible(ctx, claims, mailboxID); err != nil {
		return model.Draft{}, err
	}
	row, revs, err := p.loadDraft(ctx, draftID)
	if err != nil {
		return model.Draft{}, err
	}
	if row.MailboxID != mailboxID || !ownedByClaims(claims, row.AliasID) {
		return model.Draft{}, ErrUnauthorized
	}
	return draftFromRows(row, revs)
}

func (p *Postgres) CreateDraft(ctx context.Context, claims model.TokenClaims, input CreateDraftInput) (model.Draft, error) {
	if err := p.mustMailboxVisible(ctx, claims, input.MailboxID); err != nil {
		return model.Draft{}, err
	}
	recipients, err := sanitizeDraftRecipients(input.To)
	if err != nil {
		return model.Draft{}, err
	}
	if len(recipients) == 0 {
		return model.Draft{}, fmt.Errorf("at least one recipient is required")
	}
	aliasID, err := scopedAliasID(claims)
	if err != nil {
		return model.Draft{}, err
	}
	if input.SourceMessageID != nil && strings.TrimSpace(*input.SourceMessageID) != "" {
		if err := p.mustSourceMessageVisible(ctx, claims, input.MailboxID, *input.SourceMessageID); err != nil {
			return model.Draft{}, err
		}
	}
	if input.ConversationID != nil && strings.TrimSpace(*input.ConversationID) != "" {
		conversation, err := p.mustConversationVisible(ctx, claims, input.MailboxID, strings.TrimSpace(*input.ConversationID))
		if err != nil {
			return model.Draft{}, err
		}
		if input.SourceMessageID == nil {
			input.SourceMessageID = conversation.MessageID
		} else {
			matches, err := p.conversationContainsSourceMessage(ctx, claims, input.MailboxID, conversation, *input.SourceMessageID)
			if err != nil {
				return model.Draft{}, err
			}
			if !matches {
				return model.Draft{}, fmt.Errorf("draft source message does not belong to conversation thread")
			}
		}
		if input.ThreadRFCMessageID == nil {
			input.ThreadRFCMessageID = conversation.ThreadRFCMessageID
		} else if conversation.ThreadRFCMessageID != nil && strings.TrimSpace(*input.ThreadRFCMessageID) != strings.TrimSpace(*conversation.ThreadRFCMessageID) {
			return model.Draft{}, fmt.Errorf("draft thread does not match conversation")
		}
	}
	now := time.Now().UTC()
	draftID := uuid.NewString()
	revID := uuid.NewString()
	toJSON, err := json.Marshal(recipients)
	if err != nil {
		return model.Draft{}, err
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return model.Draft{}, err
	}
	defer tx.Rollback()
	draftInsert := draftRow{
		ID:                 draftID,
		MailboxID:          input.MailboxID,
		AliasID:            aliasID,
		ConversationID:     input.ConversationID,
		SourceMessageID:    input.SourceMessageID,
		ThreadRFCMessageID: input.ThreadRFCMessageID,
		Status:             string(model.DraftStatusPendingReview),
		LatestRevisionID:   &revID,
		ActorUserID:        actorUserID(input.Actor),
		LastRequestID:      actorRequestID(input.Actor),
		PermissionIntent:   actorPermission(input.Actor),
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	revInsert := draftRevisionRow{
		ID:               revID,
		DraftID:          draftID,
		RevisionNumber:   1,
		Provenance:       toValidUTF8(strings.TrimSpace(input.Provenance)),
		Subject:          sanitizeDraftHeader(input.Subject),
		ToEmails:         toJSON,
		Body:             toValidUTF8(input.Body),
		InReplyTo:        sanitizeDraftOptionalHeader(input.InReplyTo),
		References:       sanitizeDraftOptionalHeader(input.References),
		ActorUserID:      actorUserID(input.Actor),
		RequestID:        actorRequestID(input.Actor),
		PermissionIntent: actorPermission(input.Actor),
		CreatedAt:        now,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO drafts
		(id, mailbox_id, alias_id, conversation_id, source_message_id, thread_rfc_message_id, status, actor_user_id, last_request_id, permission_intent, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		draftInsert.ID, draftInsert.MailboxID, draftInsert.AliasID, draftInsert.ConversationID, draftInsert.SourceMessageID, draftInsert.ThreadRFCMessageID, draftInsert.Status, draftInsert.ActorUserID, draftInsert.LastRequestID, draftInsert.PermissionIntent, now, now); err != nil {
		return model.Draft{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO draft_revisions
		(id, draft_id, revision_number, provenance, subject, to_emails, body, in_reply_to, "references", actor_user_id, request_id, permission_intent, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		revInsert.ID, revInsert.DraftID, revInsert.RevisionNumber, revInsert.Provenance, revInsert.Subject, revInsert.ToEmails, revInsert.Body, revInsert.InReplyTo, revInsert.References, revInsert.ActorUserID, revInsert.RequestID, revInsert.PermissionIntent, now); err != nil {
		return model.Draft{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET latest_revision_id=$2 WHERE id=$1`, draftID, revID); err != nil {
		return model.Draft{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Draft{}, err
	}
	return p.GetDraft(ctx, claims, input.MailboxID, draftID)
}

func (p *Postgres) ReviseDraft(ctx context.Context, claims model.TokenClaims, input ReviseDraftInput) (model.Draft, error) {
	if err := p.mustMailboxVisible(ctx, claims, input.MailboxID); err != nil {
		return model.Draft{}, err
	}
	recipients, err := sanitizeDraftRecipients(input.To)
	if err != nil {
		return model.Draft{}, err
	}
	if len(recipients) == 0 {
		return model.Draft{}, fmt.Errorf("at least one recipient is required")
	}
	now := time.Now().UTC()
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return model.Draft{}, err
	}
	defer tx.Rollback()
	var row draftRow
	if err := tx.QueryRowContext(ctx, `SELECT id, mailbox_id, alias_id, status FROM drafts WHERE id=$1 FOR UPDATE`, input.DraftID).Scan(&row.ID, &row.MailboxID, &row.AliasID, &row.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Draft{}, ErrNotFound
		}
		return model.Draft{}, err
	}
	if row.MailboxID != input.MailboxID || !ownedByClaims(claims, row.AliasID) {
		return model.Draft{}, ErrUnauthorized
	}
	switch model.DraftStatus(row.Status) {
	case model.DraftStatusQueued, model.DraftStatusSent, model.DraftStatusDiscarded:
		return model.Draft{}, fmt.Errorf("draft is no longer editable")
	}
	var nextRevision int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision_number), 0) + 1 FROM draft_revisions WHERE draft_id=$1`, input.DraftID).Scan(&nextRevision); err != nil {
		return model.Draft{}, err
	}
	toJSON, err := json.Marshal(recipients)
	if err != nil {
		return model.Draft{}, err
	}
	revID := uuid.NewString()
	var previousInReplyTo, previousReferences *string
	if err := tx.QueryRowContext(ctx, `SELECT in_reply_to, "references" FROM draft_revisions WHERE draft_id=$1 ORDER BY revision_number DESC LIMIT 1`, input.DraftID).Scan(&previousInReplyTo, &previousReferences); err != nil {
		return model.Draft{}, err
	}
	inReplyTo := input.InReplyTo
	if inReplyTo == nil {
		inReplyTo = previousInReplyTo
	}
	references := input.References
	if references == nil {
		references = previousReferences
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO draft_revisions
		(id, draft_id, revision_number, provenance, subject, to_emails, body, in_reply_to, "references", actor_user_id, request_id, permission_intent, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		revID, input.DraftID, nextRevision, toValidUTF8(strings.TrimSpace(input.Provenance)), sanitizeDraftHeader(input.Subject), toJSON, toValidUTF8(input.Body), sanitizeDraftOptionalHeader(inReplyTo), sanitizeDraftOptionalHeader(references), actorUserID(input.Actor), actorRequestID(input.Actor), actorPermission(input.Actor), now); err != nil {
		return model.Draft{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts
		SET status=$2, latest_revision_id=$3, send_idempotency_key=NULL, sent_revision_id=NULL, actor_user_id=$4, last_request_id=$5, permission_intent=$6, updated_at=$7
		WHERE id=$1`,
		input.DraftID, string(model.DraftStatusPendingReview), revID, actorUserID(input.Actor), actorRequestID(input.Actor), actorPermission(input.Actor), now); err != nil {
		return model.Draft{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Draft{}, err
	}
	return p.GetDraft(ctx, claims, input.MailboxID, input.DraftID)
}

func (p *Postgres) DiscardDraft(ctx context.Context, claims model.TokenClaims, mailboxID, draftID string, actor *model.VerifiedActor) (model.Draft, error) {
	if err := p.mustMailboxVisible(ctx, claims, mailboxID); err != nil {
		return model.Draft{}, err
	}
	now := time.Now().UTC()
	query := p.bun().NewUpdate().Model((*draftRow)(nil)).
		Set("status = ?", string(model.DraftStatusDiscarded)).
		Set("actor_user_id = ?", actorUserID(actor)).
		Set("last_request_id = ?", actorRequestID(actor)).
		Set("permission_intent = ?", actorPermission(actor)).
		Set("updated_at = ?", now).
		Where("id = ?", draftID).
		Where("mailbox_id = ?", mailboxID).
		Where("status NOT IN (?)", bun.In([]string{string(model.DraftStatusQueued), string(model.DraftStatusSent)}))
	if claims.AliasID != nil && strings.TrimSpace(*claims.AliasID) != "" {
		query = query.Where("alias_id = ?", strings.TrimSpace(*claims.AliasID))
	}
	res, err := query.Exec(ctx)
	if err != nil {
		return model.Draft{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.Draft{}, ErrNotFound
	}
	return p.GetDraft(ctx, claims, mailboxID, draftID)
}

func (p *Postgres) SendDraft(ctx context.Context, claims model.TokenClaims, input SendDraftInput) (DraftSendResult, error) {
	if err := p.mustMailboxVisible(ctx, claims, input.MailboxID); err != nil {
		return DraftSendResult{}, err
	}
	input.DraftID = strings.TrimSpace(input.DraftID)
	input.ReviewedRevisionID = strings.TrimSpace(input.ReviewedRevisionID)
	input.AliasID = strings.TrimSpace(input.AliasID)
	if input.DraftID == "" || input.ReviewedRevisionID == "" {
		return DraftSendResult{}, fmt.Errorf("draft ID and reviewed revision ID are required")
	}
	if input.AliasID == "" {
		return DraftSendResult{}, ErrUnauthorized
	}
	if claims.AliasID != nil && strings.TrimSpace(*claims.AliasID) != input.AliasID {
		return DraftSendResult{}, ErrUnauthorized
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return DraftSendResult{}, err
	}
	defer tx.Rollback()
	var authorizedFrom string
	if err := tx.QueryRowContext(ctx, `SELECT email::text FROM aliases WHERE id=$1 AND mailbox_id=$2 AND enabled=true`, input.AliasID, input.MailboxID).Scan(&authorizedFrom); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DraftSendResult{}, ErrUnauthorized
		}
		return DraftSendResult{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(input.FromEmail), strings.TrimSpace(authorizedFrom)) {
		return DraftSendResult{}, ErrUnauthorized
	}
	var row draftRow
	if err := tx.QueryRowContext(ctx, `SELECT id, mailbox_id, alias_id, status, latest_revision_id, send_idempotency_key, sent_revision_id, conversation_id, source_message_id, thread_rfc_message_id, actor_user_id, last_request_id, permission_intent, created_at, updated_at
		FROM drafts WHERE id=$1 FOR UPDATE`, input.DraftID).
		Scan(&row.ID, &row.MailboxID, &row.AliasID, &row.Status, &row.LatestRevisionID, &row.SendIdempotencyKey, &row.SentRevisionID, &row.ConversationID, &row.SourceMessageID, &row.ThreadRFCMessageID, &row.ActorUserID, &row.LastRequestID, &row.PermissionIntent, &row.CreatedAt, &row.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DraftSendResult{}, ErrNotFound
		}
		return DraftSendResult{}, err
	}
	if row.MailboxID != input.MailboxID || !ownedByClaims(claims, row.AliasID) {
		return DraftSendResult{}, ErrUnauthorized
	}
	if row.LatestRevisionID == nil || *row.LatestRevisionID != input.ReviewedRevisionID {
		return DraftSendResult{}, fmt.Errorf("reviewed revision is stale")
	}
	var revRow draftRevisionRow
	if err := tx.QueryRowContext(ctx, `SELECT id, draft_id, revision_number, provenance, subject, to_emails, body, in_reply_to, "references", actor_user_id, request_id, permission_intent, created_at
		FROM draft_revisions WHERE draft_id=$1 AND id=$2 FOR UPDATE`, input.DraftID, input.ReviewedRevisionID).
		Scan(&revRow.ID, &revRow.DraftID, &revRow.RevisionNumber, &revRow.Provenance, &revRow.Subject, &revRow.ToEmails, &revRow.Body, &revRow.InReplyTo, &revRow.References, &revRow.ActorUserID, &revRow.RequestID, &revRow.PermissionIntent, &revRow.CreatedAt); err != nil {
		return DraftSendResult{}, err
	}
	key := fmt.Sprintf("draft:%s:revision:%d", input.DraftID, revRow.RevisionNumber)
	switch model.DraftStatus(row.Status) {
	case model.DraftStatusDiscarded:
		return DraftSendResult{}, fmt.Errorf("draft is discarded")
	case model.DraftStatusQueued, model.DraftStatusSent:
		if row.SendIdempotencyKey == nil || *row.SendIdempotencyKey != key {
			return DraftSendResult{}, fmt.Errorf("draft is no longer sendable")
		}
		var existing outboundMessageRow
		if err := tx.QueryRowContext(ctx, `SELECT id, mailbox_id, alias_id, from_email::text, to_emails, raw_rfc822, idempotency_key, status, attempts, next_attempt_at, lease_until, last_error, accepted_at, sent_at, created_at, updated_at
			FROM outbound_messages WHERE mailbox_id=$1 AND idempotency_key=$2`, input.MailboxID, key).
			Scan(&existing.ID, &existing.MailboxID, &existing.AliasID, &existing.FromEmail, &existing.ToEmails, &existing.RawRFC822, &existing.IdempotencyKey, &existing.Status, &existing.Attempts, &existing.NextAttemptAt, &existing.LeaseUntil, &existing.LastError, &existing.AcceptedAt, &existing.SentAt, &existing.CreatedAt, &existing.UpdatedAt); err != nil {
			return DraftSendResult{}, err
		}
		if existing.AliasID != input.AliasID {
			return DraftSendResult{}, ErrUnauthorized
		}
		if err := tx.Commit(); err != nil {
			return DraftSendResult{}, err
		}
		draft, err := p.GetDraft(ctx, claims, input.MailboxID, input.DraftID)
		if err != nil {
			return DraftSendResult{}, err
		}
		return DraftSendResult{Draft: draft, Outbound: outboundFromRow(existing), Idempotent: true}, nil
	}
	raw := buildDraftRawMessage(input.FromEmail, parseOutboundRecipients(revRow.ToEmails), revRow.Subject, revRow.Body, revRow.InReplyTo, revRow.References)
	now := time.Now().UTC()
	outboundID := uuid.NewString()
	result, err := tx.ExecContext(ctx, `INSERT INTO outbound_messages
		(id, mailbox_id, alias_id, from_email, to_emails, raw_rfc822, idempotency_key, status, attempts, next_attempt_at, accepted_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',0,$8,$8,$8,$8)
		ON CONFLICT (mailbox_id, idempotency_key) DO UPDATE SET
			status = CASE WHEN outbound_messages.status='dead' THEN 'pending' ELSE outbound_messages.status END,
			attempts = CASE WHEN outbound_messages.status='dead' THEN 0 ELSE outbound_messages.attempts END,
			next_attempt_at = CASE WHEN outbound_messages.status='dead' THEN now() ELSE outbound_messages.next_attempt_at END,
			lease_until = CASE WHEN outbound_messages.status='dead' THEN NULL ELSE outbound_messages.lease_until END,
			last_error = CASE WHEN outbound_messages.status='dead' THEN NULL ELSE outbound_messages.last_error END,
			updated_at = now()
		WHERE outbound_messages.alias_id=EXCLUDED.alias_id`,
		outboundID, input.MailboxID, input.AliasID, authorizedFrom, revRow.ToEmails, raw, key, now)
	if err != nil {
		return DraftSendResult{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return DraftSendResult{}, ErrUnauthorized
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts
		SET status=$2, send_idempotency_key=$3, sent_revision_id=$4, actor_user_id=$5, last_request_id=$6, permission_intent=$7, updated_at=$8
		WHERE id=$1`,
		input.DraftID, string(model.DraftStatusQueued), key, revRow.ID, actorUserID(input.Actor), actorRequestID(input.Actor), actorPermission(input.Actor), now); err != nil {
		return DraftSendResult{}, err
	}
	var outboundRow outboundMessageRow
	if err := tx.QueryRowContext(ctx, `SELECT id, mailbox_id, alias_id, from_email::text, to_emails, raw_rfc822, idempotency_key, status, attempts, next_attempt_at, lease_until, last_error, accepted_at, sent_at, created_at, updated_at
		FROM outbound_messages WHERE mailbox_id=$1 AND idempotency_key=$2`, input.MailboxID, key).
		Scan(&outboundRow.ID, &outboundRow.MailboxID, &outboundRow.AliasID, &outboundRow.FromEmail, &outboundRow.ToEmails, &outboundRow.RawRFC822, &outboundRow.IdempotencyKey, &outboundRow.Status, &outboundRow.Attempts, &outboundRow.NextAttemptAt, &outboundRow.LeaseUntil, &outboundRow.LastError, &outboundRow.AcceptedAt, &outboundRow.SentAt, &outboundRow.CreatedAt, &outboundRow.UpdatedAt); err != nil {
		return DraftSendResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return DraftSendResult{}, err
	}
	draft, err := p.GetDraft(ctx, claims, input.MailboxID, input.DraftID)
	if err != nil {
		return DraftSendResult{}, err
	}
	return DraftSendResult{Draft: draft, Outbound: outboundFromRow(outboundRow), Idempotent: outboundRow.ID != outboundID}, nil
}

func sanitizeDraftHeader(value string) string {
	value = strings.ReplaceAll(toValidUTF8(value), "\r\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func sanitizeDraftOptionalHeader(value *string) *string {
	if value == nil {
		return nil
	}
	clean := sanitizeDraftHeader(*value)
	return &clean
}

func sanitizeDraftRecipients(items []string) ([]string, error) {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if strings.ContainsAny(item, "\r\n") {
			return nil, fmt.Errorf("invalid recipient header")
		}
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		address, err := mail.ParseAddress(item)
		if err != nil {
			return nil, fmt.Errorf("invalid recipient %q: %w", item, err)
		}
		out = append(out, address.Address)
	}
	return out, nil
}

func buildDraftRawMessage(from string, to []string, subject string, body string, inReplyTo *string, references *string) []byte {
	from = sanitizeDraftHeader(from)
	to, _ = sanitizeDraftRecipients(to)
	subject = sanitizeDraftHeader(subject)
	inReplyTo = sanitizeDraftOptionalHeader(inReplyTo)
	references = sanitizeDraftOptionalHeader(references)
	headers := []string{
		"Message-ID: <" + uuid.NewString() + "@agent-mailboxes.invalid>",
		fmt.Sprintf("From: %s", from),
		fmt.Sprintf("To: %s", strings.Join(to, ", ")),
		fmt.Sprintf("Subject: %s", subject),
		fmt.Sprintf("Date: %s", time.Now().Format(time.RFC1123Z)),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	}
	if inReplyTo != nil && strings.TrimSpace(*inReplyTo) != "" {
		headers = append(headers, "In-Reply-To: "+strings.TrimSpace(*inReplyTo))
	}
	if references != nil && strings.TrimSpace(*references) != "" {
		headers = append(headers, "References: "+strings.TrimSpace(*references))
	}
	return []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + body)
}

func (p *Postgres) CreateAgentConversation(ctx context.Context, claims model.TokenClaims, mailboxID string, sourceMessageID *string, threadRFCMessageID *string, createdBy *string) (model.AgentConversation, error) {
	if err := p.mustMailboxVisible(ctx, claims, mailboxID); err != nil {
		return model.AgentConversation{}, err
	}
	aliasID, err := scopedAliasID(claims)
	if err != nil {
		return model.AgentConversation{}, err
	}
	if sourceMessageID != nil && strings.TrimSpace(*sourceMessageID) != "" {
		normalized := strings.TrimSpace(*sourceMessageID)
		sourceMessageID = &normalized
		if err := p.mustSourceMessageVisible(ctx, claims, mailboxID, normalized); err != nil {
			return model.AgentConversation{}, err
		}
	}
	if threadRFCMessageID != nil {
		normalized := strings.TrimSpace(*threadRFCMessageID)
		if normalized == "" {
			threadRFCMessageID = nil
		} else {
			threadRFCMessageID = &normalized
		}
	}
	now := time.Now().UTC()
	row := agentConversationRow{
		ID:                 uuid.NewString(),
		MailboxID:          mailboxID,
		AliasID:            aliasID,
		MessageID:          sourceMessageID,
		ThreadRFCMessageID: threadRFCMessageID,
		Status:             string(model.ConversationStatusRunning),
		CreatedBy:          createdBy,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	insert := p.bun().NewInsert().Model(&row)
	if threadRFCMessageID != nil && strings.TrimSpace(*threadRFCMessageID) != "" {
		if aliasID == nil {
			insert = insert.On("CONFLICT (mailbox_id, thread_rfc_message_id) WHERE alias_id IS NULL AND thread_rfc_message_id IS NOT NULL DO NOTHING")
		} else {
			insert = insert.On("CONFLICT (mailbox_id, alias_id, thread_rfc_message_id) WHERE alias_id IS NOT NULL AND thread_rfc_message_id IS NOT NULL DO NOTHING")
		}
	} else {
		if aliasID == nil {
			insert = insert.On("CONFLICT (mailbox_id, message_id) WHERE alias_id IS NULL AND message_id IS NOT NULL DO NOTHING")
		} else {
			insert = insert.On("CONFLICT (mailbox_id, alias_id, message_id) WHERE alias_id IS NOT NULL AND message_id IS NOT NULL DO NOTHING")
		}
	}
	result, err := insert.Exec(ctx)
	if err != nil {
		return model.AgentConversation{}, err
	}
	if inserted, _ := result.RowsAffected(); inserted > 0 || (sourceMessageID == nil && threadRFCMessageID == nil) {
		return agentConversationFromRow(row), nil
	}
	var existing agentConversationRow
	query := p.bun().NewSelect().Model(&existing).Where("mailbox_id = ?", mailboxID)
	if aliasID == nil {
		query = query.Where("alias_id IS NULL")
	} else {
		query = query.Where("alias_id = ?", *aliasID)
	}
	if threadRFCMessageID != nil && strings.TrimSpace(*threadRFCMessageID) != "" {
		query = query.Where("thread_rfc_message_id = ?", strings.TrimSpace(*threadRFCMessageID))
	} else {
		query = query.Where("message_id = ?", strings.TrimSpace(*sourceMessageID))
	}
	if err := query.Order("created_at ASC").Limit(1).Scan(ctx); err != nil {
		return model.AgentConversation{}, err
	}
	return agentConversationFromRow(existing), nil
}

func (p *Postgres) mustConversationVisible(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID string) (agentConversationRow, error) {
	if err := p.mustMailboxVisible(ctx, claims, mailboxID); err != nil {
		return agentConversationRow{}, err
	}
	var row agentConversationRow
	if err := p.bun().NewSelect().Model(&row).Where("id = ?", conversationID).Limit(1).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentConversationRow{}, ErrNotFound
		}
		return agentConversationRow{}, err
	}
	if row.MailboxID != mailboxID || !ownedByClaims(claims, row.AliasID) {
		return agentConversationRow{}, ErrUnauthorized
	}
	return row, nil
}

func (p *Postgres) loadConversation(ctx context.Context, conversationID string) (model.AgentConversation, error) {
	var row agentConversationRow
	if err := p.bun().NewSelect().Model(&row).Where("id = ?", conversationID).Limit(1).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.AgentConversation{}, ErrNotFound
		}
		return model.AgentConversation{}, err
	}
	conv := agentConversationFromRow(row)
	var msgRows []agentMessageRow
	if err := p.bun().NewSelect().Model(&msgRows).Where("conversation_id = ?", conversationID).Order("created_at ASC").Scan(ctx); err != nil {
		return model.AgentConversation{}, err
	}
	conv.Messages = make([]model.AgentConversationTurn, 0, len(msgRows))
	for _, msg := range msgRows {
		conv.Messages = append(conv.Messages, agentConversationTurnFromRow(msg))
	}
	var toolRows []agentToolCallRow
	if err := p.bun().NewSelect().Model(&toolRows).Where("conversation_id = ?", conversationID).Order("started_at ASC").Scan(ctx); err != nil {
		return model.AgentConversation{}, err
	}
	conv.ToolCalls = make([]model.AgentToolCall, 0, len(toolRows))
	for _, tool := range toolRows {
		conv.ToolCalls = append(conv.ToolCalls, agentToolCallFromRow(tool))
	}
	return conv, nil
}

func (p *Postgres) GetAgentConversation(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID string) (model.AgentConversation, error) {
	row, err := p.mustConversationVisible(ctx, claims, mailboxID, conversationID)
	if err != nil {
		return model.AgentConversation{}, err
	}
	return p.loadConversation(ctx, row.ID)
}

func (p *Postgres) ClaimAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken string, lease time.Duration) (bool, error) {
	if strings.TrimSpace(runToken) == "" || lease <= 0 {
		return false, fmt.Errorf("run token and positive lease are required")
	}
	if _, err := p.mustConversationVisible(ctx, claims, mailboxID, conversationID); err != nil {
		return false, err
	}
	leaseMilliseconds := lease.Milliseconds()
	if leaseMilliseconds < 1 {
		leaseMilliseconds = 1
	}
	query := p.bun().NewUpdate().Model((*agentConversationRow)(nil)).
		Set("run_token = ?", runToken).
		Set("run_lease_until = CURRENT_TIMESTAMP + (? * interval '1 millisecond')", leaseMilliseconds).
		Where("id = ?", conversationID).
		Where("mailbox_id = ?", mailboxID).
		Where("run_token IS NULL OR run_lease_until IS NULL OR run_lease_until <= CURRENT_TIMESTAMP")
	if claims.AliasID != nil && strings.TrimSpace(*claims.AliasID) != "" {
		aliasID := strings.TrimSpace(*claims.AliasID)
		query = query.
			Where("alias_id = ?", aliasID).
			Where("EXISTS (SELECT 1 FROM aliases WHERE id = ? AND mailbox_id = ? AND enabled = TRUE)", aliasID, mailboxID)
	}
	result, err := query.Exec(ctx)
	if err != nil {
		return false, err
	}
	claimed, err := result.RowsAffected()
	return claimed == 1, err
}

func (p *Postgres) ReleaseAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken string) (bool, error) {
	if strings.TrimSpace(runToken) == "" {
		return false, fmt.Errorf("run token is required")
	}
	if _, err := p.mustConversationVisible(ctx, claims, mailboxID, conversationID); err != nil {
		return false, err
	}
	result, err := p.bun().NewUpdate().Model((*agentConversationRow)(nil)).
		Set("run_token = NULL").
		Set("run_lease_until = NULL").
		Where("id = ?", conversationID).
		Where("mailbox_id = ?", mailboxID).
		Where("run_token = ?", runToken).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	released, err := result.RowsAffected()
	return released == 1, err
}

func (p *Postgres) CompleteAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken, draftID string, sourceMessageID *string, completionMessage string) error {
	conversationID = strings.TrimSpace(conversationID)
	runToken = strings.TrimSpace(runToken)
	draftID = strings.TrimSpace(draftID)
	if conversationID == "" || runToken == "" || draftID == "" {
		return fmt.Errorf("conversation id, run token, and draft id are required")
	}
	if err := p.mustMailboxVisible(ctx, claims, mailboxID); err != nil {
		return err
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var conversationAlias sql.NullString
	var storedRunToken sql.NullString
	var leaseLive bool
	err = tx.QueryRowContext(ctx, `SELECT alias_id::text, run_token, COALESCE(run_lease_until > CURRENT_TIMESTAMP, false)
		FROM agent_conversations WHERE id=$1 AND mailbox_id=$2 FOR UPDATE`, conversationID, mailboxID).
		Scan(&conversationAlias, &storedRunToken, &leaseLive)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var conversationAliasID *string
	if conversationAlias.Valid {
		conversationAliasID = &conversationAlias.String
	}
	if !ownedByClaims(claims, conversationAliasID) {
		return ErrUnauthorized
	}
	if !storedRunToken.Valid || storedRunToken.String != runToken || !leaseLive {
		return ErrAgentRunConflict
	}

	var draftMailboxID string
	var draftAlias sql.NullString
	var draftConversation sql.NullString
	var draftSource sql.NullString
	var draftStatus string
	err = tx.QueryRowContext(ctx, `SELECT mailbox_id::text, alias_id::text, conversation_id::text, source_message_id::text, status
		FROM drafts WHERE id=$1 FOR UPDATE`, draftID).
		Scan(&draftMailboxID, &draftAlias, &draftConversation, &draftSource, &draftStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var draftAliasID *string
	if draftAlias.Valid {
		draftAliasID = &draftAlias.String
	}
	if draftMailboxID != mailboxID || !ownedByClaims(claims, draftAliasID) || !draftConversation.Valid || draftConversation.String != conversationID {
		return ErrUnauthorized
	}
	expectedSource := ""
	if sourceMessageID != nil {
		expectedSource = strings.TrimSpace(*sourceMessageID)
	}
	actualSource := ""
	if draftSource.Valid {
		actualSource = strings.TrimSpace(draftSource.String)
	}
	if actualSource != expectedSource {
		return fmt.Errorf("draft source message does not match the active agent run")
	}
	switch model.DraftStatus(draftStatus) {
	case model.DraftStatusPendingReview, model.DraftStatusReadyToSend, model.DraftStatusFailed:
	default:
		return fmt.Errorf("draft status %q is not editable", draftStatus)
	}

	now := time.Now().UTC()
	if strings.TrimSpace(completionMessage) != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO agent_messages (id,conversation_id,role,content,payload,created_at)
			VALUES ($1,$2,'assistant',$3,'{}'::jsonb,$4)`, uuid.NewString(), conversationID, toValidUTF8(completionMessage), now)
		if err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_conversations
		SET status=$1,last_draft_id=$2,updated_at=$3
		WHERE id=$4 AND mailbox_id=$5 AND run_token=$6 AND run_lease_until > CURRENT_TIMESTAMP`,
		string(model.ConversationStatusCompleted), draftID, now, conversationID, mailboxID, runToken)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrAgentRunConflict
	}
	return tx.Commit()
}

func (p *Postgres) FailAgentConversationRun(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, runToken string, lastDraftID *string, failure string) error {
	conversationID = strings.TrimSpace(conversationID)
	runToken = strings.TrimSpace(runToken)
	if conversationID == "" || runToken == "" {
		return fmt.Errorf("conversation id and run token are required")
	}
	if err := p.mustMailboxVisible(ctx, claims, mailboxID); err != nil {
		return err
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var conversationAlias sql.NullString
	var storedRunToken sql.NullString
	var status string
	err = tx.QueryRowContext(ctx, `SELECT alias_id::text, run_token, status
		FROM agent_conversations WHERE id=$1 AND mailbox_id=$2 FOR UPDATE`, conversationID, mailboxID).
		Scan(&conversationAlias, &storedRunToken, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var conversationAliasID *string
	if conversationAlias.Valid {
		conversationAliasID = &conversationAlias.String
	}
	if !ownedByClaims(claims, conversationAliasID) {
		return ErrUnauthorized
	}
	if !storedRunToken.Valid || storedRunToken.String != runToken {
		return ErrAgentRunConflict
	}
	if model.ConversationStatus(status) == model.ConversationStatusCompleted {
		return ErrAgentRunConflict
	}

	var validDraftID *string
	if lastDraftID != nil && strings.TrimSpace(*lastDraftID) != "" {
		draftID := strings.TrimSpace(*lastDraftID)
		var draftMailboxID string
		var draftAlias, draftConversation sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT mailbox_id::text, alias_id::text, conversation_id::text
			FROM drafts WHERE id=$1 FOR UPDATE`, draftID).
			Scan(&draftMailboxID, &draftAlias, &draftConversation)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var draftAliasID *string
		if draftAlias.Valid {
			draftAliasID = &draftAlias.String
		}
		if draftMailboxID != mailboxID || !ownedByClaims(claims, draftAliasID) || !draftConversation.Valid || draftConversation.String != conversationID {
			return ErrUnauthorized
		}
		validDraftID = &draftID
	}

	now := time.Now().UTC()
	failure = toValidUTF8(strings.TrimSpace(failure))
	if failure == "" {
		failure = "agent run failed"
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_tool_calls
		SET status=$1, result_summary='{}'::jsonb, error_text=$2, completed_at=$3
		WHERE conversation_id=$4 AND status=$5`,
		string(model.ToolCallStatusFailed), failure, now, conversationID, string(model.ToolCallStatusStarted))
	if err != nil {
		return err
	}
	query := `UPDATE agent_conversations SET status=$1, updated_at=$2`
	args := []any{string(model.ConversationStatusFailed), now}
	if validDraftID != nil {
		query += `, last_draft_id=$3 WHERE id=$4 AND mailbox_id=$5 AND run_token=$6 AND status<>$7`
		args = append(args, *validDraftID, conversationID, mailboxID, runToken, string(model.ConversationStatusCompleted))
	} else {
		query += ` WHERE id=$3 AND mailbox_id=$4 AND run_token=$5 AND status<>$6`
		args = append(args, conversationID, mailboxID, runToken, string(model.ConversationStatusCompleted))
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrAgentRunConflict
	}
	return tx.Commit()
}

const (
	defaultAgentConversationListLimit = 100
	maxAgentConversationListLimit     = 200
)

func normalizeAgentConversationListLimit(limit int) int {
	if limit <= 0 {
		return defaultAgentConversationListLimit
	}
	if limit > maxAgentConversationListLimit {
		return maxAgentConversationListLimit
	}
	return limit
}

func (p *Postgres) ListAgentConversations(ctx context.Context, claims model.TokenClaims, mailboxID string, options AgentConversationListOptions) ([]model.AgentConversation, error) {
	if err := p.mustMailboxVisible(ctx, claims, mailboxID); err != nil {
		return nil, err
	}
	query := p.bun().NewSelect().Model((*agentConversationRow)(nil)).Where("mailbox_id = ?", mailboxID).Order("created_at DESC")
	if claims.AliasID != nil && strings.TrimSpace(*claims.AliasID) != "" {
		query = query.Where("alias_id = ?", strings.TrimSpace(*claims.AliasID))
	}
	if options.ThreadRFCMessageID != nil && strings.TrimSpace(*options.ThreadRFCMessageID) != "" {
		query = query.Where("thread_rfc_message_id = ?", strings.TrimSpace(*options.ThreadRFCMessageID))
	} else if options.SourceMessageID != nil && strings.TrimSpace(*options.SourceMessageID) != "" {
		query = query.Where("message_id = ?", strings.TrimSpace(*options.SourceMessageID))
	}
	query = query.Limit(normalizeAgentConversationListLimit(options.Limit))
	var rows []agentConversationRow
	if err := query.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]model.AgentConversation, 0, len(rows))
	for _, row := range rows {
		out = append(out, agentConversationFromRow(row))
	}
	return out, nil
}

func (p *Postgres) AddAgentConversationMessage(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, role, content string, payload json.RawMessage) (model.AgentConversationTurn, error) {
	if _, err := p.mustConversationVisible(ctx, claims, mailboxID, conversationID); err != nil {
		return model.AgentConversationTurn{}, err
	}
	now := time.Now().UTC()
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	row := agentMessageRow{
		ID:             uuid.NewString(),
		ConversationID: conversationID,
		Role:           toValidUTF8(strings.TrimSpace(role)),
		Content:        toValidUTF8(content),
		Payload:        append(json.RawMessage(nil), payload...),
		CreatedAt:      now,
	}
	_, err := p.bun().NewInsert().Model(&row).Exec(ctx)
	if err != nil {
		return model.AgentConversationTurn{}, err
	}
	_, _ = p.bun().NewUpdate().Model((*agentConversationRow)(nil)).Set("updated_at = ?", now).Where("id = ?", conversationID).Exec(ctx)
	return agentConversationTurnFromRow(row), nil
}

func (p *Postgres) StartAgentToolCall(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID, toolName string, argsSummary json.RawMessage, policyDecision string) (model.AgentToolCall, error) {
	if _, err := p.mustConversationVisible(ctx, claims, mailboxID, conversationID); err != nil {
		return model.AgentToolCall{}, err
	}
	now := time.Now().UTC()
	if len(argsSummary) == 0 {
		argsSummary = json.RawMessage(`{}`)
	}
	row := agentToolCallRow{
		ID:             uuid.NewString(),
		ConversationID: conversationID,
		ToolName:       toValidUTF8(strings.TrimSpace(toolName)),
		Status:         string(model.ToolCallStatusStarted),
		ArgsSummary:    append(json.RawMessage(nil), argsSummary...),
		ResultSummary:  json.RawMessage(`{}`),
		PolicyDecision: toValidUTF8(strings.TrimSpace(policyDecision)),
		StartedAt:      now,
	}
	_, err := p.bun().NewInsert().Model(&row).Exec(ctx)
	return agentToolCallFromRow(row), err
}

func (p *Postgres) FinishAgentToolCall(ctx context.Context, claims model.TokenClaims, mailboxID, toolCallID string, status model.ToolCallStatus, resultSummary json.RawMessage, errText *string) (model.AgentToolCall, error) {
	var conversationID string
	if err := p.DB.QueryRowContext(ctx, `SELECT conversation_id FROM agent_tool_calls WHERE id=$1`, toolCallID).Scan(&conversationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.AgentToolCall{}, ErrNotFound
		}
		return model.AgentToolCall{}, err
	}
	if _, err := p.mustConversationVisible(ctx, claims, mailboxID, conversationID); err != nil {
		return model.AgentToolCall{}, err
	}
	now := time.Now().UTC()
	if len(resultSummary) == 0 {
		resultSummary = json.RawMessage(`{}`)
	}
	res, err := p.bun().NewUpdate().Model((*agentToolCallRow)(nil)).
		Set("status = ?", string(status)).
		Set("result_summary = ?", resultSummary).
		Set("error_text = ?", sanitizeOptionalString(errText)).
		Set("completed_at = ?", now).
		Where("id = ?", toolCallID).
		Exec(ctx)
	if err != nil {
		return model.AgentToolCall{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.AgentToolCall{}, ErrNotFound
	}
	var row agentToolCallRow
	if err := p.bun().NewSelect().Model(&row).Where("id = ?", toolCallID).Limit(1).Scan(ctx); err != nil {
		return model.AgentToolCall{}, err
	}
	return agentToolCallFromRow(row), nil
}

func (p *Postgres) SetAgentConversationStatus(ctx context.Context, claims model.TokenClaims, mailboxID, conversationID string, status model.ConversationStatus, lastDraftID *string) error {
	if _, err := p.mustConversationVisible(ctx, claims, mailboxID, conversationID); err != nil {
		return err
	}
	if lastDraftID != nil && strings.TrimSpace(*lastDraftID) != "" {
		draft, err := p.GetDraft(ctx, claims, mailboxID, strings.TrimSpace(*lastDraftID))
		if err != nil {
			return err
		}
		if draft.ConversationID == nil || *draft.ConversationID != conversationID {
			return ErrUnauthorized
		}
	}
	query := p.bun().NewUpdate().Model((*agentConversationRow)(nil)).
		Set("status = ?", string(status)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", conversationID)
	if lastDraftID != nil {
		if strings.TrimSpace(*lastDraftID) == "" {
			query = query.Set("last_draft_id = NULL")
		} else {
			query = query.Set("last_draft_id = ?", strings.TrimSpace(*lastDraftID))
		}
	}
	_, err := query.Exec(ctx)
	return err
}

func defaultMailboxAgentSettings(mailboxID string) model.MailboxAgentSettings {
	return model.MailboxAgentSettings{
		MailboxID:        mailboxID,
		SystemPrompt:     "",
		EnabledTools:     defaultAgentTools(),
		AutoDraftEnabled: false,
		MaxToolSteps:     6,
		ToolTimeoutSec:   45,
		PolicyVersion:    1,
		UpdatedAt:        time.Time{},
	}
}

func (p *Postgres) GetMailboxAgentSettings(ctx context.Context, mailboxID string) (model.MailboxAgentSettings, error) {
	var row mailboxAgentSettingsRow
	if err := p.bun().NewSelect().Model(&row).Where("mailbox_id = ?", mailboxID).Limit(1).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return defaultMailboxAgentSettings(mailboxID), nil
		}
		return model.MailboxAgentSettings{}, err
	}
	return mailboxAgentSettingsFromRow(row), nil
}

func (p *Postgres) UpdateMailboxAgentSettings(ctx context.Context, mailboxID string, settings UpdateMailboxAgentSettingsInput) (model.MailboxAgentSettings, error) {
	if settings.MaxToolSteps < 0 {
		return model.MailboxAgentSettings{}, fmt.Errorf("max tool steps must not be negative")
	}
	if settings.ToolTimeoutSec < 0 {
		return model.MailboxAgentSettings{}, fmt.Errorf("tool timeout seconds must not be negative")
	}
	now := time.Now().UTC()
	insertMaxToolSteps := settings.MaxToolSteps
	if insertMaxToolSteps == 0 {
		insertMaxToolSteps = 6
	}
	insertToolTimeoutSec := settings.ToolTimeoutSec
	if insertToolTimeoutSec == 0 {
		insertToolTimeoutSec = 45
	}
	row := mailboxAgentSettingsRow{
		MailboxID:        mailboxID,
		SystemPrompt:     toValidUTF8(settings.SystemPrompt),
		EnabledTools:     append([]string{}, settings.EnabledTools...),
		ModelOverride:    sanitizeOptionalString(settings.ModelOverride),
		AutoDraftEnabled: settings.AutoDraftEnabled,
		MaxToolSteps:     insertMaxToolSteps,
		ToolTimeoutSec:   insertToolTimeoutSec,
		PolicyVersion:    1,
		UpdatedBy:        actorUserID(settings.Actor),
		LastRequestID:    actorRequestID(settings.Actor),
		PermissionIntent: actorPermission(settings.Actor),
		UpdatedAt:        now,
	}
	if settings.EnabledTools == nil {
		row.EnabledTools = defaultAgentTools()
	}
	_, err := p.bun().NewInsert().Model(&row).
		On("CONFLICT (mailbox_id) DO UPDATE").
		Set("system_prompt = EXCLUDED.system_prompt").
		Set("enabled_tools = CASE WHEN ? THEN EXCLUDED.enabled_tools ELSE mailbox_agent_settings_row.enabled_tools END", settings.EnabledTools != nil).
		Set("model_override = EXCLUDED.model_override").
		Set("auto_draft_enabled = EXCLUDED.auto_draft_enabled").
		Set("max_tool_steps = CASE WHEN ? > 0 THEN EXCLUDED.max_tool_steps ELSE mailbox_agent_settings_row.max_tool_steps END", settings.MaxToolSteps).
		Set("tool_timeout_sec = CASE WHEN ? > 0 THEN EXCLUDED.tool_timeout_sec ELSE mailbox_agent_settings_row.tool_timeout_sec END", settings.ToolTimeoutSec).
		Set("updated_by = EXCLUDED.updated_by").
		Set("last_request_id = EXCLUDED.last_request_id").
		Set("permission_intent = EXCLUDED.permission_intent").
		Set("updated_at = EXCLUDED.updated_at").
		Set("policy_version = mailbox_agent_settings_row.policy_version + 1").
		Exec(ctx)
	if err != nil {
		return model.MailboxAgentSettings{}, err
	}
	return p.GetMailboxAgentSettings(ctx, mailboxID)
}

func (p *Postgres) RecordActorReplay(ctx context.Context, mailboxID string, requestID string, nonce string, action string, targetID string, expiresAt time.Time) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Bound maintenance work per request while retaining every live replay marker.
	if _, err := tx.ExecContext(ctx, `DELETE FROM actor_request_replays WHERE ctid IN (SELECT ctid FROM actor_request_replays WHERE expires_at < CURRENT_TIMESTAMP ORDER BY expires_at LIMIT 1000 FOR UPDATE SKIP LOCKED)`); err != nil {
		return err
	}
	row := actorReplayRow{
		MailboxID: mailboxID,
		RequestID: toValidUTF8(strings.TrimSpace(requestID)),
		Nonce:     toValidUTF8(strings.TrimSpace(nonce)),
		Action:    toValidUTF8(strings.TrimSpace(action)),
		TargetID:  toValidUTF8(strings.TrimSpace(targetID)),
		ExpiresAt: expiresAt.UTC(),
		CreatedAt: time.Now().UTC(),
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO actor_request_replays (mailbox_id,request_id,nonce,action,target_id,expires_at,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, row.MailboxID, row.RequestID, row.Nonce, row.Action, row.TargetID, row.ExpiresAt, row.CreatedAt)
	if err == nil {
		return tx.Commit()
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return ErrUnauthorized
	}
	return err
}

func (p *Postgres) QueueAutoDraftJob(ctx context.Context, mailboxID, messageID string, policyVersion int) (AutoDraftJob, bool, error) {
	now := time.Now().UTC()
	row := autoDraftJobRow{
		ID:            uuid.NewString(),
		MailboxID:     mailboxID,
		MessageID:     messageID,
		PolicyVersion: policyVersion,
		Status:        "pending",
		Attempts:      0,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	_, err := p.bun().NewInsert().Model(&row).
		On("CONFLICT (mailbox_id, message_id, policy_version) DO UPDATE").
		Set("updated_at = now()").
		Exec(ctx)
	if err != nil {
		return AutoDraftJob{}, false, err
	}
	var stored autoDraftJobRow
	if err := p.bun().NewSelect().Model(&stored).
		Where("mailbox_id = ?", mailboxID).
		Where("message_id = ?", messageID).
		Where("policy_version = ?", policyVersion).
		Limit(1).
		Scan(ctx); err != nil {
		return AutoDraftJob{}, false, err
	}
	return autoDraftJobFromRow(stored), stored.ID == row.ID, nil
}

func (p *Postgres) ClaimAutoDraftJob(ctx context.Context, mailboxID string, lease time.Duration) (AutoDraftJob, bool, error) {
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	leaseMillis := lease.Milliseconds()
	if leaseMillis < 1 {
		leaseMillis = 1
	}
	claimToken := uuid.NewString()
	var row autoDraftJobRow
	err := p.DB.QueryRowContext(ctx, `WITH claim AS (
SELECT j.id
FROM auto_draft_jobs j
JOIN mailboxes mb ON mb.id=j.mailbox_id
WHERE j.mailbox_id=$1
  AND mb.enabled=true
  AND j.next_attempt_at<=now()
  AND (j.status='pending' OR (j.status='processing' AND (j.lease_until IS NULL OR j.lease_until < now())))
ORDER BY j.next_attempt_at ASC, j.created_at ASC
FOR UPDATE SKIP LOCKED
LIMIT 1
)
UPDATE auto_draft_jobs j
SET status='processing', attempts=j.attempts+1, claim_token=$2, lease_until=now()+($3::bigint * interval '1 millisecond'), updated_at=now()
FROM claim
WHERE j.id=claim.id
RETURNING j.id::text,j.mailbox_id::text,j.message_id::text,j.policy_version,j.status,j.attempts,j.claim_token::text,j.next_attempt_at,j.lease_until,j.last_error,j.created_at,j.updated_at`,
		mailboxID, claimToken, leaseMillis).
		Scan(&row.ID, &row.MailboxID, &row.MessageID, &row.PolicyVersion, &row.Status, &row.Attempts, &claimToken, &row.NextAttemptAt, &row.LeaseUntil, &row.LastError, &row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AutoDraftJob{}, false, nil
	}
	if err != nil {
		return AutoDraftJob{}, false, err
	}
	row.ClaimToken = &claimToken
	return autoDraftJobFromRow(row), true, nil
}

func (p *Postgres) RenewAutoDraftJobLease(ctx context.Context, jobID, claimToken string, lease time.Duration) (bool, error) {
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	leaseMillis := lease.Milliseconds()
	if leaseMillis < 1 {
		leaseMillis = 1
	}
	result, err := p.DB.ExecContext(ctx, `UPDATE auto_draft_jobs
SET lease_until=now()+($3::bigint * interval '1 millisecond'), updated_at=now()
WHERE id=$1 AND status='processing' AND claim_token=$2 AND lease_until>now()`, jobID, claimToken, leaseMillis)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func autoDraftRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 5 {
		attempt = 5
	}
	return time.Duration(attempt) * time.Minute
}

func (p *Postgres) MarkAutoDraftJobDone(ctx context.Context, jobID, claimToken string) (bool, error) {
	result, err := p.DB.ExecContext(ctx, `UPDATE auto_draft_jobs
SET status='done', claim_token=NULL, lease_until=NULL, updated_at=now()
WHERE id=$1 AND status='processing' AND claim_token=$2 AND lease_until>now()`, jobID, claimToken)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (p *Postgres) MarkAutoDraftJobRetry(ctx context.Context, jobID, claimToken, lastErr string, retryAfter time.Duration) (bool, error) {
	if retryAfter <= 0 {
		retryAfter = 2 * time.Minute
	}
	retryMillis := retryAfter.Milliseconds()
	if retryMillis < 1 {
		retryMillis = 1
	}
	result, err := p.DB.ExecContext(ctx, `UPDATE auto_draft_jobs
SET status='pending', claim_token=NULL, lease_until=NULL, last_error=$3,
    next_attempt_at=now()+($4::bigint * interval '1 millisecond'), updated_at=now()
WHERE id=$1 AND status='processing' AND claim_token=$2 AND lease_until>now()`,
		jobID, claimToken, toValidUTF8(strings.TrimSpace(lastErr)), retryMillis)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (p *Postgres) CreateAutoDraftRun(ctx context.Context, mailboxID, messageID, jobID string, policyVersion int, status string, failureReason *string) (model.AutoDraftRun, error) {
	now := time.Now().UTC()
	row := autoDraftRunRow{
		ID:            uuid.NewString(),
		MailboxID:     mailboxID,
		MessageID:     messageID,
		JobID:         jobID,
		PolicyVersion: policyVersion,
		Status:        toValidUTF8(strings.TrimSpace(status)),
		FailureReason: sanitizeOptionalString(failureReason),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	_, err := p.bun().NewInsert().Model(&row).Exec(ctx)
	if err != nil {
		return model.AutoDraftRun{}, err
	}
	return model.AutoDraftRun{
		ID:            row.ID,
		MailboxID:     row.MailboxID,
		MessageID:     row.MessageID,
		JobID:         row.JobID,
		PolicyVersion: row.PolicyVersion,
		Status:        row.Status,
		FailureReason: row.FailureReason,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}, nil
}

func (p *Postgres) UpdateAutoDraftRunStatus(ctx context.Context, runID string, status string, failureReason *string) error {
	_, err := p.bun().NewUpdate().Model((*autoDraftRunRow)(nil)).
		Set("status = ?", toValidUTF8(strings.TrimSpace(status))).
		Set("failure_reason = ?", sanitizeOptionalString(failureReason)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", runID).
		Exec(ctx)
	return err
}
