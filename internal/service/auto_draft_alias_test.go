package service

import (
	"context"
	"testing"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/router"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type autoDraftAudienceStore struct{ *fakeStore }

func (s *autoDraftAudienceStore) ListAliases(context.Context, string) ([]router.Alias, error) {
	return []router.Alias{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}, {ID: "disabled", Enabled: false}}, nil
}
func (s *autoDraftAudienceStore) ReadMessage(_ context.Context, claims model.TokenClaims, id string) (model.Message, error) {
	if claims.IsAdmin() || claims.AliasID == nil {
		panic("automatic drafting must not use mailbox-wide authority")
	}
	if *claims.AliasID != "a" {
		return model.Message{}, store.ErrUnauthorized
	}
	return model.Message{ID: id}, nil
}
func TestAutoDraftRunsOnlyWithVisibleAliasClaims(t *testing.T) {
	svc := &Service{Store: &autoDraftAudienceStore{newFakeStore()}, MailboxID: "mailbox"}
	claims, err := svc.autoDraftAudienceClaims(context.Background(), "message")
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].AliasID == nil || *claims[0].AliasID != "a" || claims[0].IsAdmin() {
		t.Fatalf("unexpected auto-draft audience: %+v", claims)
	}
	if !claims[0].Has(model.ScopeRead) || !claims[0].Has(model.ScopeSend) {
		t.Fatal("missing scoped agent capabilities")
	}
}

func (s *autoDraftHarnessStore) ReadMessage(_ context.Context, claims model.TokenClaims, id string) (model.Message, error) {
	if claims.AliasID == nil || claims.IsAdmin() {
		return model.Message{}, store.ErrUnauthorized
	}
	return model.Message{ID: id}, nil
}
