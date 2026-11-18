package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/auth"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/service"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type mailboxAgentAuthStore struct {
	replayStubStore
	claims    model.TokenClaims
	tokenHash string
	settings  model.MailboxAgentSettings
}

func (s mailboxAgentAuthStore) AuthenticateToken(ctx context.Context, tokenHash string) (model.TokenClaims, error) {
	if tokenHash == s.tokenHash {
		return s.claims, nil
	}
	return model.TokenClaims{}, store.ErrUnauthorized
}

func (s mailboxAgentAuthStore) GetMailboxAgentSettings(ctx context.Context, mailboxID string) (model.MailboxAgentSettings, error) {
	if mailboxID != s.settings.MailboxID {
		return model.MailboxAgentSettings{}, store.ErrNotFound
	}
	return s.settings, nil
}

func TestMailboxAgentContextReturnsCoreMailboxID(t *testing.T) {
	rawToken := "tok-agent-context"
	masterKey := "master-key"
	st := mailboxAgentAuthStore{
		claims: model.TokenClaims{
			TokenID: "tok-1",
			Scopes:  map[model.Scope]bool{model.ScopeRead: true},
		},
		tokenHash: auth.HashToken(masterKey, rawToken),
		settings:  model.MailboxAgentSettings{MailboxID: "mb-1"},
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

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/context?mailbox=primary", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "{\"mailbox_id\":\"mb-1\"}\n" {
		t.Fatalf("unexpected body: %s", got)
	}
}

func TestMailboxAgentSettingsGetRequiresVerifiedAdminEnvelope(t *testing.T) {
	rawToken := "tok-agent-settings"
	masterKey := "master-key"
	st := mailboxAgentAuthStore{
		claims: model.TokenClaims{
			TokenID: "tok-1",
			Scopes:  map[model.Scope]bool{model.ScopeRead: true, model.ScopeAdmin: true},
		},
		tokenHash: auth.HashToken(masterKey, rawToken),
		settings: model.MailboxAgentSettings{
			MailboxID:        "mb-1",
			SystemPrompt:     "prompt",
			EnabledTools:     []string{"search_mailbox"},
			AutoDraftEnabled: true,
			MaxToolSteps:     6,
			ToolTimeoutSec:   30,
			PolicyVersion:    1,
		},
	}
	svc := &service.Service{Store: st, MailboxID: "mb-1", MailboxName: "primary"}
	server := &Server{
		Svc:                       svc,
		ServicesByName:            map[string]*service.Service{"primary": svc},
		ServicesByMailboxID:       map[string]*service.Service{"mb-1": svc},
		Store:                     st,
		MasterKey:                 masterKey,
		MasterKeys:                []string{masterKey},
		ActorEnvelopeSecret:       "shared-secret",
		ActorEnvelopeReplayWindow: 5 * time.Minute,
	}
	handler := server.Handler()

	t.Run("missing envelope", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/agent/settings?mailbox=primary", nil)
		req.Header.Set("Authorization", "Bearer "+rawToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("valid envelope", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/agent/settings?mailbox=primary", nil)
		req.Header.Set("Authorization", "Bearer "+rawToken)
		env := model.ActorEnvelope{
			UserID:     "user-1",
			MailboxID:  "mb-1",
			RequestID:  "req-1",
			Permission: "admin",
			Timestamp:  time.Now().UTC().Unix(),
			Nonce:      "nonce-1",
			Action:     "read_mailbox_agent_settings",
			TargetID:   "mb-1",
		}
		for key, values := range signedActorHeaders("shared-secret", env) {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
		}
	})
}

func TestMailboxAgentSettingsUpdateRejectsAliasScopedAdmin(t *testing.T) {
	rawToken := "tok-alias-admin"
	masterKey := "master-key"
	aliasID := "alias-1"
	st := mailboxAgentAuthStore{
		claims: model.TokenClaims{
			TokenID: "tok-1",
			AliasID: &aliasID,
			Scopes:  map[model.Scope]bool{model.ScopeRead: true, model.ScopeAdmin: true},
		},
		tokenHash: auth.HashToken(masterKey, rawToken),
		settings:  model.MailboxAgentSettings{MailboxID: "mb-1"},
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
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/settings?mailbox=primary", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/agent/settings?mailbox=primary", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected alias-scoped read rejection, got %d body=%s", rec.Code, rec.Body.String())
	}
}
