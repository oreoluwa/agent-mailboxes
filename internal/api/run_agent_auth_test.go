package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/auth"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/service"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type runAgentAuthStore struct {
	store.Store
	claims    model.TokenClaims
	tokenHash string
}

func (s runAgentAuthStore) AuthenticateToken(ctx context.Context, tokenHash string) (model.TokenClaims, error) {
	if tokenHash == s.tokenHash {
		return s.claims, nil
	}
	return model.TokenClaims{}, store.ErrUnauthorized
}

func TestRunAgentRequiresVerifiedActorEnvelope(t *testing.T) {
	rawToken := "tok-run-agent"
	masterKey := "master-key"
	st := runAgentAuthStore{
		claims: model.TokenClaims{
			TokenID: "tok-1",
			Scopes:  map[model.Scope]bool{model.ScopeAdmin: true},
		},
		tokenHash: auth.HashToken(masterKey, rawToken),
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
	body, err := json.Marshal(map[string]any{
		"message_id":    "msg-1",
		"prompt":        "Draft a reply",
		"actor_user_id": "spoofed-user",
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("missing envelope", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/agent/run?mailbox=primary", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rawToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("spoofed envelope", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/agent/run?mailbox=primary", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rawToken)
		now := time.Now().UTC().Unix()
		env := model.ActorEnvelope{
			UserID:     "real-user",
			MailboxID:  "mb-1",
			RequestID:  "req-1",
			Permission: "send",
			Timestamp:  now,
			Nonce:      "nonce-1",
			Action:     "run_agent",
			TargetID:   "msg-1",
		}
		req.Header.Set(headerActorUserID, env.UserID)
		req.Header.Set(headerActorMailboxID, env.MailboxID)
		req.Header.Set(headerActorRequestID, env.RequestID)
		req.Header.Set(headerActorPermission, env.Permission)
		req.Header.Set(headerActorTimestamp, strconv.FormatInt(env.Timestamp, 10))
		req.Header.Set(headerActorNonce, env.Nonce)
		req.Header.Set(headerActorAction, env.Action)
		req.Header.Set(headerActorTargetID, env.TargetID)
		req.Header.Set(headerActorSignature, base64.StdEncoding.EncodeToString(computeActorEnvelopeMAC("wrong-secret", env)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
		}
	})
}
