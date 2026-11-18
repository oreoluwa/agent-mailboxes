package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	connect "connectrpc.com/connect"
	"github.com/oreoluwa/agent-mailboxes/internal/auth"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/service"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
	emaildv1 "github.com/oreoluwa/agent-mailboxes/proto/agentemails/emaild/v1"
)

type listConversationsFilterStore struct {
	store.Store
	tokenHash string
	claims    model.TokenClaims
	options   store.AgentConversationListOptions
}

func (s *listConversationsFilterStore) AuthenticateToken(_ context.Context, tokenHash string) (model.TokenClaims, error) {
	if tokenHash != s.tokenHash {
		return model.TokenClaims{}, store.ErrUnauthorized
	}
	return s.claims, nil
}

func (s *listConversationsFilterStore) ReadMessage(_ context.Context, _ model.TokenClaims, messageID string) (model.Message, error) {
	if messageID != "message-1" {
		return model.Message{}, store.ErrNotFound
	}
	return model.Message{ID: messageID, RFCMessageID: "<thread-1@example.test>"}, nil
}

func (s *listConversationsFilterStore) ListAgentConversations(_ context.Context, _ model.TokenClaims, mailboxID string, options store.AgentConversationListOptions) ([]model.AgentConversation, error) {
	if mailboxID != "mb-1" {
		return nil, store.ErrNotFound
	}
	s.options = options
	return []model.AgentConversation{{ID: "conversation-1", MailboxID: mailboxID}}, nil
}

func newListConversationsFilterServer(t *testing.T) (*Server, *listConversationsFilterStore, string) {
	t.Helper()
	const (
		token     = "conversation-list-token"
		masterKey = "master-key"
	)
	st := &listConversationsFilterStore{
		tokenHash: auth.HashToken(masterKey, token),
		claims:    model.TokenClaims{TokenID: "token-1", Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeAdmin: true}},
	}
	svc := &service.Service{Store: st, MailboxID: "mb-1", MailboxName: "primary"}
	server := &Server{
		Svc: svc, ServicesByName: map[string]*service.Service{"primary": svc},
		ServicesByMailboxID: map[string]*service.Service{"mb-1": svc}, Store: st,
		MasterKey: masterKey, MasterKeys: []string{masterKey},
	}
	return server, st, token
}

func TestRESTListAgentConversationsPassesFiltersToStore(t *testing.T) {
	server, st, token := newListConversationsFilterServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/agent/conversations?mailbox=primary&message_id=message-1&limit=17", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	assertConversationListOptions(t, st.options, 17)
}

func TestConnectListAgentConversationsPassesFiltersToStore(t *testing.T) {
	server, st, token := newListConversationsFilterServer(t)
	messageID := "message-1"
	req := connect.NewRequest(&emaildv1.ListAgentConversationsRequest{MailboxName: "primary", MessageId: &messageID, Limit: 23})
	req.Header().Set("Authorization", "Bearer "+token)
	response, err := server.ConnectHandler().ListAgentConversations(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Msg.GetConversations()) != 1 {
		t.Fatalf("conversations = %+v", response.Msg.GetConversations())
	}
	assertConversationListOptions(t, st.options, 23)
}

func assertConversationListOptions(t *testing.T, options store.AgentConversationListOptions, limit int) {
	t.Helper()
	if options.SourceMessageID == nil || *options.SourceMessageID != "message-1" {
		t.Fatalf("source message option = %+v", options.SourceMessageID)
	}
	if options.ThreadRFCMessageID == nil || *options.ThreadRFCMessageID != "<thread-1@example.test>" {
		t.Fatalf("thread option = %+v", options.ThreadRFCMessageID)
	}
	if options.Limit != limit {
		t.Fatalf("limit = %d, want %d", options.Limit, limit)
	}
}
