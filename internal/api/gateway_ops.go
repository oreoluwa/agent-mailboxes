package api

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/service"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type EventBroker struct {
	mu     sync.RWMutex
	nextID int
	subs   map[int]eventSubscription
}

type eventSubscription struct {
	id          int
	mailboxID   string
	mailboxName string
	claims      model.TokenClaims
	ch          chan service.MailboxEvent
}

func NewEventBroker() *EventBroker {
	return &EventBroker{
		subs: map[int]eventSubscription{},
	}
}

func (b *EventBroker) Publish(ev service.MailboxEvent) {
	if b == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range b.subs {
		if sub.mailboxID != "" && sub.mailboxID != ev.MailboxID {
			continue
		}
		if sub.mailboxName != "" && !strings.EqualFold(sub.mailboxName, ev.MailboxName) {
			continue
		}
		if !mailboxEventVisibleToClaims(ev, sub.claims) {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

func (b *EventBroker) Subscribe(mailboxID, mailboxName string, claims model.TokenClaims) (<-chan service.MailboxEvent, func()) {
	ch := make(chan service.MailboxEvent, 32)
	if b == nil {
		return ch, func() { close(ch) }
	}
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = eventSubscription{
		id:          id,
		mailboxID:   strings.TrimSpace(mailboxID),
		mailboxName: strings.TrimSpace(mailboxName),
		claims:      claims,
		ch:          ch,
	}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		if sub, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(sub.ch)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

func mailboxEventVisibleToClaims(ev service.MailboxEvent, claims model.TokenClaims) bool {
	if claims.MailboxID != nil && strings.TrimSpace(*claims.MailboxID) != strings.TrimSpace(ev.MailboxID) {
		return false
	}
	// An alias-bound token remains alias-bound even if it also carries an admin
	// scope. Only a mailbox administrator without an alias binding may observe
	// unscoped events.
	if claims.AliasID == nil || strings.TrimSpace(*claims.AliasID) == "" {
		return claims.IsAdmin()
	}
	aliasID := strings.TrimSpace(*claims.AliasID)
	if aliasID == strings.TrimSpace(ev.OwningAliasID) {
		return true
	}
	for _, audienceAliasID := range ev.Audience {
		if aliasID == strings.TrimSpace(audienceAliasID) {
			return true
		}
	}
	return false
}

type listInboxParams struct {
	Folder string
	Unread bool
	Limit  int
}

type messageParams struct {
	MailboxName string
	MessageID   string
}

type sendMessageParams struct {
	MailboxName    string
	To             []string
	Subject        string
	Body           string
	IdempotencyKey string
}

type sendRawMessageParams struct {
	MailboxName    string
	From           string
	To             []string
	RawRFC822      []byte
	IdempotencyKey string
}

type replyMessageParams struct {
	MailboxName    string
	MessageID      string
	Body           string
	IdempotencyKey string
}

type draftIDParams struct {
	MailboxName string
	DraftID     string
}

type listDraftsParams struct {
	MailboxName     string
	SourceMessageID *string
	Status          model.DraftStatus
	Limit           int
}

type listAgentConversationsParams struct {
	MailboxName     string
	SourceMessageID *string
	Limit           int
}

type createDraftParams struct {
	MailboxName     string
	SourceMessageID *string
	ConversationID  *string
	Subject         string
	To              []string
	Body            string
	Actor           *model.VerifiedActor
}

type reviseDraftParams struct {
	MailboxName string
	DraftID     string
	Subject     string
	To          []string
	Body        string
	Actor       *model.VerifiedActor
}

type sendFromDraftParams struct {
	MailboxName        string
	DraftID            string
	ReviewedRevisionID string
	Actor              *model.VerifiedActor
}

func draftApprovalTarget(draftID, revisionID string) string {
	return "draft:" + strings.TrimSpace(draftID) + ":revision:" + strings.TrimSpace(revisionID)
}

type runAgentParams struct {
	MailboxName    string
	ConversationID *string
	MessageID      *string
	Prompt         string
	Actor          *model.VerifiedActor
}

func (s *Server) listInboxOp(ctx context.Context, claims model.TokenClaims, req listInboxParams) ([]model.ListItem, error) {
	if strings.EqualFold(req.Folder, "all") && !s.AllowInboxAll {
		return nil, errors.New("folder \"all\" is disabled")
	}
	items, err := s.Store.ListInbox(ctx, claims, strings.TrimSpace(req.Folder), req.Unread, req.Limit)
	if err != nil {
		return nil, err
	}
	_ = s.Store.RecordAudit(ctx, claims.TokenID, "list", nil, nil)
	return items, nil
}

func (s *Server) listFoldersOp(ctx context.Context, claims model.TokenClaims) ([]model.FolderSummary, error) {
	folders, err := s.Store.ListFolders(ctx, claims)
	if err != nil {
		return nil, err
	}
	_ = s.Store.RecordAudit(ctx, claims.TokenID, "list_folders", nil, nil)
	return folders, nil
}

func (s *Server) readMessageOp(ctx context.Context, claims model.TokenClaims, req messageParams) (model.Message, error) {
	m, err := s.Store.ReadMessage(ctx, claims, strings.TrimSpace(req.MessageID))
	if err != nil {
		return model.Message{}, err
	}
	id := strings.TrimSpace(req.MessageID)
	_ = s.Store.RecordAudit(ctx, claims.TokenID, "read", &id, nil)
	return m, nil
}

func (s *Server) getMessageBodyOp(ctx context.Context, claims model.TokenClaims, req messageParams) (model.Body, error) {
	b, err := s.Store.GetBody(ctx, claims, strings.TrimSpace(req.MessageID))
	if err != nil {
		return model.Body{}, err
	}
	id := strings.TrimSpace(req.MessageID)
	_ = s.Store.RecordAudit(ctx, claims.TokenID, "fetch_body", &id, nil)
	return b, nil
}

func (s *Server) getRawMessageOp(ctx context.Context, claims model.TokenClaims, req messageParams) ([]byte, error) {
	raw, err := s.Store.GetRawMessage(ctx, claims, strings.TrimSpace(req.MessageID))
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(req.MessageID)
	_ = s.Store.RecordAudit(ctx, claims.TokenID, "fetch_raw", &id, nil)
	return raw, nil
}

func (s *Server) ackMessageOp(ctx context.Context, claims model.TokenClaims, req messageParams) error {
	messageID := strings.TrimSpace(req.MessageID)
	if err := s.Store.AckMessage(ctx, claims, messageID); err != nil {
		return err
	}
	_ = s.Store.RecordAudit(ctx, claims.TokenID, "ack", &messageID, nil)
	if svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName); err == nil {
		s.publishBrokerEvent(svc, "message_acked", messageID, "", claims.AliasID, nil)
	}
	return nil
}

func (s *Server) unackMessageOp(ctx context.Context, claims model.TokenClaims, req messageParams) error {
	messageID := strings.TrimSpace(req.MessageID)
	if err := s.Store.UnackMessage(ctx, claims, messageID); err != nil {
		return err
	}
	_ = s.Store.RecordAudit(ctx, claims.TokenID, "unack", &messageID, nil)
	if svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName); err == nil {
		s.publishBrokerEvent(svc, "message_unacked", messageID, "", claims.AliasID, nil)
	}
	return nil
}

func (s *Server) sendMessageOp(ctx context.Context, claims model.TokenClaims, req sendMessageParams) (store.OutboundMessage, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return store.OutboundMessage{}, err
	}
	return svc.SendWithIdempotency(ctx, claims, req.To, req.Subject, req.Body, req.IdempotencyKey)
}

func (s *Server) sendRawMessageOp(ctx context.Context, claims model.TokenClaims, req sendRawMessageParams) (store.OutboundMessage, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return store.OutboundMessage{}, err
	}
	return svc.SendRawWithIdempotency(ctx, claims, req.From, req.To, req.RawRFC822, req.IdempotencyKey)
}

func (s *Server) replyMessageOp(ctx context.Context, claims model.TokenClaims, req replyMessageParams) (store.OutboundMessage, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return store.OutboundMessage{}, err
	}
	return svc.ReplyWithIdempotency(ctx, claims, req.MessageID, req.Body, req.IdempotencyKey)
}

func (s *Server) listDraftsOp(ctx context.Context, claims model.TokenClaims, req listDraftsParams) ([]model.Draft, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return nil, err
	}
	return svc.ListDrafts(ctx, claims, store.DraftListOptions{
		SourceMessageID: req.SourceMessageID,
		Status:          req.Status,
		Limit:           req.Limit,
	})
}

func (s *Server) getDraftOp(ctx context.Context, claims model.TokenClaims, req draftIDParams) (model.Draft, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return model.Draft{}, err
	}
	return svc.GetDraft(ctx, claims, req.DraftID)
}

func (s *Server) createDraftOp(ctx context.Context, claims model.TokenClaims, req createDraftParams) (model.Draft, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return model.Draft{}, err
	}
	draft, err := svc.CreateDraft(ctx, claims, store.CreateDraftInput{
		ConversationID:  req.ConversationID,
		SourceMessageID: req.SourceMessageID,
		Subject:         req.Subject,
		To:              req.To,
		Body:            req.Body,
		Provenance:      "human",
		Actor:           req.Actor,
	})
	if err == nil {
		_ = s.Store.RecordAudit(ctx, claims.TokenID, "draft_create", req.SourceMessageID, &draft.ID)
		s.publishBrokerEvent(svc, "draft_created", draft.ID, draft.LatestRevision.Subject, draft.AliasID, nil)
	}
	return draft, err
}

func (s *Server) reviseDraftOp(ctx context.Context, claims model.TokenClaims, req reviseDraftParams) (model.Draft, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return model.Draft{}, err
	}
	draft, err := svc.ReviseDraft(ctx, claims, store.ReviseDraftInput{
		DraftID:    req.DraftID,
		Subject:    req.Subject,
		To:         req.To,
		Body:       req.Body,
		Provenance: "human",
		Actor:      req.Actor,
	})
	if err == nil {
		_ = s.Store.RecordAudit(ctx, claims.TokenID, "draft_update", nil, &draft.ID)
		s.publishBrokerEvent(svc, "draft_revised", draft.ID, draft.LatestRevision.Subject, draft.AliasID, nil)
	}
	return draft, err
}

func (s *Server) discardDraftOp(ctx context.Context, claims model.TokenClaims, req sendFromDraftParams) (model.Draft, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return model.Draft{}, err
	}
	draft, err := svc.DiscardDraft(ctx, claims, req.DraftID, req.Actor)
	if err == nil {
		_ = s.Store.RecordAudit(ctx, claims.TokenID, "draft_discard", nil, &draft.ID)
		s.publishBrokerEvent(svc, "draft_discarded", draft.ID, draft.LatestRevision.Subject, draft.AliasID, nil)
	}
	return draft, err
}

func (s *Server) sendFromDraftOp(ctx context.Context, claims model.TokenClaims, req sendFromDraftParams) (store.DraftSendResult, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return store.DraftSendResult{}, err
	}
	result, err := svc.SendFromDraft(ctx, claims, req.DraftID, req.ReviewedRevisionID, req.Actor)
	if err == nil {
		_ = s.Store.RecordAudit(ctx, claims.TokenID, "draft_send", nil, &result.Draft.ID)
	}
	return result, err
}

func (s *Server) runAgentOp(ctx context.Context, claims model.TokenClaims, req runAgentParams) (service.RunAgentResult, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return service.RunAgentResult{}, err
	}
	result, err := svc.RunAgent(ctx, claims, service.RunAgentInput{
		ConversationID: req.ConversationID,
		MessageID:      req.MessageID,
		Prompt:         req.Prompt,
		Actor:          req.Actor,
	})
	if err == nil {
		target := result.Conversation.ID
		_ = s.Store.RecordAudit(ctx, claims.TokenID, "agent_run", nil, &target)
	}
	return result, err
}

func (s *Server) getMailboxAgentContextOp(ctx context.Context, claims model.TokenClaims, mailboxName string) (model.MailboxAgentContext, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, mailboxName)
	if err != nil {
		return model.MailboxAgentContext{}, err
	}
	return model.MailboxAgentContext{MailboxID: svc.MailboxID}, nil
}

func (s *Server) updateMailboxAgentSettingsOp(ctx context.Context, claims model.TokenClaims, mailboxName string, input store.UpdateMailboxAgentSettingsInput) (model.MailboxAgentSettings, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, mailboxName)
	if err != nil {
		return model.MailboxAgentSettings{}, err
	}
	settings, err := svc.UpdateMailboxAgentSettings(ctx, claims, input)
	if err == nil {
		target := svc.MailboxID
		_ = s.Store.RecordAudit(ctx, claims.TokenID, "settings_update", nil, &target)
	}
	return settings, err
}

func (s *Server) getAgentConversationOp(ctx context.Context, claims model.TokenClaims, req draftIDParams) (model.AgentConversation, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return model.AgentConversation{}, err
	}
	return svc.GetAgentConversation(ctx, claims, req.DraftID)
}

func (s *Server) listAgentConversationsOp(ctx context.Context, claims model.TokenClaims, req listAgentConversationsParams) ([]model.AgentConversation, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, req.MailboxName)
	if err != nil {
		return nil, err
	}
	return svc.ListAgentConversations(ctx, claims, req.SourceMessageID, req.Limit)
}

func (s *Server) searchMessagesOp(ctx context.Context, claims model.TokenClaims, mailboxName, query, folder string, limit int) ([]model.ListItem, error) {
	req := model.SearchRequest{
		Folder: strings.TrimSpace(folder),
		Limit:  limit,
		Intent: model.SearchIntent{Text: strings.TrimSpace(query)},
		Mode:   model.SearchModeHybrid,
	}
	return s.searchStructuredOp(ctx, claims, mailboxName, req)
}

func (s *Server) searchStructuredOp(ctx context.Context, claims model.TokenClaims, mailboxName string, req model.SearchRequest) ([]model.ListItem, error) {
	svc, err := s.serviceForClaimsMailbox(ctx, claims, mailboxName)
	if err != nil {
		return nil, err
	}
	if claims.IsAdmin() && claims.AliasID == nil {
		mailboxID := svc.MailboxID
		claims.MailboxID = &mailboxID
	}
	req = svc.NormalizeSearchRequest(ctx, req)
	items, err := svc.SearchStructured(ctx, claims, req)
	if err != nil {
		return nil, err
	}
	summary := summarizeSearchRequest(req, len(items))
	_ = s.Store.RecordAudit(ctx, claims.TokenID, "search", nil, &summary)
	return items, nil
}

func (s *Server) serviceForClaimsMailbox(ctx context.Context, claims model.TokenClaims, mailboxName string) (*service.Service, error) {
	if claims.AliasID != nil {
		mailboxID, err := s.Store.GetMailboxIDByAliasID(ctx, *claims.AliasID)
		if err != nil {
			return nil, err
		}
		s.mu.RLock()
		if svc, ok := s.ServicesByMailboxID[mailboxID]; ok {
			s.mu.RUnlock()
			return svc, nil
		}
		if s.Svc != nil && s.Svc.MailboxID == mailboxID {
			svc := s.Svc
			s.mu.RUnlock()
			return svc, nil
		}
		s.mu.RUnlock()
		return s.loadMailboxService(ctx, mailboxID, "")
	}
	return s.serviceForMailboxContext(ctx, strings.TrimSpace(mailboxName))
}

func (s *Server) publishBrokerEvent(svc *service.Service, eventType, messageID, subject string, owningAliasID *string, audience []string) {
	if s.Broker == nil || svc == nil {
		return
	}
	occurredAt := time.Now().UTC()
	aliasID := ""
	if owningAliasID != nil {
		aliasID = strings.TrimSpace(*owningAliasID)
	}
	s.Broker.Publish(service.MailboxEvent{
		MailboxID:     svc.MailboxID,
		MailboxName:   svc.MailboxName,
		OwningAliasID: aliasID,
		Audience:      append([]string(nil), audience...),
		EventType:     eventType,
		MessageID:     strings.TrimSpace(messageID),
		OccurredAt:    occurredAt,
		Cursor:        strconv.FormatInt(occurredAt.UnixNano(), 10),
		Subject:       strings.TrimSpace(subject),
	})
}

func decodeRawBase64(v string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimSpace(v))
}
