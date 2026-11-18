package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	connect "connectrpc.com/connect"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

const (
	headerActorUserID     = "X-Agent-Actor-User-ID"
	headerActorMailboxID  = "X-Agent-Actor-Mailbox-ID"
	headerActorRequestID  = "X-Agent-Actor-Request-ID"
	headerActorPermission = "X-Agent-Actor-Permission"
	headerActorTimestamp  = "X-Agent-Actor-Timestamp"
	headerActorNonce      = "X-Agent-Actor-Nonce"
	headerActorAction     = "X-Agent-Actor-Action"
	headerActorTargetID   = "X-Agent-Actor-Target-ID"
	headerActorSignature  = "X-Agent-Actor-Signature"
)

type headerReader interface {
	Get(string) string
}

func (s *Server) verifyActorEnvelope(ctx context.Context, headers headerReader, mailboxID, expectedPermission, expectedAction, expectedTarget string) (*model.VerifiedActor, error) {
	secret := strings.TrimSpace(s.ActorEnvelopeSecret)
	if secret == "" {
		return nil, fmt.Errorf("actor envelope secret is not configured")
	}
	env := model.ActorEnvelope{
		UserID:          strings.TrimSpace(headers.Get(headerActorUserID)),
		MailboxID:       strings.TrimSpace(headers.Get(headerActorMailboxID)),
		RequestID:       strings.TrimSpace(headers.Get(headerActorRequestID)),
		Permission:      strings.TrimSpace(headers.Get(headerActorPermission)),
		Nonce:           strings.TrimSpace(headers.Get(headerActorNonce)),
		Action:          strings.TrimSpace(headers.Get(headerActorAction)),
		TargetID:        strings.TrimSpace(headers.Get(headerActorTargetID)),
		SignatureBase64: strings.TrimSpace(headers.Get(headerActorSignature)),
	}
	if env.UserID == "" || env.MailboxID == "" || env.RequestID == "" || env.Permission == "" || env.Nonce == "" || env.Action == "" || env.TargetID == "" || env.SignatureBase64 == "" {
		return nil, store.ErrUnauthorized
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(headers.Get(headerActorTimestamp)), 10, 64)
	if err != nil {
		return nil, store.ErrUnauthorized
	}
	env.Timestamp = ts
	if env.MailboxID != strings.TrimSpace(mailboxID) {
		return nil, store.ErrUnauthorized
	}
	if expectedPermission != "" && env.Permission != expectedPermission {
		return nil, store.ErrUnauthorized
	}
	if expectedAction != "" && env.Action != expectedAction {
		return nil, store.ErrUnauthorized
	}
	if expectedTarget != "" && env.TargetID != expectedTarget {
		return nil, store.ErrUnauthorized
	}
	window := s.ActorEnvelopeReplayWindow
	if window <= 0 {
		window = 5 * time.Minute
	}
	now := time.Now().UTC()
	tsTime := time.Unix(env.Timestamp, 0).UTC()
	if tsTime.Before(now.Add(-window)) || tsTime.After(now.Add(window)) {
		return nil, store.ErrUnauthorized
	}
	expectedMAC := computeActorEnvelopeMAC(secret, env)
	gotMAC, err := base64.StdEncoding.DecodeString(env.SignatureBase64)
	if err != nil {
		return nil, store.ErrUnauthorized
	}
	if subtle.ConstantTimeCompare(expectedMAC, gotMAC) != 1 {
		return nil, store.ErrUnauthorized
	}
	if err := s.Store.RecordActorReplay(ctx, mailboxID, env.RequestID, env.Nonce, env.Action, env.TargetID, tsTime.Add(window)); err != nil {
		return nil, err
	}
	return &model.VerifiedActor{
		UserID:           env.UserID,
		MailboxID:        env.MailboxID,
		RequestID:        env.RequestID,
		PermissionIntent: env.Permission,
		Action:           env.Action,
		TargetID:         env.TargetID,
		Nonce:            env.Nonce,
		VerifiedAt:       now,
	}, nil
}

func computeActorEnvelopeMAC(secret string, env model.ActorEnvelope) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(actorEnvelopeCanonicalPayload(env)))
	return mac.Sum(nil)
}

func actorEnvelopeCanonicalPayload(env model.ActorEnvelope) string {
	return strings.Join([]string{
		"v1",
		env.UserID,
		env.MailboxID,
		env.RequestID,
		env.Permission,
		strconv.FormatInt(env.Timestamp, 10),
		env.Nonce,
		env.Action,
		env.TargetID,
	}, "\n")
}

func connectUnauthorized(err error) error {
	if err == nil {
		return nil
	}
	if err == store.ErrUnauthorized {
		return connect.NewError(connect.CodeUnauthenticated, err)
	}
	return connect.NewError(connect.CodeUnauthenticated, err)
}
