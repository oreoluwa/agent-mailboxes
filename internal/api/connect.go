package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	connect "connectrpc.com/connect"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
	emaildv1 "github.com/oreoluwa/agent-mailboxes/proto/agentemails/emaild/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type ConnectServer struct {
	Server *Server
}

func (s *Server) ConnectHandler() *ConnectServer {
	return &ConnectServer{Server: s}
}

func (c *ConnectServer) ListFolders(ctx context.Context, req *connect.Request[emaildv1.ListFoldersRequest]) (*connect.Response[emaildv1.ListFoldersResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	folders, err := c.Server.listFoldersOp(ctx, claims)
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.ListFoldersResponse{Folders: mapFolders(folders)}), nil
}

func (c *ConnectServer) ListInbox(ctx context.Context, req *connect.Request[emaildv1.ListInboxRequest]) (*connect.Response[emaildv1.ListInboxResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	items, err := c.Server.listInboxOp(ctx, claims, listInboxParams{
		Folder: req.Msg.GetFolder(),
		Unread: req.Msg.GetUnread(),
		Limit:  int(req.Msg.GetLimit()),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.ListInboxResponse{Items: mapMessageSummaries(items)}), nil
}

func (c *ConnectServer) ReadMessage(ctx context.Context, req *connect.Request[emaildv1.ReadMessageRequest]) (*connect.Response[emaildv1.ReadMessageResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	msg, err := c.Server.readMessageOp(ctx, claims, messageParams{
		MailboxName: req.Msg.GetMailboxName(),
		MessageID:   req.Msg.GetMessageId(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.ReadMessageResponse{Message: mapMessage(msg)}), nil
}

func (c *ConnectServer) GetMessageBody(ctx context.Context, req *connect.Request[emaildv1.GetMessageBodyRequest]) (*connect.Response[emaildv1.GetMessageBodyResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	body, err := c.Server.getMessageBodyOp(ctx, claims, messageParams{
		MailboxName: req.Msg.GetMailboxName(),
		MessageID:   req.Msg.GetMessageId(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.GetMessageBodyResponse{Body: mapBody(body)}), nil
}

func (c *ConnectServer) GetRawMessage(ctx context.Context, req *connect.Request[emaildv1.GetRawMessageRequest]) (*connect.Response[emaildv1.GetRawMessageResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	raw, err := c.Server.getRawMessageOp(ctx, claims, messageParams{
		MailboxName: req.Msg.GetMailboxName(),
		MessageID:   req.Msg.GetMessageId(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.GetRawMessageResponse{Rfc822: raw}), nil
}

func (c *ConnectServer) SearchMessages(ctx context.Context, req *connect.Request[emaildv1.SearchMessagesRequest]) (*connect.Response[emaildv1.SearchMessagesResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	items, err := c.Server.searchMessagesOp(ctx, claims, req.Msg.GetMailboxName(), req.Msg.GetQuery(), req.Msg.GetFolder(), int(req.Msg.GetLimit()))
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.SearchMessagesResponse{Items: mapMessageSummaries(items)}), nil
}

func (c *ConnectServer) SearchStructured(ctx context.Context, req *connect.Request[emaildv1.SearchStructuredRequest]) (*connect.Response[emaildv1.SearchStructuredResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	items, err := c.Server.searchStructuredOp(ctx, claims, req.Msg.GetMailboxName(), unmapSearchRequest(req.Msg.GetRequest()))
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.SearchStructuredResponse{Items: mapMessageSummaries(items)}), nil
}

func (c *ConnectServer) SendMessage(ctx context.Context, req *connect.Request[emaildv1.SendMessageRequest]) (*connect.Response[emaildv1.QueuedResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeSend)
	if err != nil {
		return nil, err
	}
	job, err := c.Server.sendMessageOp(ctx, claims, sendMessageParams{
		MailboxName:    req.Msg.GetMailboxName(),
		To:             req.Msg.GetTo(),
		Subject:        req.Msg.GetSubject(),
		Body:           req.Msg.GetBody(),
		IdempotencyKey: req.Msg.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.QueuedResponse{Status: "queued", JobId: job.ID}), nil
}

func (c *ConnectServer) SendRawMessage(ctx context.Context, req *connect.Request[emaildv1.SendRawMessageRequest]) (*connect.Response[emaildv1.QueuedResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeSend)
	if err != nil {
		return nil, err
	}
	job, err := c.Server.sendRawMessageOp(ctx, claims, sendRawMessageParams{
		MailboxName:    req.Msg.GetMailboxName(),
		From:           req.Msg.GetFrom(),
		To:             req.Msg.GetTo(),
		RawRFC822:      req.Msg.GetRawRfc822(),
		IdempotencyKey: req.Msg.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.QueuedResponse{Status: "queued", JobId: job.ID}), nil
}

func (c *ConnectServer) ReplyMessage(ctx context.Context, req *connect.Request[emaildv1.ReplyMessageRequest]) (*connect.Response[emaildv1.QueuedResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeSend)
	if err != nil {
		return nil, err
	}
	job, err := c.Server.replyMessageOp(ctx, claims, replyMessageParams{
		MailboxName:    req.Msg.GetMailboxName(),
		MessageID:      req.Msg.GetMessageId(),
		Body:           req.Msg.GetBody(),
		IdempotencyKey: req.Msg.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.QueuedResponse{Status: "queued", JobId: job.ID}), nil
}

func (c *ConnectServer) AckMessage(ctx context.Context, req *connect.Request[emaildv1.AckMessageRequest]) (*connect.Response[emaildv1.AckMessageResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	if err := c.Server.ackMessageOp(ctx, claims, messageParams{
		MailboxName: req.Msg.GetMailboxName(),
		MessageID:   req.Msg.GetMessageId(),
	}); err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.AckMessageResponse{Status: "ok"}), nil
}

func (c *ConnectServer) UnackMessage(ctx context.Context, req *connect.Request[emaildv1.UnackMessageRequest]) (*connect.Response[emaildv1.UnackMessageResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	if err := c.Server.unackMessageOp(ctx, claims, messageParams{
		MailboxName: req.Msg.GetMailboxName(),
		MessageID:   req.Msg.GetMessageId(),
	}); err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.UnackMessageResponse{Status: "ok"}), nil
}

func (c *ConnectServer) ListDrafts(ctx context.Context, req *connect.Request[emaildv1.ListDraftsRequest]) (*connect.Response[emaildv1.ListDraftsResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	var sourceMessageID *string
	if req.Msg.SourceMessageId != nil && strings.TrimSpace(req.Msg.GetSourceMessageId()) != "" {
		value := strings.TrimSpace(req.Msg.GetSourceMessageId())
		sourceMessageID = &value
	}
	items, err := c.Server.listDraftsOp(ctx, claims, listDraftsParams{
		MailboxName:     req.Msg.GetMailboxName(),
		SourceMessageID: sourceMessageID,
		Status:          model.DraftStatus(strings.TrimSpace(req.Msg.GetStatus())),
		Limit:           int(req.Msg.GetLimit()),
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.ListDraftsResponse{Drafts: mapDrafts(items)}), nil
}

func (c *ConnectServer) GetDraft(ctx context.Context, req *connect.Request[emaildv1.GetDraftRequest]) (*connect.Response[emaildv1.GetDraftResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	item, err := c.Server.getDraftOp(ctx, claims, draftIDParams{MailboxName: req.Msg.GetMailboxName(), DraftID: req.Msg.GetDraftId()})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.GetDraftResponse{Draft: mapDraft(item)}), nil
}

func (c *ConnectServer) CreateDraft(ctx context.Context, req *connect.Request[emaildv1.CreateDraftRequest]) (*connect.Response[emaildv1.CreateDraftResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeSend)
	if err != nil {
		return nil, err
	}
	svc, err := c.Server.serviceForClaimsMailbox(ctx, claims, req.Msg.GetMailboxName())
	if err != nil {
		return nil, connectError(err)
	}
	target := svc.MailboxID
	if req.Msg.SourceMessageId != nil && strings.TrimSpace(req.Msg.GetSourceMessageId()) != "" {
		target = strings.TrimSpace(req.Msg.GetSourceMessageId())
	}
	actor, err := c.Server.verifyActorEnvelope(ctx, req.Header(), svc.MailboxID, "send", "create_draft", target)
	if err != nil {
		return nil, connectUnauthorized(err)
	}
	draft, err := c.Server.createDraftOp(ctx, claims, createDraftParams{
		MailboxName:     req.Msg.GetMailboxName(),
		SourceMessageID: req.Msg.SourceMessageId,
		ConversationID:  req.Msg.ConversationId,
		Subject:         req.Msg.GetSubject(),
		To:              req.Msg.GetTo(),
		Body:            req.Msg.GetBody(),
		Actor:           actor,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.CreateDraftResponse{Draft: mapDraft(draft)}), nil
}

func (c *ConnectServer) ReviseDraft(ctx context.Context, req *connect.Request[emaildv1.ReviseDraftRequest]) (*connect.Response[emaildv1.ReviseDraftResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeSend)
	if err != nil {
		return nil, err
	}
	svc, err := c.Server.serviceForClaimsMailbox(ctx, claims, req.Msg.GetMailboxName())
	if err != nil {
		return nil, connectError(err)
	}
	actor, err := c.Server.verifyActorEnvelope(ctx, req.Header(), svc.MailboxID, "send", "revise_draft", strings.TrimSpace(req.Msg.GetDraftId()))
	if err != nil {
		return nil, connectUnauthorized(err)
	}
	draft, err := c.Server.reviseDraftOp(ctx, claims, reviseDraftParams{
		MailboxName: req.Msg.GetMailboxName(),
		DraftID:     req.Msg.GetDraftId(),
		Subject:     req.Msg.GetSubject(),
		To:          req.Msg.GetTo(),
		Body:        req.Msg.GetBody(),
		Actor:       actor,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.ReviseDraftResponse{Draft: mapDraft(draft)}), nil
}

func (c *ConnectServer) DiscardDraft(ctx context.Context, req *connect.Request[emaildv1.DiscardDraftRequest]) (*connect.Response[emaildv1.DiscardDraftResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeSend)
	if err != nil {
		return nil, err
	}
	svc, err := c.Server.serviceForClaimsMailbox(ctx, claims, req.Msg.GetMailboxName())
	if err != nil {
		return nil, connectError(err)
	}
	actor, err := c.Server.verifyActorEnvelope(ctx, req.Header(), svc.MailboxID, "send", "discard_draft", strings.TrimSpace(req.Msg.GetDraftId()))
	if err != nil {
		return nil, connectUnauthorized(err)
	}
	draft, err := c.Server.discardDraftOp(ctx, claims, sendFromDraftParams{
		MailboxName: req.Msg.GetMailboxName(),
		DraftID:     req.Msg.GetDraftId(),
		Actor:       actor,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.DiscardDraftResponse{Draft: mapDraft(draft)}), nil
}

func (c *ConnectServer) SendFromDraft(ctx context.Context, req *connect.Request[emaildv1.SendFromDraftRequest]) (*connect.Response[emaildv1.SendFromDraftResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeSend)
	if err != nil {
		return nil, err
	}
	svc, err := c.Server.serviceForClaimsMailbox(ctx, claims, req.Msg.GetMailboxName())
	if err != nil {
		return nil, connectError(err)
	}
	draftID := strings.TrimSpace(req.Msg.GetDraftId())
	reviewedRevisionID := strings.TrimSpace(req.Msg.GetReviewedRevisionId())
	if draftID == "" || reviewedRevisionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("draft_id and reviewed_revision_id are required"))
	}
	actor, err := c.Server.verifyActorEnvelope(ctx, req.Header(), svc.MailboxID, "send", "send_from_draft", draftApprovalTarget(draftID, reviewedRevisionID))
	if err != nil {
		return nil, connectUnauthorized(err)
	}
	result, err := c.Server.sendFromDraftOp(ctx, claims, sendFromDraftParams{
		MailboxName:        req.Msg.GetMailboxName(),
		DraftID:            draftID,
		ReviewedRevisionID: reviewedRevisionID,
		Actor:              actor,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.SendFromDraftResponse{
		Status:     "queued",
		JobId:      result.Outbound.ID,
		Draft:      mapDraft(result.Draft),
		Idempotent: result.Idempotent,
	}), nil
}

func (c *ConnectServer) RunAgent(ctx context.Context, req *connect.Request[emaildv1.RunAgentRequest]) (*connect.Response[emaildv1.RunAgentResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeSend)
	if err != nil {
		return nil, err
	}
	svc, err := c.Server.serviceForClaimsMailbox(ctx, claims, req.Msg.GetMailboxName())
	if err != nil {
		return nil, connectError(err)
	}
	target := svc.MailboxID
	if req.Msg.MessageId != nil && strings.TrimSpace(req.Msg.GetMessageId()) != "" {
		target = strings.TrimSpace(req.Msg.GetMessageId())
	}
	actor, err := c.Server.verifyActorEnvelope(ctx, req.Header(), svc.MailboxID, "send", "run_agent", target)
	if err != nil {
		return nil, connectUnauthorized(err)
	}
	result, err := c.Server.runAgentOp(ctx, claims, runAgentParams{
		MailboxName:    svc.MailboxName,
		ConversationID: req.Msg.ConversationId,
		MessageID:      req.Msg.MessageId,
		Prompt:         req.Msg.GetPrompt(),
		Actor:          actor,
	})
	if err != nil {
		return nil, connectError(err)
	}
	resp := &emaildv1.RunAgentResponse{Conversation: mapAgentConversation(result.Conversation)}
	if result.Draft != nil {
		resp.Draft = mapDraft(*result.Draft)
	}
	return connect.NewResponse(resp), nil
}

func (c *ConnectServer) ListAgentConversations(ctx context.Context, req *connect.Request[emaildv1.ListAgentConversationsRequest]) (*connect.Response[emaildv1.ListAgentConversationsResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	items, err := c.Server.listAgentConversationsOp(ctx, claims, listAgentConversationsParams{
		MailboxName: req.Msg.GetMailboxName(), SourceMessageID: req.Msg.MessageId, Limit: int(req.Msg.GetLimit()),
	})
	if err != nil {
		return nil, connectError(err)
	}
	out := make([]*emaildv1.AgentConversation, 0, len(items))
	for _, item := range items {
		out = append(out, mapAgentConversation(item))
	}
	return connect.NewResponse(&emaildv1.ListAgentConversationsResponse{Conversations: out}), nil
}

func (c *ConnectServer) GetAgentConversation(ctx context.Context, req *connect.Request[emaildv1.GetAgentConversationRequest]) (*connect.Response[emaildv1.GetAgentConversationResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	item, err := c.Server.getAgentConversationOp(ctx, claims, draftIDParams{MailboxName: req.Msg.GetMailboxName(), DraftID: req.Msg.GetConversationId()})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.GetAgentConversationResponse{Conversation: mapAgentConversation(item)}), nil
}

func (c *ConnectServer) GetMailboxAgentContext(ctx context.Context, req *connect.Request[emaildv1.GetMailboxAgentContextRequest]) (*connect.Response[emaildv1.GetMailboxAgentContextResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	item, err := c.Server.getMailboxAgentContextOp(ctx, claims, req.Msg.GetMailboxName())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.GetMailboxAgentContextResponse{Context: mapMailboxAgentContext(item)}), nil
}

func (c *ConnectServer) GetMailboxAgentSettings(ctx context.Context, req *connect.Request[emaildv1.GetMailboxAgentSettingsRequest]) (*connect.Response[emaildv1.GetMailboxAgentSettingsResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	if claims.AliasID != nil || !claims.IsAdmin() {
		return nil, connectError(store.ErrUnauthorized)
	}
	svc, err := c.Server.serviceForClaimsMailbox(ctx, claims, req.Msg.GetMailboxName())
	if err != nil {
		return nil, connectError(err)
	}
	if _, err := c.Server.verifyActorEnvelope(ctx, req.Header(), svc.MailboxID, "admin", "read_mailbox_agent_settings", svc.MailboxID); err != nil {
		return nil, connectUnauthorized(err)
	}
	settings, err := svc.GetMailboxAgentSettings(ctx, claims)
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.GetMailboxAgentSettingsResponse{Settings: mapMailboxAgentSettings(settings)}), nil
}

func (c *ConnectServer) UpdateMailboxAgentSettings(ctx context.Context, req *connect.Request[emaildv1.UpdateMailboxAgentSettingsRequest]) (*connect.Response[emaildv1.UpdateMailboxAgentSettingsResponse], error) {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return nil, err
	}
	if claims.AliasID != nil {
		return nil, connectError(store.ErrUnauthorized)
	}
	svc, err := c.Server.serviceForClaimsMailbox(ctx, claims, req.Msg.GetMailboxName())
	if err != nil {
		return nil, connectError(err)
	}
	actor, err := c.Server.verifyActorEnvelope(ctx, req.Header(), svc.MailboxID, "admin", "update_mailbox_agent_settings", svc.MailboxID)
	if err != nil {
		return nil, connectUnauthorized(err)
	}
	var enabledTools []string
	if req.Msg.GetEnabledToolsPresent() || len(req.Msg.GetEnabledTools()) > 0 {
		enabledTools = append([]string{}, req.Msg.GetEnabledTools()...)
	}
	settings, err := c.Server.updateMailboxAgentSettingsOp(ctx, claims, req.Msg.GetMailboxName(), store.UpdateMailboxAgentSettingsInput{
		SystemPrompt:     req.Msg.GetSystemPrompt(),
		EnabledTools:     enabledTools,
		ModelOverride:    req.Msg.ModelOverride,
		AutoDraftEnabled: req.Msg.GetAutoDraftEnabled(),
		MaxToolSteps:     int(req.Msg.GetMaxToolSteps()),
		ToolTimeoutSec:   int(req.Msg.GetToolTimeoutSec()),
		Actor:            actor,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&emaildv1.UpdateMailboxAgentSettingsResponse{Settings: mapMailboxAgentSettings(settings)}), nil
}

func (c *ConnectServer) StreamMailboxEvents(ctx context.Context, req *connect.Request[emaildv1.StreamMailboxEventsRequest], stream *connect.ServerStream[emaildv1.MailboxEvent]) error {
	claims, err := c.Server.authenticateConnect(ctx, req.Header().Get("Authorization"), model.ScopeRead)
	if err != nil {
		return err
	}
	svc, err := c.Server.serviceForClaimsMailbox(ctx, claims, req.Msg.GetMailboxName())
	if err != nil {
		return connectError(err)
	}
	evCh, cancel := c.Server.Broker.Subscribe(svc.MailboxID, svc.MailboxName, claims)
	defer cancel()
	if err := stream.Send(mapMailboxEvent(modelMailboxEvent{
		MailboxID:   svc.MailboxID,
		MailboxName: svc.MailboxName,
		EventType:   "ready",
		OccurredAt:  time.Now().UTC(),
		Cursor:      fmt.Sprintf("%d", time.Now().UTC().UnixNano()),
	})); err != nil {
		return connectError(err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-evCh:
			if !ok {
				return nil
			}
			if err := stream.Send(mapMailboxEvent(modelMailboxEvent{
				MailboxID:   ev.MailboxID,
				MailboxName: ev.MailboxName,
				EventType:   ev.EventType,
				MessageID:   ev.MessageID,
				OccurredAt:  ev.OccurredAt,
				Cursor:      ev.Cursor,
				Subject:     ev.Subject,
			})); err != nil {
				return connectError(err)
			}
		}
	}
}

func (s *Server) authenticateConnect(ctx context.Context, authorization string, scope model.Scope) (model.TokenClaims, error) {
	authSvc := s.authService()
	if authSvc == nil {
		return model.TokenClaims{}, connect.NewError(connect.CodeUnavailable, errors.New("service not configured: onboard at /onboarding"))
	}
	claims, err := authSvc.Authenticate(ctx, authorization, s.acceptedMasterKeys())
	if err != nil {
		return model.TokenClaims{}, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if !claims.IsAdmin() && !claims.Has(scope) {
		return model.TokenClaims{}, connect.NewError(connect.CodePermissionDenied, errors.New("forbidden"))
	}
	return claims, nil
}

func connectError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, store.ErrUnauthorized):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrAgentRunConflict):
		return connect.NewError(connect.CodeAborted, err)
	default:
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
}

type modelMailboxEvent struct {
	MailboxID   string
	MailboxName string
	EventType   string
	MessageID   string
	OccurredAt  time.Time
	Cursor      string
	Subject     string
}

func mapFolders(folders []model.FolderSummary) []*emaildv1.Folder {
	out := make([]*emaildv1.Folder, 0, len(folders))
	for _, f := range folders {
		out = append(out, mapFolder(f))
	}
	return out
}

func mapFolder(f model.FolderSummary) *emaildv1.Folder {
	return &emaildv1.Folder{
		Name:        f.Name,
		DisplayName: f.DisplayName,
		Role:        f.Role,
		Total:       int32(f.Total),
		Unread:      int32(f.Unread),
	}
}

func mapMessageSummaries(items []model.ListItem) []*emaildv1.MessageSummary {
	out := make([]*emaildv1.MessageSummary, 0, len(items))
	for _, item := range items {
		out = append(out, mapMessageSummary(item))
	}
	return out
}

func mapMessageSummary(item model.ListItem) *emaildv1.MessageSummary {
	msg := &emaildv1.MessageSummary{
		MsgId:            item.MsgID,
		Folder:           item.Folder,
		ReceivedAt:       timestamppb.New(item.ReceivedAt),
		From:             mapFrom(item.From),
		Seen:             item.Seen,
		Digest:           mapDigest(item.Digest),
		AttachmentsCount: int32(item.AttachmentsCount),
		BodyState:        item.BodyState,
		Thread:           mapThread(item.Thread),
	}
	if item.Subject != nil {
		msg.Subject = item.Subject
	}
	return msg
}

func mapDrafts(items []model.Draft) []*emaildv1.Draft {
	out := make([]*emaildv1.Draft, 0, len(items))
	for _, item := range items {
		out = append(out, mapDraft(item))
	}
	return out
}

func mapDraft(item model.Draft) *emaildv1.Draft {
	revisions := make([]*emaildv1.DraftRevision, 0, len(item.Revisions))
	for _, rev := range item.Revisions {
		revisions = append(revisions, mapDraftRevision(rev))
	}
	out := &emaildv1.Draft{
		Id:               item.ID,
		MailboxId:        item.MailboxID,
		Status:           string(item.Status),
		LatestRevisionId: item.LatestRevisionID,
		LatestRevision:   mapDraftRevision(item.LatestRevision),
		Revisions:        revisions,
		CreatedAt:        timestamppb.New(item.CreatedAt),
		UpdatedAt:        timestamppb.New(item.UpdatedAt),
	}
	if item.ConversationID != nil {
		out.ConversationId = item.ConversationID
	}
	if item.AliasID != nil {
		out.AliasId = item.AliasID
	}
	if item.SourceMessageID != nil {
		out.SourceMessageId = item.SourceMessageID
	}
	if item.ThreadRFCMessageID != nil {
		out.ThreadRfcMessageId = item.ThreadRFCMessageID
	}
	if item.ActorUserID != nil {
		out.ActorUserId = item.ActorUserID
	}
	if item.LastRequestID != nil {
		out.LastRequestId = item.LastRequestID
	}
	if item.PermissionIntent != nil {
		out.PermissionIntent = item.PermissionIntent
	}
	if item.SendIdempotencyKey != nil {
		out.SendIdempotencyKey = item.SendIdempotencyKey
	}
	if item.SentRevisionID != nil {
		out.SentRevisionId = item.SentRevisionID
	}
	return out
}

func mapDraftRevision(item model.DraftRevision) *emaildv1.DraftRevision {
	out := &emaildv1.DraftRevision{
		Id:             item.ID,
		DraftId:        item.DraftID,
		RevisionNumber: int32(item.RevisionNumber),
		Provenance:     item.Provenance,
		Subject:        item.Subject,
		To:             item.To,
		Body:           item.Body,
		CreatedAt:      timestamppb.New(item.CreatedAt),
	}
	if item.InReplyTo != nil {
		out.InReplyTo = item.InReplyTo
	}
	if item.References != nil {
		out.References = item.References
	}
	if item.ActorUserID != nil {
		out.ActorUserId = item.ActorUserID
	}
	if item.RequestID != nil {
		out.RequestId = item.RequestID
	}
	if item.PermissionIntent != nil {
		out.PermissionIntent = item.PermissionIntent
	}
	return out
}

func mapAgentConversation(item model.AgentConversation) *emaildv1.AgentConversation {
	out := &emaildv1.AgentConversation{
		Id:        item.ID,
		MailboxId: item.MailboxID,
		Status:    string(item.Status),
		CreatedAt: timestamppb.New(item.CreatedAt),
		UpdatedAt: timestamppb.New(item.UpdatedAt),
	}
	if item.MessageID != nil {
		out.MessageId = item.MessageID
	}
	if item.AliasID != nil {
		out.AliasId = item.AliasID
	}
	if item.ThreadRFCMessageID != nil {
		out.ThreadRfcMessageId = item.ThreadRFCMessageID
	}
	if item.CreatedBy != nil {
		out.CreatedBy = item.CreatedBy
	}
	if item.LastDraftID != nil {
		out.LastDraftId = item.LastDraftID
	}
	for _, turn := range item.Messages {
		out.Messages = append(out.Messages, mapAgentConversationTurn(turn))
	}
	for _, call := range item.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, mapAgentToolCall(call))
	}
	return out
}

func mapAgentConversationTurn(item model.AgentConversationTurn) *emaildv1.AgentConversationTurn {
	return &emaildv1.AgentConversationTurn{
		Id:             item.ID,
		ConversationId: item.ConversationID,
		Role:           item.Role,
		Content:        item.Content,
		PayloadJson:    string(item.Payload),
		CreatedAt:      timestamppb.New(item.CreatedAt),
	}
}

func mapAgentToolCall(item model.AgentToolCall) *emaildv1.AgentToolCall {
	out := &emaildv1.AgentToolCall{
		Id:                item.ID,
		ConversationId:    item.ConversationID,
		ToolName:          item.ToolName,
		Status:            string(item.Status),
		ArgsSummaryJson:   string(item.ArgsSummary),
		ResultSummaryJson: string(item.ResultSummary),
		PolicyDecision:    item.PolicyDecision,
		StartedAt:         timestamppb.New(item.StartedAt),
	}
	if item.ErrorText != nil {
		out.ErrorText = item.ErrorText
	}
	if item.CompletedAt != nil {
		out.CompletedAt = timestamppb.New(*item.CompletedAt)
	}
	return out
}

func mapMailboxAgentSettings(item model.MailboxAgentSettings) *emaildv1.MailboxAgentSettings {
	out := &emaildv1.MailboxAgentSettings{
		MailboxId:        item.MailboxID,
		SystemPrompt:     item.SystemPrompt,
		EnabledTools:     item.EnabledTools,
		AutoDraftEnabled: item.AutoDraftEnabled,
		MaxToolSteps:     int32(item.MaxToolSteps),
		ToolTimeoutSec:   int32(item.ToolTimeoutSec),
		PolicyVersion:    int32(item.PolicyVersion),
	}
	if item.ModelOverride != nil {
		out.ModelOverride = item.ModelOverride
	}
	if item.UpdatedBy != nil {
		out.UpdatedBy = item.UpdatedBy
	}
	if item.LastRequestID != nil {
		out.LastRequestId = item.LastRequestID
	}
	if item.PermissionIntent != nil {
		out.PermissionIntent = item.PermissionIntent
	}
	if !item.UpdatedAt.IsZero() {
		out.UpdatedAt = timestamppb.New(item.UpdatedAt)
	}
	return out
}

func mapMailboxAgentContext(item model.MailboxAgentContext) *emaildv1.MailboxAgentContext {
	return &emaildv1.MailboxAgentContext{MailboxId: item.MailboxID}
}

func mapMessage(msg model.Message) *emaildv1.Message {
	out := &emaildv1.Message{
		MsgId:      msg.ID,
		ReceivedAt: timestamppb.New(msg.InternalDate),
		FromEmail:  msg.FromEmail,
		BodyState:  msg.BodyState,
		Thread:     mapThread(msg.Thread),
		From:       mapFrom(msg.From),
	}
	if msg.RFCMessageID != "" {
		out.RfcMessageId = &msg.RFCMessageID
	}
	if msg.SentDate != nil {
		out.SentDate = timestamppb.New(*msg.SentDate)
	}
	if msg.FromName != nil {
		out.FromName = msg.FromName
	}
	if msg.Subject != nil {
		out.Subject = msg.Subject
	}
	if msg.Snippet != nil {
		out.Snippet = msg.Snippet
	}
	if msg.InReplyTo != nil {
		out.InReplyTo = msg.InReplyTo
	}
	if msg.References != nil {
		out.References = msg.References
	}
	if msg.Digest != nil {
		out.Digest = mapDigest(*msg.Digest)
	}
	for _, attachment := range msg.Attachments {
		out.Attachments = append(out.Attachments, mapAttachment(attachment))
	}
	return out
}

func mapBody(body model.Body) *emaildv1.MessageBody {
	out := &emaildv1.MessageBody{MessageId: body.MessageID}
	if body.TextPlain != nil {
		out.TextPlain = body.TextPlain
	}
	if body.TextHTML != nil {
		out.TextHtml = body.TextHTML
	}
	if body.ExtractedText != nil {
		out.ExtractedText = body.ExtractedText
	}
	return out
}

func mapFrom(in model.From) *emaildv1.From {
	out := &emaildv1.From{Email: in.Email}
	if in.Name != nil {
		out.Name = in.Name
	}
	return out
}

func mapThread(in model.Thread) *emaildv1.Thread {
	out := &emaildv1.Thread{MessageId: in.MessageID}
	if in.InReplyTo != nil {
		out.InReplyTo = in.InReplyTo
	}
	if in.References != nil {
		out.References = in.References
	}
	return out
}

func mapAttachment(in model.Attachment) *emaildv1.Attachment {
	out := &emaildv1.Attachment{
		Id:        in.ID,
		MessageId: in.MessageID,
		Filename:  in.Filename,
		Mime:      in.MIME,
		SizeBytes: in.SizeBytes,
		CreatedAt: timestamppb.New(in.CreatedAt),
	}
	if in.Storage != nil {
		out.StoragePath = in.Storage
	}
	if in.CreatedAt.IsZero() {
		out.CreatedAt = nil
	}
	return out
}

func mapDigest(in model.Digest) *emaildv1.Digest {
	out := &emaildv1.Digest{
		MessageId:             in.MessageID,
		ModelName:             in.ModelName,
		SummaryShort:          in.SummaryShort,
		SummaryBullets:        in.SummaryBullets,
		Intent:                in.Intent,
		Priority:              in.Priority,
		RequiredAction:        in.RequiredAction,
		SuggestedNextStep:     in.SuggestedNextStep,
		ExtractedEntitiesJson: string(in.ExtractedEntities),
		Confidence:            in.Confidence,
	}
	if !in.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(in.CreatedAt)
	}
	return out
}

func mapMailboxEvent(in modelMailboxEvent) *emaildv1.MailboxEvent {
	out := &emaildv1.MailboxEvent{
		MailboxName: in.MailboxName,
		MailboxId:   in.MailboxID,
		EventType:   in.EventType,
		MessageId:   in.MessageID,
		OccurredAt:  timestamppb.New(in.OccurredAt),
		Cursor:      in.Cursor,
	}
	if strings.TrimSpace(in.Subject) != "" {
		out.Subject = &in.Subject
	}
	return out
}

func unmapSearchRequest(in *emaildv1.SearchRequest) model.SearchRequest {
	if in == nil {
		return model.SearchRequest{}
	}
	req := model.SearchRequest{
		Folder:     in.GetFolder(),
		Limit:      int(in.GetLimit()),
		Mode:       model.SearchMode(in.GetMode()),
		ParserMode: in.GetParserMode(),
		Filters:    unmapSearchFilters(in.GetFilters()),
		Intent:     unmapSearchIntent(in.GetIntent()),
	}
	return req
}

func unmapSearchFilters(in *emaildv1.SearchFilters) model.SearchFilters {
	if in == nil {
		return model.SearchFilters{}
	}
	return model.SearchFilters{
		Headers: unmapSearchHeaderFilters(in.GetHeaders()),
		Flags:   unmapSearchFlagFilters(in.GetFlags()),
		Date:    unmapSearchDateFilters(in.GetDate()),
		Size:    unmapSearchSizeFilters(in.GetSize()),
		Logic:   unmapSearchLogic(in.GetLogic()),
	}
}

func unmapSearchHeaderFilters(in *emaildv1.SearchHeaderFilters) model.SearchHeaderFilters {
	if in == nil {
		return model.SearchHeaderFilters{}
	}
	generic := map[string][]string{}
	for _, item := range in.GetGeneric() {
		if item == nil || strings.TrimSpace(item.GetName()) == "" {
			continue
		}
		generic[item.GetName()] = item.GetValues()
	}
	return model.SearchHeaderFilters{
		Subject:   in.GetSubject(),
		From:      in.GetFrom(),
		To:        in.GetTo(),
		MessageID: in.GetMessageId(),
		Generic:   generic,
	}
}

func unmapSearchFlagFilters(in *emaildv1.SearchFlagFilters) model.SearchFlagFilters {
	if in == nil {
		return model.SearchFlagFilters{}
	}
	return model.SearchFlagFilters{
		With:    in.GetWith(),
		Without: in.GetWithout(),
	}
}

func unmapSearchDateFilters(in *emaildv1.SearchDateFilters) model.SearchDateFilters {
	if in == nil {
		return model.SearchDateFilters{}
	}
	return model.SearchDateFilters{
		InternalSince:  protoTimePtr(in.InternalSince),
		InternalBefore: protoTimePtr(in.InternalBefore),
		SentSince:      protoTimePtr(in.SentSince),
		SentBefore:     protoTimePtr(in.SentBefore),
		Field:          in.GetField(),
		Range:          unmapSearchDateRange(in.GetRange()),
		Temporal:       unmapSearchTemporalFilter(in.GetTemporal()),
	}
}

func unmapSearchDateRange(in *emaildv1.SearchDateRange) *model.SearchDateRange {
	if in == nil {
		return nil
	}
	return &model.SearchDateRange{
		Start:        unmapSearchTemporalPoint(in.GetStart()),
		End:          unmapSearchTemporalPoint(in.GetEnd()),
		EndInclusive: in.GetEndInclusive(),
	}
}

func unmapSearchTemporalPoint(in *emaildv1.SearchTemporalPoint) *model.SearchTemporalPoint {
	if in == nil {
		return nil
	}
	return &model.SearchTemporalPoint{
		Type:      in.GetType(),
		Value:     in.GetValue(),
		Timestamp: in.GetTimestamp(),
		Date:      in.GetDate(),
		Direction: in.GetDirection(),
		Amount:    int(in.GetAmount()),
		Unit:      in.GetUnit(),
		Weekday:   in.GetWeekday(),
		Period:    in.GetPeriod(),
		Boundary:  in.GetBoundary(),
		Snap:      in.GetSnap(),
		Timezone:  in.GetTimezone(),
	}
}

func unmapSearchTemporalFilter(in *emaildv1.SearchTemporalFilter) *model.SearchTemporalFilter {
	if in == nil {
		return nil
	}
	return &model.SearchTemporalFilter{
		Kind:     in.GetKind(),
		Value:    in.GetValue(),
		Amount:   int(in.GetAmount()),
		Unit:     in.GetUnit(),
		Field:    in.GetField(),
		Timezone: in.GetTimezone(),
		Date:     in.GetDate(),
	}
}

func unmapSearchSizeFilters(in *emaildv1.SearchSizeFilters) model.SearchSizeFilters {
	if in == nil {
		return model.SearchSizeFilters{}
	}
	out := model.SearchSizeFilters{}
	if in.Larger != nil {
		v := in.GetLarger()
		out.Larger = &v
	}
	if in.Smaller != nil {
		v := in.GetSmaller()
		out.Smaller = &v
	}
	return out
}

func unmapSearchLogic(in *emaildv1.SearchLogic) model.SearchLogic {
	if in == nil {
		return model.SearchLogic{}
	}
	out := model.SearchLogic{}
	for _, item := range in.GetNot() {
		out.Not = append(out.Not, unmapSearchSubquery(item))
	}
	for _, item := range in.GetOr() {
		if item == nil {
			continue
		}
		clause := model.SearchOrClause{}
		for _, sub := range item.GetAny() {
			clause.Any = append(clause.Any, unmapSearchSubquery(sub))
		}
		out.Or = append(out.Or, clause)
	}
	return out
}

func unmapSearchSubquery(in *emaildv1.SearchSubquery) model.SearchSubquery {
	if in == nil {
		return model.SearchSubquery{}
	}
	return model.SearchSubquery{
		Filters: unmapSearchFilters(in.GetFilters()),
		Intent:  unmapSearchIntent(in.GetIntent()),
	}
}

func unmapSearchIntent(in *emaildv1.SearchIntent) model.SearchIntent {
	if in == nil {
		return model.SearchIntent{}
	}
	return model.SearchIntent{
		Text:      in.GetText(),
		BodyTerms: in.GetBodyTerms(),
		HeaderTerms: model.SearchHeaderIntent{
			Subject: in.GetHeaderTerms().GetSubject(),
			From:    in.GetHeaderTerms().GetFrom(),
			To:      in.GetHeaderTerms().GetTo(),
		},
	}
}

func protoTimePtr(in *timestamppb.Timestamp) *time.Time {
	if in == nil {
		return nil
	}
	t := in.AsTime()
	return &t
}
