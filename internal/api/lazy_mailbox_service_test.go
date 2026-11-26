package api

import (
	"context"
	"errors"
	"github.com/oreoluwa/agent-mailboxes/internal/auth"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/service"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type lazyMailboxStore struct {
	store.Store
	mailboxID string
}

func (s lazyMailboxStore) GetMailboxIDByAliasID(context.Context, string) (string, error) {
	return s.mailboxID, nil
}

func TestServiceForClaimsLazilyRegistersMailboxWithoutWorkers(t *testing.T) {
	loads := 0
	server := &Server{
		Store:               lazyMailboxStore{mailboxID: "mailbox-1"},
		ServicesByName:      map[string]*service.Service{},
		ServicesByMailboxID: map[string]*service.Service{},
		LoadMailboxService: func(_ context.Context, mailboxID, mailboxName string) (*service.Service, error) {
			loads++
			if mailboxID != "mailbox-1" || mailboxName != "" {
				t.Fatalf("loader selectors = %q, %q", mailboxID, mailboxName)
			}
			return &service.Service{MailboxID: "mailbox-1", MailboxName: "primary"}, nil
		},
	}
	aliasID := "alias-1"
	claims := model.TokenClaims{AliasID: &aliasID}
	req := httptest.NewRequest("GET", "/v1/inbox/list", nil)
	first, err := server.serviceForClaims(req.Context(), claims, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.serviceForClaims(req.Context(), claims, req)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || loads != 1 {
		t.Fatalf("service cache mismatch loads=%d first=%p second=%p", loads, first, second)
	}
}

func TestServiceForMailboxContextLazilyLoadsByName(t *testing.T) {
	server := &Server{
		ServicesByName:      map[string]*service.Service{},
		ServicesByMailboxID: map[string]*service.Service{},
		LoadMailboxService: func(_ context.Context, mailboxID, mailboxName string) (*service.Service, error) {
			if mailboxID != "" || mailboxName != "primary" {
				t.Fatalf("loader selectors = %q, %q", mailboxID, mailboxName)
			}
			return &service.Service{MailboxID: "mailbox-1", MailboxName: "primary"}, nil
		},
	}
	svc, err := server.serviceForMailboxContext(context.Background(), "primary")
	if err != nil || svc.MailboxID != "mailbox-1" {
		t.Fatalf("service=%+v err=%v", svc, err)
	}
}

func TestLazyMailboxLoaderRejectsMismatchedIdentity(t *testing.T) {
	server := &Server{
		ServicesByName:      map[string]*service.Service{},
		ServicesByMailboxID: map[string]*service.Service{},
		LoadMailboxService: func(context.Context, string, string) (*service.Service, error) {
			return &service.Service{MailboxID: "other", MailboxName: "other"}, nil
		},
	}
	if _, err := server.loadMailboxService(context.Background(), "mailbox-1", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error=%v, want not found", err)
	}
}

type lazyAuthStore struct {
	lazyMailboxStore
	tokenHash string
}

func (s lazyAuthStore) AuthenticateToken(_ context.Context, hash string) (model.TokenClaims, error) {
	if hash != s.tokenHash {
		return model.TokenClaims{}, store.ErrUnauthorized
	}
	alias := "alias-1"
	return model.TokenClaims{AliasID: &alias, Scopes: map[model.Scope]bool{model.ScopeRead: true}}, nil
}
func TestEmptyAPIInstanceAuthenticatesThenLoadsNewMailbox(t *testing.T) {
	loaded := 0
	s := &Server{MasterKey: "test-master", Store: lazyAuthStore{lazyMailboxStore: lazyMailboxStore{mailboxID: "mailbox-1"}, tokenHash: auth.HashToken("test-master", "test-token")}, LoadMailboxService: func(_ context.Context, id, name string) (*service.Service, error) {
		loaded++
		return &service.Service{MailboxID: id, MailboxName: "primary"}, nil
	}}
	handler := s.withAuth(func(w http.ResponseWriter, r *http.Request, c model.TokenClaims) {
		if _, err := s.serviceForClaims(r.Context(), c, r); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}, model.ScopeRead)
	for _, tc := range []struct {
		token  string
		status int
	}{{"", 401}, {"wrong-token", 401}, {"test-token", 204}} {
		req := httptest.NewRequest("GET", "/v1/inbox/list", nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		res := httptest.NewRecorder()
		handler(res, req)
		if res.Code != tc.status {
			t.Fatalf("status=%d want=%d", res.Code, tc.status)
		}
	}
	if loaded != 1 {
		t.Fatalf("loads=%d", loaded)
	}
}
