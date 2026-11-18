package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oreoluwa/agent-mailboxes/internal/auth"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type onboardStubStore struct {
	store.Store
	input           store.OnboardMailboxInput
	result          store.OnboardMailboxResult
	err             error
	provisionInput  store.ProvisionMailboxAgentInput
	provisionResult store.ProvisionMailboxAgentResult
	provisionErr    error
	committed       bool
}

func (s *onboardStubStore) ProvisionMailboxAgent(_ context.Context, input store.ProvisionMailboxAgentInput) (store.ProvisionMailboxAgentResult, error) {
	s.provisionInput = input
	return s.provisionResult, s.provisionErr
}

func (s *onboardStubStore) OnboardMailbox(_ context.Context, input store.OnboardMailboxInput) (store.OnboardMailboxResult, error) {
	s.input = input
	if s.err != nil {
		return store.OnboardMailboxResult{}, s.err
	}
	s.committed = true
	return s.result, nil
}

func validOnboardPayload() map[string]any {
	return map[string]any{
		"name": "zoho", "imap_host": "imap.example.test", "imap_port": 993, "imap_tls": true,
		"smtp_host": "smtp.example.test", "smtp_port": 465, "smtp_tls": true,
		"username": "owner@example.test", "password": "provider-secret",
		"alias_email": "agent@example.test", "token_name": "agent-token",
	}
}

func onboardRequest(t *testing.T, server *Server, masterKey string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/onboard", bytes.NewReader(body))
	if masterKey != "" {
		req.Header.Set("X-Master-Key", masterKey)
	}
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	return recorder
}

func TestOnboardMailboxCommitsBeforeStartingPoller(t *testing.T) {
	const masterKey = "test-master"
	st := &onboardStubStore{result: store.OnboardMailboxResult{
		Mailbox: store.Mailbox{ID: "mailbox-1", Name: "zoho", Enabled: true}, AliasID: "alias-1", TokenID: "token-1",
	}}
	callbackCalled := false
	server := &Server{
		Store: st, MasterKey: masterKey, MasterKeys: []string{masterKey},
		ValidateMailboxCredentials: func(_ context.Context, creds store.MailboxCreds) error {
			if creds.Password != "provider-secret" {
				t.Fatal("validator did not receive provider credential")
			}
			return nil
		},
		OnMailboxCreated: func(_ context.Context, mailbox store.Mailbox) (bool, error) {
			callbackCalled = true
			if !st.committed || mailbox.ID != "mailbox-1" {
				t.Fatal("poller callback ran before atomic onboarding commit")
			}
			return true, nil
		},
	}
	recorder := onboardRequest(t, server, masterKey, validOnboardPayload())
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("onboarding credential response must not be cached")
	}
	if !callbackCalled {
		t.Fatal("poller callback was not invoked after commit")
	}
	if st.input.AliasEmail != "agent@example.test" || strings.Join(st.input.Scopes, ",") != "read,send" {
		t.Fatalf("atomic onboarding input=%+v", st.input)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	rawToken, _ := response["token"].(string)
	if rawToken == "" || st.input.TokenHash != auth.HashToken(masterKey, rawToken) {
		t.Fatal("response token was not bound to the stored hash")
	}
	if strings.Contains(recorder.Body.String(), "provider-secret") {
		t.Fatal("provider credential leaked in response")
	}
}

func TestOnboardMailboxFailureDoesNotStartPollerOrLeakCredential(t *testing.T) {
	st := &onboardStubStore{err: errors.New("duplicate mailbox")}
	callbackCalled := false
	server := &Server{
		Store: st, MasterKey: "master", MasterKeys: []string{"master"},
		ValidateMailboxCredentials: func(context.Context, store.MailboxCreds) error { return nil },
		OnMailboxCreated:           func(context.Context, store.Mailbox) (bool, error) { callbackCalled = true; return true, nil },
	}
	recorder := onboardRequest(t, server, "master", validOnboardPayload())
	if recorder.Code == http.StatusOK || callbackCalled {
		t.Fatalf("status=%d callback=%v", recorder.Code, callbackCalled)
	}

	server.ValidateMailboxCredentials = func(_ context.Context, creds store.MailboxCreds) error {
		return errors.New("provider rejected " + creds.Password)
	}
	recorder = onboardRequest(t, server, "master", validOnboardPayload())
	if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Body.String(), "provider-secret") {
		t.Fatalf("credential validation response=%q status=%d", recorder.Body.String(), recorder.Code)
	}
}

func TestOnboardMailboxProviderValidationLogsSafeDetails(t *testing.T) {
	tests := []struct {
		name          string
		validationErr error
		wantLog       []string
	}{
		{
			name:          "known protocol failure",
			validationErr: newMailboxCredentialValidationError("imap", errors.New("provider response: invalid password provider-secret")),
			wantLog:       []string{"protocol=imap", "category=provider_rejected", "error_type=*errors.errorString"},
		},
		{
			name:          "unknown validator failure",
			validationErr: errors.New("unexpected provider response provider-secret"),
			wantLog:       []string{"protocol=unknown", "category=unknown", "error_type=*errors.errorString"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			previousOutput, previousFlags, previousPrefix := log.Writer(), log.Flags(), log.Prefix()
			log.SetOutput(&logs)
			log.SetFlags(0)
			log.SetPrefix("")
			t.Cleanup(func() {
				log.SetOutput(previousOutput)
				log.SetFlags(previousFlags)
				log.SetPrefix(previousPrefix)
			})

			server := &Server{
				Store: &onboardStubStore{}, MasterKey: "master", MasterKeys: []string{"master"},
				ValidateMailboxCredentials: func(context.Context, store.MailboxCreds) error { return tt.validationErr },
			}
			recorder := onboardRequest(t, server, "master", validOnboardPayload())
			if recorder.Code != http.StatusBadRequest || strings.TrimSpace(recorder.Body.String()) != "provider credential validation failed" {
				t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
			}
			for _, want := range tt.wantLog {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("log=%q missing %q", logs.String(), want)
				}
			}
			for _, secret := range []string{"provider-secret", "provider response", "invalid password"} {
				if strings.Contains(logs.String(), secret) || strings.Contains(recorder.Body.String(), secret) {
					t.Fatalf("provider detail %q leaked: log=%q body=%q", secret, logs.String(), recorder.Body.String())
				}
			}
		})
	}
}

func TestMailboxCredentialValidationErrorCategory(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "context timeout", err: context.DeadlineExceeded, want: "timeout"},
		{name: "network timeout", err: &net.DNSError{IsTimeout: true}, want: "timeout"},
		{name: "TLS certificate", err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, want: "tls_certificate"},
		{name: "provider rejection", err: errors.New("provider response must stay private"), want: "provider_rejected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mailboxCredentialValidationErrorCategory(tt.err); got != tt.want {
				t.Fatalf("category=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestOnboardMailboxRequiresMasterAndExactAlias(t *testing.T) {
	called := false
	server := &Server{
		Store: &onboardStubStore{}, MasterKey: "master", MasterKeys: []string{"master"},
		ValidateMailboxCredentials: func(context.Context, store.MailboxCreds) error { called = true; return nil },
	}
	if response := onboardRequest(t, server, "", validOnboardPayload()); response.Code != http.StatusUnauthorized {
		t.Fatalf("missing master status=%d", response.Code)
	}
	payload := validOnboardPayload()
	payload["alias_email"] = "Agent <agent@example.test>"
	if response := onboardRequest(t, server, "master", payload); response.Code == http.StatusOK {
		t.Fatal("display-name alias was accepted")
	}
	if called {
		t.Fatal("provider validation ran for invalid alias")
	}
}

func TestOnboardingPageUsesAtomicEndpointAndDownloadProfile(t *testing.T) {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/onboarding", nil)
	(&Server{}).Handler().ServeHTTP(recorder, req)
	body := recorder.Body.String()
	for _, required := range []string{"/v1/admin/onboard", "aliasEmail", "tokenName", "Download Private Agent Profile", "URL.createObjectURL", "form.elements.password.value = \"\"", "submitEl.disabled = true", "requestBody = \"\"", "permissions to 0600"} {
		if !strings.Contains(body, required) {
			t.Fatalf("onboarding page missing %q", required)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage", `fetch("/v1/admin/mailboxes", {\n        method: "POST"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("onboarding page contains forbidden secret persistence or legacy submit: %q", forbidden)
		}
	}
}

func TestProvisionMailboxAgentRejectsDuplicateUnlessReplaceIsExplicit(t *testing.T) {
	const masterKey = "master"
	st := &onboardStubStore{provisionErr: store.ErrAgentCredentialExists}
	server := &Server{Store: st, MasterKey: masterKey, MasterKeys: []string{masterKey}}
	payload := map[string]any{"mailbox_name": "primary", "alias_email": "second@example.test", "token_name": "assistant"}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/agents", bytes.NewReader(body))
	req.Header.Set("X-Master-Key", masterKey)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict || st.provisionInput.Replace {
		t.Fatalf("status=%d input=%+v", recorder.Code, st.provisionInput)
	}

	st.provisionErr = nil
	st.provisionResult = store.ProvisionMailboxAgentResult{
		MailboxID: "mailbox-1", MailboxName: "primary", AliasID: "alias-2", AliasEmail: "second@example.test", TokenID: "token-2",
	}
	payload["replace"] = true
	body, _ = json.Marshal(payload)
	req = httptest.NewRequest(http.MethodPost, "/v1/admin/agents", bytes.NewReader(body))
	req.Header.Set("X-Master-Key", masterKey)
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !st.provisionInput.Replace || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d input=%+v", recorder.Code, st.provisionInput)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	rawToken, _ := response["token"].(string)
	if rawToken == "" || st.provisionInput.TokenHash != auth.HashToken(masterKey, rawToken) {
		t.Fatal("replacement token was not bound to its stored hash")
	}
}
