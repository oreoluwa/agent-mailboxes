package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

func signedActorHeaders(secret string, env model.ActorEnvelope) http.Header {
	headers := make(http.Header)
	headers.Set(headerActorUserID, env.UserID)
	headers.Set(headerActorMailboxID, env.MailboxID)
	headers.Set(headerActorRequestID, env.RequestID)
	headers.Set(headerActorPermission, env.Permission)
	headers.Set(headerActorTimestamp, strconv.FormatInt(env.Timestamp, 10))
	headers.Set(headerActorNonce, env.Nonce)
	headers.Set(headerActorAction, env.Action)
	headers.Set(headerActorTargetID, env.TargetID)
	headers.Set(headerActorSignature, base64.StdEncoding.EncodeToString(computeActorEnvelopeMAC(secret, env)))
	return headers
}

func TestVerifyActorEnvelopeRejectsWrongMailbox(t *testing.T) {
	srv := &Server{
		Store:                     replayStubStore{},
		ActorEnvelopeSecret:       "shared-secret",
		ActorEnvelopeReplayWindow: 5 * time.Minute,
	}
	env := model.ActorEnvelope{
		UserID:     "user-1",
		MailboxID:  "mb-other",
		RequestID:  "req-1",
		Permission: "send",
		Timestamp:  time.Now().UTC().Unix(),
		Nonce:      "nonce-1",
		Action:     "create_draft",
		TargetID:   "msg-1",
	}

	_, err := srv.verifyActorEnvelope(context.Background(), signedActorHeaders("shared-secret", env), "mb-1", "send", "create_draft", "msg-1")
	if err != store.ErrUnauthorized {
		t.Fatalf("expected unauthorized for mailbox mismatch, got %v", err)
	}
}

func TestVerifyActorEnvelopeRejectsWrongTarget(t *testing.T) {
	srv := &Server{
		Store:                     replayStubStore{},
		ActorEnvelopeSecret:       "shared-secret",
		ActorEnvelopeReplayWindow: 5 * time.Minute,
	}
	env := model.ActorEnvelope{
		UserID:     "user-1",
		MailboxID:  "mb-1",
		RequestID:  "req-1",
		Permission: "send",
		Timestamp:  time.Now().UTC().Unix(),
		Nonce:      "nonce-1",
		Action:     "send_from_draft",
		TargetID:   "draft-other",
	}

	_, err := srv.verifyActorEnvelope(context.Background(), signedActorHeaders("shared-secret", env), "mb-1", "send", "send_from_draft", "draft-1")
	if err != store.ErrUnauthorized {
		t.Fatalf("expected unauthorized for target mismatch, got %v", err)
	}
}
