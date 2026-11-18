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

type listDraftsFilterStore struct {
	store.Store
	tokenHash string
	claims    model.TokenClaims
	options   store.DraftListOptions
}

func (s *listDraftsFilterStore) AuthenticateToken(_ context.Context, tokenHash string) (model.TokenClaims, error) {
	if tokenHash != s.tokenHash {
		return model.TokenClaims{}, store.ErrUnauthorized
	}
	return s.claims, nil
}

func (s *listDraftsFilterStore) ListDrafts(_ context.Context, _ model.TokenClaims, mailboxID string, options store.DraftListOptions) ([]model.Draft, error) {
	if mailboxID != "mb-1" {
		return nil, store.ErrNotFound
	}
	s.options = options
	return []model.Draft{{ID: "draft-1", MailboxID: mailboxID, Status: model.DraftStatusPendingReview}}, nil
}

func newListDraftsFilterServer(t *testing.T) (*Server, *listDraftsFilterStore, string) {
	t.Helper()
	const (
		token     = "draft-list-token"
		masterKey = "master-key"
	)
	st := &listDraftsFilterStore{
		tokenHash: auth.HashToken(masterKey, token),
		claims: model.TokenClaims{
			TokenID: "token-1",
			Scopes:  map[model.Scope]bool{model.ScopeRead: true, model.ScopeAdmin: true},
		},
	}
	svc := &service.Service{Store: st, MailboxID: "mb-1", MailboxName: "primary"}
	server := &Server{
		Svc:                 svc,
		ServicesByName:      map[string]*service.Service{"primary": svc},
		ServicesByMailboxID: map[string]*service.Service{"mb-1": svc},
		Store:               st,
		MasterKey:           masterKey,
		MasterKeys:          []string{masterKey},
	}
	return server, st, token
}

func TestRESTListDraftsPassesFiltersToStore(t *testing.T) {
	server, st, token := newListDraftsFilterServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/drafts?mailbox=primary&message_id=message-1&status=pending_review&limit=17", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if st.options.SourceMessageID == nil || *st.options.SourceMessageID != "message-1" || st.options.Status != model.DraftStatusPendingReview || st.options.Limit != 17 {
		t.Fatalf("options = %+v", st.options)
	}
}

func TestConnectListDraftsPassesFiltersToStore(t *testing.T) {
	server, st, token := newListDraftsFilterServer(t)
	sourceMessageID := "message-2"
	req := connect.NewRequest(&emaildv1.ListDraftsRequest{
		MailboxName:     "primary",
		SourceMessageId: &sourceMessageID,
		Status:          string(model.DraftStatusFailed),
		Limit:           23,
	})
	req.Header().Set("Authorization", "Bearer "+token)
	response, err := server.ConnectHandler().ListDrafts(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Msg.GetDrafts()) != 1 {
		t.Fatalf("drafts = %+v", response.Msg.GetDrafts())
	}
	if st.options.SourceMessageID == nil || *st.options.SourceMessageID != sourceMessageID || st.options.Status != model.DraftStatusFailed || st.options.Limit != 23 {
		t.Fatalf("options = %+v", st.options)
	}
}
