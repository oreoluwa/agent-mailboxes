package router

import (
	"slices"
	"testing"
)

func TestResolveAudienceMatchesExactRecipients(t *testing.T) {
	aliases := []Alias{
		{ID: "a1", Email: "owner+alpha@mail.example.test", Enabled: true},
		{ID: "a2", Email: "owner+beta@mail.example.test", Enabled: true},
	}
	got := ResolveAudience([]string{" OWNER+ALPHA@MAIL.EXAMPLE.TEST ", "owner+beta@mail.example.test"}, "", aliases, false)
	slices.Sort(got)
	if !slices.Equal(got, []string{"a1", "a2"}) {
		t.Fatalf("expected both exact aliases, got %v", got)
	}

	got = ResolveAudience([]string{"owner@mail.example.test", "owner+alph@mail.example.test", "prefix-owner+alpha@mail.example.test"}, "", aliases, false)
	if len(got) != 0 {
		t.Fatalf("expected no audience for related but non-exact addresses, got %v", got)
	}
}

func TestResolveAudienceDoesNotTrustArbitraryRawHeaders(t *testing.T) {
	aliases := []Alias{{ID: "a1", Email: "owner+alpha@mail.example.test", Enabled: true}}
	rawHeaders := "From: owner+alpha@mail.example.test\nReply-To: owner+alpha@mail.example.test\nDelivered-To: owner+alpha@mail.example.test\nX-Note: notify owner+alpha@mail.example.test\n"

	got := ResolveAudience([]string{"owner@mail.example.test"}, rawHeaders, aliases, false)
	if len(got) != 0 {
		t.Fatalf("expected sender and unrelated headers not to grant audience, got %v", got)
	}
}

func TestResolveAudienceIgnoresDisabledAliases(t *testing.T) {
	aliases := []Alias{{ID: "a1", Email: "owner+alpha@mail.example.test", Enabled: false}}
	got := ResolveAudience([]string{"owner+alpha@mail.example.test"}, "", aliases, false)
	if len(got) != 0 {
		t.Fatalf("expected disabled alias not to match, got %v", got)
	}
}
