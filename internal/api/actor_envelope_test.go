package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type replayStubStore struct {
	store.Store
	recordFn func(ctx context.Context, mailboxID, requestID, nonce, action, targetID string, expiresAt time.Time) error
}

func (s replayStubStore) RecordActorReplay(ctx context.Context, mailboxID, requestID, nonce, action, targetID string, expiresAt time.Time) error {
	if s.recordFn != nil {
		return s.recordFn(ctx, mailboxID, requestID, nonce, action, targetID, expiresAt)
	}
	return nil
}

func TestActorEnvelopeCanonicalPayloadOrder(t *testing.T) {
	got := actorEnvelopeCanonicalPayload(model.ActorEnvelope{
		UserID:     "user-1",
		MailboxID:  "mb-1",
		RequestID:  "req-1",
		Permission: "send",
		Timestamp:  1700000000,
		Nonce:      "nonce-1",
		Action:     "create_draft",
		TargetID:   "msg-1",
	})
	want := "v1\nuser-1\nmb-1\nreq-1\nsend\n1700000000\nnonce-1\ncreate_draft\nmsg-1"
	if got != want {
		t.Fatalf("canonical payload mismatch\nwant: %q\ngot:  %q", want, got)
	}
}

func TestVerifyActorEnvelopeAcceptsValidHeaderSet(t *testing.T) {
	srv := &Server{
		Store:                     replayStubStore{},
		ActorEnvelopeSecret:       "shared-secret",
		ActorEnvelopeReplayWindow: 5 * time.Minute,
	}
	headers := make(http.Header)
	now := time.Now().UTC().Unix()
	env := model.ActorEnvelope{
		UserID:     "user-1",
		MailboxID:  "mb-1",
		RequestID:  "req-1",
		Permission: "send",
		Timestamp:  now,
		Nonce:      "nonce-1",
		Action:     "create_draft",
		TargetID:   "msg-1",
	}
	headers.Set(headerActorUserID, env.UserID)
	headers.Set(headerActorMailboxID, env.MailboxID)
	headers.Set(headerActorRequestID, env.RequestID)
	headers.Set(headerActorPermission, env.Permission)
	headers.Set(headerActorTimestamp, strconv.FormatInt(now, 10))
	headers.Set(headerActorNonce, env.Nonce)
	headers.Set(headerActorAction, env.Action)
	headers.Set(headerActorTargetID, env.TargetID)
	headers.Set(headerActorSignature, base64.StdEncoding.EncodeToString(computeActorEnvelopeMAC("shared-secret", env)))
	actor, err := srv.verifyActorEnvelope(context.Background(), headers, "mb-1", "send", "create_draft", "msg-1")
	if err != nil {
		t.Fatalf("verify actor envelope: %v", err)
	}
	if actor.UserID != "user-1" || actor.PermissionIntent != "send" {
		t.Fatalf("unexpected actor: %+v", actor)
	}
}

func TestVerifyActorEnvelopeRejectsReplay(t *testing.T) {
	srv := &Server{
		Store: replayStubStore{
			recordFn: func(ctx context.Context, mailboxID, requestID, nonce, action, targetID string, expiresAt time.Time) error {
				return store.ErrUnauthorized
			},
		},
		ActorEnvelopeSecret:       "shared-secret",
		ActorEnvelopeReplayWindow: 5 * time.Minute,
	}
	headers := make(http.Header)
	timestamp := time.Now().UTC().Unix()
	env := model.ActorEnvelope{
		UserID:     "user-1",
		MailboxID:  "mb-1",
		RequestID:  "req-1",
		Permission: "send",
		Timestamp:  timestamp,
		Nonce:      "nonce-1",
		Action:     "send_from_draft",
		TargetID:   "draft-1",
	}
	headers.Set(headerActorUserID, env.UserID)
	headers.Set(headerActorMailboxID, env.MailboxID)
	headers.Set(headerActorRequestID, env.RequestID)
	headers.Set(headerActorPermission, env.Permission)
	headers.Set(headerActorTimestamp, strconv.FormatInt(timestamp, 10))
	headers.Set(headerActorNonce, env.Nonce)
	headers.Set(headerActorAction, env.Action)
	headers.Set(headerActorTargetID, env.TargetID)
	headers.Set(headerActorSignature, base64.StdEncoding.EncodeToString(computeActorEnvelopeMAC("shared-secret", env)))
	_, err := srv.verifyActorEnvelope(context.Background(), headers, "mb-1", "send", "send_from_draft", "draft-1")
	if !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("expected replay rejection, got %v", err)
	}
}

func TestVerifyActorEnvelopeRejectsDifferentReviewedRevision(t *testing.T) {
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
		TargetID:   draftApprovalTarget("draft-1", "revision-1"),
	}
	_, err := srv.verifyActorEnvelope(
		context.Background(),
		signedActorHeaders("shared-secret", env),
		"mb-1",
		"send",
		"send_from_draft",
		draftApprovalTarget("draft-1", "revision-2"),
	)
	if !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("expected reviewed revision mismatch rejection, got %v", err)
	}
}

func TestVerifyActorEnvelopeRejectsStaleTimestamp(t *testing.T) {
	srv := &Server{
		Store:                     replayStubStore{},
		ActorEnvelopeSecret:       "shared-secret",
		ActorEnvelopeReplayWindow: 5 * time.Minute,
	}
	headers := make(http.Header)
	timestamp := time.Now().UTC().Add(-10 * time.Minute).Unix()
	env := model.ActorEnvelope{
		UserID:     "user-1",
		MailboxID:  "mb-1",
		RequestID:  "req-1",
		Permission: "send",
		Timestamp:  timestamp,
		Nonce:      "nonce-1",
		Action:     "revise_draft",
		TargetID:   "draft-1",
	}
	headers.Set(headerActorUserID, env.UserID)
	headers.Set(headerActorMailboxID, env.MailboxID)
	headers.Set(headerActorRequestID, env.RequestID)
	headers.Set(headerActorPermission, env.Permission)
	headers.Set(headerActorTimestamp, strconv.FormatInt(timestamp, 10))
	headers.Set(headerActorNonce, env.Nonce)
	headers.Set(headerActorAction, env.Action)
	headers.Set(headerActorTargetID, env.TargetID)
	headers.Set(headerActorSignature, base64.StdEncoding.EncodeToString(computeActorEnvelopeMAC("shared-secret", env)))
	_, err := srv.verifyActorEnvelope(context.Background(), headers, "mb-1", "send", "revise_draft", "draft-1")
	if !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("expected stale timestamp rejection, got %v", err)
	}
}

func TestFutureActorTimestampRetainsReplayUntilEnvelopeExpires(t *testing.T) {
	window := 5 * time.Minute
	stamp := time.Now().Add(4 * time.Minute).Unix()
	var expiry time.Time
	srv := &Server{Store: replayStubStore{recordFn: func(_ context.Context, _, _, _, _, _ string, expires time.Time) error { expiry = expires; return nil }}, ActorEnvelopeSecret: "test-secret", ActorEnvelopeReplayWindow: window}
	envelope := model.ActorEnvelope{UserID: "user", MailboxID: "mailbox", RequestID: "request", Permission: "send", Timestamp: stamp, Nonce: "nonce", Action: "create_draft", TargetID: "target"}
	if _, err := srv.verifyActorEnvelope(context.Background(), signedActorHeaders("test-secret", envelope), "mailbox", "send", "create_draft", "target"); err != nil {
		t.Fatal(err)
	}
	if want := time.Unix(stamp, 0).Add(window); !expiry.Equal(want) {
		t.Fatalf("replay expires at %v, want envelope validity deadline %v", expiry, want)
	}
}
