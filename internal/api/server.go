package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/auth"
	"github.com/oreoluwa/agent-mailboxes/internal/imapingest"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/service"
	"github.com/oreoluwa/agent-mailboxes/internal/smtpclient"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

type Server struct {
	Svc                        *service.Service
	ServicesByName             map[string]*service.Service
	ServicesByMailboxID        map[string]*service.Service
	Store                      store.Store
	Broker                     *EventBroker
	MasterKey                  string
	MasterKeys                 []string
	ActorEnvelopeSecret        string
	ActorEnvelopeReplayWindow  time.Duration
	AllowInboxAll              bool
	OnMailboxCreated           func(context.Context, store.Mailbox) (bool, error)
	OnMailboxEnabled           func(context.Context, string, bool) (bool, error)
	ValidateMailboxCredentials func(context.Context, store.MailboxCreds) error
	mu                         sync.RWMutex
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/onboarding", s.onboardingUI)
	mux.HandleFunc("/v1/admin/mailboxes", s.withBootstrapAuth(s.mailboxes))
	mux.HandleFunc("/v1/admin/onboard", s.withBootstrapAuth(s.onboardMailbox))
	mux.HandleFunc("/v1/admin/agents", s.withBootstrapAuth(s.provisionMailboxAgent))
	mux.HandleFunc("/v1/admin/mailboxes/enabled", s.withBootstrapAuth(s.mailboxEnabled))
	mux.HandleFunc("/v1/admin/mint-token", s.withBootstrapAuth(s.mintAdminToken))
	mux.HandleFunc("/v1/status", s.withAuth(s.status, model.ScopeAdmin))
	mux.HandleFunc("/v1/admin/aliases", s.withAuth(s.aliases, model.ScopeAdmin))
	mux.HandleFunc("/v1/admin/tokens", s.withAuth(s.tokens, model.ScopeAdmin))
	mux.HandleFunc("/v1/admin/tokens/revoke", s.withAuth(s.revokeToken, model.ScopeAdmin))
	mux.HandleFunc("/v1/admin/sync", s.withAuth(s.syncNow, model.ScopeAdmin))
	mux.HandleFunc("/v1/folders", s.withAuth(s.folders, model.ScopeRead))
	mux.HandleFunc("/v1/inbox/list", s.withAuth(s.listInbox, model.ScopeRead))
	mux.HandleFunc("/v1/inbox/read", s.withAuth(s.readInbox, model.ScopeRead))
	mux.HandleFunc("/v1/inbox/body", s.withAuth(s.bodyInbox, model.ScopeRead))
	mux.HandleFunc("/v1/inbox/raw", s.withAuth(s.rawInbox, model.ScopeRead))
	mux.HandleFunc("/v1/inbox/ack", s.withAuth(s.ackInbox, model.ScopeRead))
	mux.HandleFunc("/v1/inbox/unack", s.withAuth(s.unackInbox, model.ScopeRead))
	mux.HandleFunc("/v1/search", s.withAuth(s.search, model.ScopeRead))
	mux.HandleFunc("/v1/search/query", s.withAuth(s.searchStructured, model.ScopeRead))
	mux.HandleFunc("/v1/send", s.withAuth(s.send, model.ScopeSend))
	mux.HandleFunc("/v1/send/raw", s.withAuth(s.sendRaw, model.ScopeSend))
	mux.HandleFunc("/v1/reply", s.withAuth(s.reply, model.ScopeSend))
	mux.HandleFunc("/v1/drafts", s.withAuth(s.listDrafts, model.ScopeRead))
	mux.HandleFunc("/v1/drafts/get", s.withAuth(s.getDraft, model.ScopeRead))
	mux.HandleFunc("/v1/drafts/create", s.withAuth(s.createDraft, model.ScopeSend))
	mux.HandleFunc("/v1/drafts/revise", s.withAuth(s.reviseDraft, model.ScopeSend))
	mux.HandleFunc("/v1/drafts/discard", s.withAuth(s.discardDraft, model.ScopeSend))
	mux.HandleFunc("/v1/drafts/send", s.withAuth(s.sendDraft, model.ScopeSend))
	mux.HandleFunc("/v1/agent/run", s.withAuth(s.runAgent, model.ScopeSend))
	mux.HandleFunc("/v1/agent/conversations", s.withAuth(s.listAgentConversations, model.ScopeRead))
	mux.HandleFunc("/v1/agent/conversation", s.withAuth(s.getAgentConversation, model.ScopeRead))
	mux.HandleFunc("/v1/agent/context", s.withAuth(s.mailboxAgentContext, model.ScopeRead))
	mux.HandleFunc("/v1/agent/settings", s.withAuth(s.mailboxAgentSettings, model.ScopeRead))
	return mux
}

type ctxKey string

const claimsKey ctxKey = "claims"

func (s *Server) withAuth(next func(http.ResponseWriter, *http.Request, model.TokenClaims), scope model.Scope) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authSvc := s.authService()
		if authSvc == nil {
			http.Error(w, "service not configured: onboard at /onboarding", http.StatusServiceUnavailable)
			return
		}
		claims, err := authSvc.Authenticate(r.Context(), r.Header.Get("Authorization"), s.acceptedMasterKeys())
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if !claims.IsAdmin() && !claims.Has(scope) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), claimsKey, claims)), claims)
	}
}

func (s *Server) withBootstrapAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.isAcceptedMasterKey(r.Header.Get("X-Master-Key")) {
			next(w, r)
			return
		}
		authSvc := s.authService()
		if authSvc == nil {
			http.Error(w, "missing X-Master-Key", http.StatusUnauthorized)
			return
		}
		claims, err := authSvc.Authenticate(r.Context(), r.Header.Get("Authorization"), s.acceptedMasterKeys())
		if err != nil || !claims.IsAdmin() {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) authService() *service.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Svc != nil {
		return s.Svc
	}
	for _, svc := range s.ServicesByName {
		return svc
	}
	return nil
}

func (s *Server) requestedMailboxName(r *http.Request) string {
	if v := strings.TrimSpace(r.URL.Query().Get("mailbox")); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get("X-Mailbox-Name"))
}

func (s *Server) serviceForMailbox(name string) (*service.Service, error) {
	name = strings.TrimSpace(name)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if name == "" {
		if len(s.ServicesByName) == 0 {
			if s.Svc != nil {
				return s.Svc, nil
			}
			return nil, errors.New("service not configured")
		}
		if len(s.ServicesByName) == 1 {
			for _, svc := range s.ServicesByName {
				return svc, nil
			}
		}
		return nil, errors.New("mailbox is required when multiple mailboxes are active")
	}
	svc, ok := s.ServicesByName[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return svc, nil
}

func (s *Server) mailboxIDForRequest(r *http.Request) (string, error) {
	svc, err := s.serviceForMailbox(s.requestedMailboxName(r))
	if err != nil {
		return "", err
	}
	return svc.MailboxID, nil
}

func (s *Server) serviceForClaims(ctx context.Context, claims model.TokenClaims, r *http.Request) (*service.Service, error) {
	if claims.AliasID != nil {
		mailboxID, err := s.Store.GetMailboxIDByAliasID(ctx, *claims.AliasID)
		if err != nil {
			return nil, err
		}
		s.mu.RLock()
		if svc, ok := s.ServicesByMailboxID[mailboxID]; ok {
			s.mu.RUnlock()
			return svc, nil
		}
		if s.Svc != nil && s.Svc.MailboxID == mailboxID {
			svc := s.Svc
			s.mu.RUnlock()
			return svc, nil
		}
		s.mu.RUnlock()
		return nil, store.ErrNotFound
	}
	return s.serviceForMailbox(s.requestedMailboxName(r))
}

func (s *Server) RegisterMailboxService(name string, svc *service.Service) bool {
	name = strings.TrimSpace(name)
	if name == "" || svc == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ServicesByName == nil {
		s.ServicesByName = map[string]*service.Service{}
	}
	if s.ServicesByMailboxID == nil {
		s.ServicesByMailboxID = map[string]*service.Service{}
	}
	if _, exists := s.ServicesByName[name]; exists {
		return false
	}
	s.ServicesByName[name] = svc
	s.ServicesByMailboxID[svc.MailboxID] = svc
	if s.Svc == nil {
		s.Svc = svc
	}
	return true
}

func (s *Server) UnregisterMailboxService(name, mailboxID string) bool {
	name = strings.TrimSpace(name)
	mailboxID = strings.TrimSpace(mailboxID)
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, exists := s.ServicesByName[name]
	if !exists {
		return false
	}
	delete(s.ServicesByName, name)
	if mailboxID == "" {
		mailboxID = svc.MailboxID
	}
	delete(s.ServicesByMailboxID, mailboxID)
	if s.Svc == svc {
		s.Svc = nil
		for _, v := range s.ServicesByName {
			s.Svc = v
			break
		}
	}
	return true
}

func (s *Server) onboardingUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	type data struct {
		Now string
	}
	const page = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Emaild Onboarding</title>
  <style>
    :root { color-scheme: light; font-family: "Avenir Next", "Segoe UI", sans-serif; }
    body { margin: 0; background: linear-gradient(120deg, #f8fafc, #e2e8f0); color: #0f172a; }
    .wrap { max-width: 760px; margin: 48px auto; padding: 24px; background: #ffffffdd; border-radius: 16px; box-shadow: 0 12px 40px rgba(15,23,42,0.12); }
    h1 { margin-top: 0; font-size: 1.8rem; }
    p { color: #334155; }
    form { display: grid; gap: 12px; grid-template-columns: repeat(2, minmax(0, 1fr)); }
    label { display: grid; gap: 4px; font-size: 0.9rem; color: #1e293b; }
    input { padding: 10px; border: 1px solid #cbd5e1; border-radius: 10px; font: inherit; }
    .full { grid-column: 1 / -1; }
    .row { display: flex; gap: 16px; align-items: center; }
    button { padding: 10px 16px; border: 0; border-radius: 10px; background: #0f766e; color: white; font: inherit; cursor: pointer; }
    pre { background: #0f172a; color: #f8fafc; padding: 12px; border-radius: 10px; overflow: auto; }
    @media (max-width: 720px) { form { grid-template-columns: 1fr; } }
  </style>
</head>
<body>
  <main class="wrap">
    <h1>Mailbox Onboarding</h1>
    <p>Create a mailbox, its first agent alias, and a scoped agent profile in one onboarding step.</p>
    <form id="form">
      <label class="full">Master Key<input name="masterKey" type="password" required /></label>
      <label>Mailbox Name<input name="name" value="primary" required /></label>
      <label>IMAP Host<input name="imapHost" required /></label>
      <label>IMAP Port<input name="imapPort" type="number" value="993" required /></label>
      <label>IMAP TLS<input name="imapTLS" value="true" /></label>
      <label>SMTP Host<input name="smtpHost" required /></label>
      <label>SMTP Port<input name="smtpPort" type="number" value="465" required /></label>
      <label>SMTP TLS<input name="smtpTLS" value="true" /></label>
      <label>Username<input name="username" required /></label>
      <label>Password<input name="password" type="password" required /></label>
      <label>Agent Alias<input name="aliasEmail" type="email" required /></label>
      <label>Agent Profile Name<input name="tokenName" placeholder="assistant" /></label>
      <div class="full row"><button id="submit" type="submit">Create Mailbox + Agent</button><button id="download" type="button" hidden>Download Private Agent Profile</button><span id="status"></span></div>
    </form>
    <h2>Configured Mailboxes</h2>
    <pre id="list">Loading...</pre>
    <p><small>Updated at {{.Now}} UTC. If <code>emaild</code> is running with <code>--all-mailboxes</code>, new enabled mailboxes start automatically; otherwise restart after onboarding.</small></p>
  </main>
  <script>
    const form = document.getElementById("form");
    const statusEl = document.getElementById("status");
    const listEl = document.getElementById("list");
    const submitEl = document.getElementById("submit");
    const downloadEl = document.getElementById("download");
    let profileURL = "";
    async function loadList(masterKey) {
      const res = await fetch("/v1/admin/mailboxes", { headers: masterKey ? { "X-Master-Key": masterKey } : {} });
      if (!res.ok) { listEl.textContent = "Provide master key and submit to load mailbox list."; return; }
      const data = await res.json();
      listEl.textContent = JSON.stringify(data, null, 2);
    }
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      if (submitEl.disabled) return;
      submitEl.disabled = true;
      const fd = new FormData(form);
      const body = {
        name: fd.get("name"),
        imap_host: fd.get("imapHost"),
        imap_port: Number(fd.get("imapPort")),
        imap_tls: String(fd.get("imapTLS")).toLowerCase() === "true",
        smtp_host: fd.get("smtpHost"),
        smtp_port: Number(fd.get("smtpPort")),
        smtp_tls: String(fd.get("smtpTLS")).toLowerCase() === "true",
        username: fd.get("username"),
        password: fd.get("password"),
        alias_email: fd.get("aliasEmail"),
        token_name: fd.get("tokenName") || "agent"
      };
      const masterKey = String(fd.get("masterKey"));
      let requestBody = JSON.stringify(body);
      form.elements.password.value = "";
      fd.delete("password");
      body.password = "";
      statusEl.textContent = "Validating provider and creating agent...";
      try {
        const res = await fetch("/v1/admin/onboard", {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-Master-Key": masterKey },
          body: requestBody
        });
        requestBody = "";
        if (!res.ok) {
          statusEl.textContent = "Onboarding failed. Check the provider settings and server logs.";
          return;
        }
        const out = await res.json();
        const profile = { version: 1, api_url: location.origin, mailbox: out.mailbox_name, alias: out.alias_email, token: out.token };
        const nextProfileURL = URL.createObjectURL(new Blob([JSON.stringify(profile, null, 2) + "\n"], { type: "application/json" }));
        if (profileURL) URL.revokeObjectURL(profileURL);
        profileURL = nextProfileURL;
        downloadEl.onclick = () => {
          const link = document.createElement("a");
          link.href = profileURL;
          link.download = out.alias_email.replace(/[^a-z0-9._-]+/gi, "-") + ".agent-email.json";
          link.click();
          statusEl.textContent = "Profile downloaded. Set file permissions to 0600 before using email --profile.";
        };
        downloadEl.hidden = false;
        out.token = "";
        statusEl.textContent = out.poller_started ? "Ready and polling; download the private agent profile" : "Ready; download the private profile, then start or restart the mailbox worker";
        try {
          await loadList(masterKey);
        } catch (_) {
          statusEl.textContent = "Ready; profile download is available, but mailbox list refresh failed";
        }
      } catch (_) {
        statusEl.textContent = "Onboarding request failed. Existing profile download remains available.";
      } finally {
        requestBody = "";
        submitEl.disabled = false;
      }
    });
    loadList();
  </script>
</body>
</html>`
	tpl, err := template.New("onboarding").Parse(page)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tpl.Execute(w, data{Now: time.Now().UTC().Format(time.RFC3339)})
}

func (s *Server) isAcceptedMasterKey(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	for _, k := range s.acceptedMasterKeys() {
		if v == k {
			return true
		}
	}
	return false
}

func (s *Server) acceptedMasterKeys() []string {
	if len(s.MasterKeys) > 0 {
		return s.MasterKeys
	}
	if strings.TrimSpace(s.MasterKey) == "" {
		return nil
	}
	return []string{s.MasterKey}
}

func (s *Server) mailboxes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := s.Store.ListMailboxes(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		type outMailbox struct {
			ID        string    `json:"id"`
			Name      string    `json:"name"`
			Enabled   bool      `json:"enabled"`
			IMAPHost  string    `json:"imap_host"`
			IMAPPort  int       `json:"imap_port"`
			IMAPTLS   bool      `json:"imap_tls"`
			SMTPHost  string    `json:"smtp_host"`
			SMTPPort  int       `json:"smtp_port"`
			SMTPTLS   bool      `json:"smtp_tls"`
			Username  string    `json:"username"`
			CreatedAt time.Time `json:"created_at"`
		}
		out := make([]outMailbox, 0, len(items))
		for _, m := range items {
			out = append(out, outMailbox{
				ID:        m.ID,
				Name:      m.Name,
				Enabled:   m.Enabled,
				IMAPHost:  m.Creds.IMAPHost,
				IMAPPort:  m.Creds.IMAPPort,
				IMAPTLS:   m.Creds.IMAPTLS,
				SMTPHost:  m.Creds.SMTPHost,
				SMTPPort:  m.Creds.SMTPPort,
				SMTPTLS:   m.Creds.SMTPTLS,
				Username:  m.Creds.Username,
				CreatedAt: m.CreatedAt,
			})
		}
		writeJSON(w, out)
	case http.MethodPost:
		var in struct {
			Name     string `json:"name"`
			IMAPHost string `json:"imap_host"`
			IMAPPort int    `json:"imap_port"`
			IMAPTLS  bool   `json:"imap_tls"`
			SMTPHost string `json:"smtp_host"`
			SMTPPort int    `json:"smtp_port"`
			SMTPTLS  bool   `json:"smtp_tls"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, err)
			return
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			writeErr(w, errors.New("name is required"))
			return
		}
		creds := store.MailboxCreds{
			IMAPHost: strings.TrimSpace(in.IMAPHost),
			IMAPPort: in.IMAPPort,
			IMAPTLS:  in.IMAPTLS,
			SMTPHost: strings.TrimSpace(in.SMTPHost),
			SMTPPort: in.SMTPPort,
			SMTPTLS:  in.SMTPTLS,
			Username: strings.TrimSpace(in.Username),
			Password: in.Password,
		}
		if err := validateMailboxCredentials(r.Context(), creds); err != nil {
			writeErr(w, err)
			return
		}
		mb, err := s.Store.CreateMailbox(r.Context(), name, creds)
		if err != nil {
			writeErr(w, err)
			return
		}
		pollerStarted := false
		var pollerErr string
		if s.OnMailboxCreated != nil {
			started, err := s.OnMailboxCreated(r.Context(), mb)
			pollerStarted = started
			if err != nil {
				pollerErr = err.Error()
				log.Printf("mailbox created but poller activation failed mailbox=%s err=%v", mb.Name, err)
			}
		}
		resp := map[string]any{"status": "ok", "mailbox_id": mb.ID, "mailbox_name": mb.Name, "poller_started": pollerStarted}
		if pollerErr != "" {
			resp["poller_error"] = pollerErr
		}
		writeJSON(w, resp)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) onboardMailbox(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Name       string `json:"name"`
		IMAPHost   string `json:"imap_host"`
		IMAPPort   int    `json:"imap_port"`
		IMAPTLS    bool   `json:"imap_tls"`
		SMTPHost   string `json:"smtp_host"`
		SMTPPort   int    `json:"smtp_port"`
		SMTPTLS    bool   `json:"smtp_tls"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		AliasEmail string `json:"alias_email"`
		TokenName  string `json:"token_name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		writeErr(w, errors.New("invalid onboarding request"))
		return
	}
	name := strings.TrimSpace(in.Name)
	aliasEmail := strings.ToLower(strings.TrimSpace(in.AliasEmail))
	tokenName := strings.TrimSpace(in.TokenName)
	if name == "" || len(name) > 128 || tokenName == "" || len(tokenName) > 200 {
		writeErr(w, errors.New("valid name and token_name are required"))
		return
	}
	if !isExactEmailAddress(aliasEmail) {
		writeErr(w, errors.New("alias_email must be one exact email address"))
		return
	}
	creds := store.MailboxCreds{
		IMAPHost: strings.TrimSpace(in.IMAPHost), IMAPPort: in.IMAPPort, IMAPTLS: in.IMAPTLS,
		SMTPHost: strings.TrimSpace(in.SMTPHost), SMTPPort: in.SMTPPort, SMTPTLS: in.SMTPTLS,
		Username: strings.TrimSpace(in.Username), Password: in.Password,
	}
	if !creds.IMAPTLS || !creds.SMTPTLS || creds.IMAPHost == "" || len(creds.IMAPHost) > 253 || creds.SMTPHost == "" || len(creds.SMTPHost) > 253 || creds.Username == "" || len(creds.Username) > 320 || creds.Password == "" || len(creds.Password) > 16384 || creds.IMAPPort < 1 || creds.IMAPPort > 65535 || creds.SMTPPort < 1 || creds.SMTPPort > 65535 {
		writeErr(w, errors.New("complete implicit-TLS IMAP and SMTP settings are required"))
		return
	}
	validator := s.ValidateMailboxCredentials
	if validator == nil {
		validator = validateMailboxCredentials
	}
	if err := validator(r.Context(), creds); err != nil {
		protocol, category, errorType := mailboxCredentialValidationFailureDetails(err)
		log.Printf("provider credential validation failed protocol=%s category=%s error_type=%s", protocol, category, errorType)
		http.Error(w, "provider credential validation failed", http.StatusBadRequest)
		return
	}
	rawToken, err := auth.GenerateToken()
	if err != nil {
		writeErr(w, errors.New("generate agent credential"))
		return
	}
	result, err := s.Store.OnboardMailbox(r.Context(), store.OnboardMailboxInput{
		Name: name, Creds: creds, AliasEmail: aliasEmail, TokenName: tokenName,
		TokenHash: auth.HashToken(s.MasterKey, rawToken), Scopes: []string{string(model.ScopeRead), string(model.ScopeSend)},
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	pollerStarted := false
	var pollerErr string
	if s.OnMailboxCreated != nil {
		started, callbackErr := s.OnMailboxCreated(r.Context(), result.Mailbox)
		pollerStarted = started
		if callbackErr != nil {
			pollerErr = callbackErr.Error()
			log.Printf("mailbox onboarded but poller activation failed mailbox=%s err=%v", result.Mailbox.Name, callbackErr)
		}
	}
	response := map[string]any{
		"status": "ok", "mailbox_id": result.Mailbox.ID, "mailbox_name": result.Mailbox.Name,
		"alias_email": aliasEmail, "token": rawToken, "token_id": result.TokenID, "poller_started": pollerStarted,
	}
	if pollerErr != "" {
		response["poller_error"] = pollerErr
	}
	writeJSON(w, response)
}

func (s *Server) provisionMailboxAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		MailboxName string `json:"mailbox_name"`
		AliasEmail  string `json:"alias_email"`
		TokenName   string `json:"token_name"`
		Replace     bool   `json:"replace"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		writeErr(w, errors.New("invalid agent provisioning request"))
		return
	}
	mailboxName := strings.TrimSpace(in.MailboxName)
	aliasEmail := strings.ToLower(strings.TrimSpace(in.AliasEmail))
	tokenName := strings.TrimSpace(in.TokenName)
	if mailboxName == "" || len(mailboxName) > 128 || tokenName == "" || len(tokenName) > 200 || !isExactEmailAddress(aliasEmail) {
		writeErr(w, errors.New("valid mailbox_name, alias_email, and token_name are required"))
		return
	}
	rawToken, err := auth.GenerateToken()
	if err != nil {
		writeErr(w, errors.New("generate agent credential"))
		return
	}
	result, err := s.Store.ProvisionMailboxAgent(r.Context(), store.ProvisionMailboxAgentInput{
		MailboxName: mailboxName, AliasEmail: aliasEmail, TokenName: tokenName,
		TokenHash: auth.HashToken(s.MasterKey, rawToken), Scopes: []string{string(model.ScopeRead), string(model.ScopeSend)}, Replace: in.Replace,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"status": "ok", "mailbox_id": result.MailboxID, "mailbox_name": result.MailboxName,
		"alias_email": result.AliasEmail, "token": rawToken, "token_id": result.TokenID,
	})
}

func isExactEmailAddress(value string) bool {
	if value == "" || len(value) > 320 {
		return false
	}
	parsed, err := mail.ParseAddress(value)
	return err == nil && parsed.Name == "" && strings.ToLower(parsed.Address) == value
}

func validateMailboxCredentials(ctx context.Context, creds store.MailboxCreds) error {
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	imap := imapingest.IMAPFetcher{
		Host:     creds.IMAPHost,
		Port:     creds.IMAPPort,
		TLS:      creds.IMAPTLS,
		Username: creds.Username,
		Password: creds.Password,
		Folder:   "INBOX",
	}
	if err := imap.ValidateCredentialsContext(checkCtx); err != nil {
		return newMailboxCredentialValidationError("imap", err)
	}
	smtp := smtpclient.Client{
		Host:     creds.SMTPHost,
		Port:     creds.SMTPPort,
		TLS:      creds.SMTPTLS,
		Username: creds.Username,
		Password: creds.Password,
	}
	if err := smtp.ValidateCredentials(checkCtx); err != nil {
		return newMailboxCredentialValidationError("smtp", err)
	}
	return nil
}

type mailboxCredentialValidationError struct {
	protocol string
	category string
	cause    error
}

func newMailboxCredentialValidationError(protocol string, cause error) error {
	return &mailboxCredentialValidationError{
		protocol: protocol,
		category: mailboxCredentialValidationErrorCategory(cause),
		cause:    cause,
	}
}

func (e *mailboxCredentialValidationError) Error() string {
	return e.protocol + " provider credential validation failed"
}

func (e *mailboxCredentialValidationError) Unwrap() error {
	return e.cause
}

func mailboxCredentialValidationErrorCategory(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var tlsVerificationErr *tls.CertificateVerificationError
	var unknownAuthorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certificateInvalidErr x509.CertificateInvalidError
	var systemRootsErr x509.SystemRootsError
	if errors.As(err, &tlsVerificationErr) ||
		errors.As(err, &unknownAuthorityErr) ||
		errors.As(err, &hostnameErr) ||
		errors.As(err, &certificateInvalidErr) ||
		errors.As(err, &systemRootsErr) {
		return "tls_certificate"
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		if networkErr.Timeout() {
			return "timeout"
		}
		return "network"
	}
	return "provider_rejected"
}

func mailboxCredentialValidationFailureDetails(err error) (protocol, category, errorType string) {
	protocol, category = "unknown", "unknown"
	errorType = fmt.Sprintf("%T", err)
	var validationErr *mailboxCredentialValidationError
	if errors.As(err, &validationErr) {
		protocol = validationErr.protocol
		category = validationErr.category
		errorType = fmt.Sprintf("%T", validationErr.cause)
	}
	return protocol, category, errorType
}

func (s *Server) mailboxEnabled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeErr(w, errors.New("name is required"))
		return
	}
	if err := s.Store.SetMailboxEnabled(r.Context(), in.Name, in.Enabled); err != nil {
		writeErr(w, err)
		return
	}
	pollerUpdated := false
	var pollerErr string
	if s.OnMailboxEnabled != nil {
		updated, err := s.OnMailboxEnabled(r.Context(), in.Name, in.Enabled)
		pollerUpdated = updated
		if err != nil {
			pollerErr = err.Error()
			log.Printf("mailbox enabled state changed but runtime update failed mailbox=%s enabled=%v err=%v", strings.TrimSpace(in.Name), in.Enabled, err)
		}
	}
	resp := map[string]any{"status": "ok", "name": strings.TrimSpace(in.Name), "enabled": in.Enabled, "poller_updated": pollerUpdated}
	if pollerErr != "" {
		resp["poller_error"] = pollerErr
	}
	writeJSON(w, resp)
}

func (s *Server) mintAdminToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name := "cli"
	if r.Body != nil {
		var in struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if s := strings.TrimSpace(in.Name); s != "" {
			name = s
		}
	}
	authSvc := s.authService()
	if authSvc == nil {
		writeErr(w, errors.New("service not configured"))
		return
	}
	tok, id, err := authSvc.MintAdminToken(r.Context(), name, s.MasterKey)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"token": tok, "token_id": id})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if name := s.requestedMailboxName(r); name != "" {
		svc, err := s.serviceForMailbox(name)
		if err != nil {
			writeErr(w, err)
			return
		}
		st, err := svc.Store.Status(r.Context(), svc.MailboxID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, st)
		return
	}
	s.mu.RLock()
	servicesCount := len(s.ServicesByName)
	s.mu.RUnlock()
	if servicesCount <= 1 {
		svc := s.authService()
		if svc == nil {
			writeErr(w, errors.New("service not configured"))
			return
		}
		st, err := svc.Store.Status(r.Context(), svc.MailboxID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, st)
		return
	}
	type mailboxStatus struct {
		Mailbox string       `json:"mailbox"`
		Status  store.Status `json:"status"`
	}
	s.mu.RLock()
	services := make(map[string]*service.Service, len(s.ServicesByName))
	for name, svc := range s.ServicesByName {
		services[name] = svc
	}
	s.mu.RUnlock()
	out := make([]mailboxStatus, 0, len(services))
	for name, svc := range services {
		st, err := svc.Store.Status(r.Context(), svc.MailboxID)
		if err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, mailboxStatus{Mailbox: name, Status: st})
	}
	writeJSON(w, out)
}

func (s *Server) aliases(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	mailboxID, err := s.mailboxIDForRequest(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		aliases, err := s.Store.ListAliases(r.Context(), mailboxID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, aliases)
	case http.MethodPost:
		var in struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, err)
			return
		}
		if err := s.Store.AddAlias(r.Context(), mailboxID, in.Email); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) tokens(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		AliasEmail string   `json:"alias_email"`
		Name       string   `json:"name"`
		Scopes     []string `json:"scopes"`
		Admin      bool     `json:"admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	authSvc := s.authService()
	if authSvc == nil {
		writeErr(w, errors.New("service not configured"))
		return
	}
	if in.Admin {
		tok, id, err := authSvc.MintAdminToken(r.Context(), in.Name, s.MasterKey)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"token": tok, "token_id": id})
		return
	}
	svc, err := s.serviceForMailbox(s.requestedMailboxName(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	tok, id, err := svc.MintToken(r.Context(), in.AliasEmail, in.Name, s.MasterKey, in.Scopes)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"token": tok, "token_id": id})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		TokenID string `json:"token_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Store.RevokeToken(r.Context(), in.TokenID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) syncNow(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	syncCtx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if name := s.requestedMailboxName(r); name != "" {
		svc, err := s.serviceForMailbox(name)
		if err != nil {
			writeErr(w, err)
			return
		}
		if err := svc.SyncOnce(syncCtx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "ok", "mailbox": name})
		return
	}
	s.mu.RLock()
	servicesCount := len(s.ServicesByName)
	s.mu.RUnlock()
	if servicesCount == 0 {
		svc := s.authService()
		if svc == nil {
			writeErr(w, errors.New("service not configured"))
			return
		}
		if err := svc.SyncOnce(syncCtx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	s.mu.RLock()
	services := make([]*service.Service, 0, len(s.ServicesByName))
	for _, svc := range s.ServicesByName {
		services = append(services, svc)
	}
	s.mu.RUnlock()
	for _, svc := range services {
		if err := svc.SyncOnce(syncCtx); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) folders(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	folders, err := s.listFoldersOp(r.Context(), claims)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, folders)
}

func (s *Server) listInbox(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	items, err := s.listInboxOp(r.Context(), claims, listInboxParams{
		Folder: strings.TrimSpace(r.URL.Query().Get("folder")),
		Unread: r.URL.Query().Get("unread") == "1" || strings.EqualFold(r.URL.Query().Get("unread"), "true"),
		Limit:  atoiDefault(r.URL.Query().Get("limit")),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, items)
}

func (s *Server) readInbox(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	m, err := s.readMessageOp(r.Context(), claims, messageParams{
		MailboxName: s.requestedMailboxName(r),
		MessageID:   r.URL.Query().Get("id"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, m)
}

func (s *Server) bodyInbox(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	b, err := s.getMessageBodyOp(r.Context(), claims, messageParams{
		MailboxName: s.requestedMailboxName(r),
		MessageID:   r.URL.Query().Get("id"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, b)
}

func (s *Server) rawInbox(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	raw, err := s.getRawMessageOp(r.Context(), claims, messageParams{
		MailboxName: s.requestedMailboxName(r),
		MessageID:   r.URL.Query().Get("id"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	_, _ = w.Write(raw)
}

func (s *Server) ackInbox(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		MessageID string `json:"message_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.ackMessageOp(r.Context(), claims, messageParams{
		MailboxName: s.requestedMailboxName(r),
		MessageID:   in.MessageID,
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) unackInbox(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		MessageID string `json:"message_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.unackMessageOp(r.Context(), claims, messageParams{
		MailboxName: s.requestedMailboxName(r),
		MessageID:   in.MessageID,
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) send(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		To             []string `json:"to"`
		Subject        string   `json:"subject"`
		Body           string   `json:"body"`
		IdempotencyKey string   `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	job, err := s.sendMessageOp(r.Context(), claims, sendMessageParams{
		MailboxName:    s.requestedMailboxName(r),
		To:             in.To,
		Subject:        in.Subject,
		Body:           in.Body,
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "queued", "job_id": job.ID})
}

func (s *Server) listDrafts(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	var sourceMessageID *string
	if value := strings.TrimSpace(r.URL.Query().Get("source_message_id")); value != "" {
		sourceMessageID = &value
	} else if value := strings.TrimSpace(r.URL.Query().Get("message_id")); value != "" {
		sourceMessageID = &value
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	items, err := s.listDraftsOp(r.Context(), claims, listDraftsParams{
		MailboxName:     s.requestedMailboxName(r),
		SourceMessageID: sourceMessageID,
		Status:          model.DraftStatus(strings.TrimSpace(r.URL.Query().Get("status"))),
		Limit:           limit,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, items)
}

func (s *Server) getDraft(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	item, err := s.getDraftOp(r.Context(), claims, draftIDParams{
		MailboxName: s.requestedMailboxName(r),
		DraftID:     r.URL.Query().Get("id"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, item)
}

func (s *Server) createDraft(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	svc, err := s.serviceForClaims(r.Context(), claims, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		SourceMessageID *string  `json:"source_message_id"`
		ConversationID  *string  `json:"conversation_id"`
		Subject         string   `json:"subject"`
		To              []string `json:"to"`
		Body            string   `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	target := svc.MailboxID
	if in.SourceMessageID != nil && strings.TrimSpace(*in.SourceMessageID) != "" {
		target = strings.TrimSpace(*in.SourceMessageID)
	}
	actor, err := s.verifyActorEnvelope(r.Context(), r.Header, svc.MailboxID, "send", "create_draft", target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	draft, err := s.createDraftOp(r.Context(), claims, createDraftParams{
		MailboxName:     svc.MailboxName,
		SourceMessageID: in.SourceMessageID,
		ConversationID:  in.ConversationID,
		Subject:         in.Subject,
		To:              in.To,
		Body:            in.Body,
		Actor:           actor,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, draft)
}

func (s *Server) reviseDraft(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	svc, err := s.serviceForClaims(r.Context(), claims, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		DraftID string   `json:"draft_id"`
		Subject string   `json:"subject"`
		To      []string `json:"to"`
		Body    string   `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	actor, err := s.verifyActorEnvelope(r.Context(), r.Header, svc.MailboxID, "send", "revise_draft", strings.TrimSpace(in.DraftID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	draft, err := s.reviseDraftOp(r.Context(), claims, reviseDraftParams{
		MailboxName: svc.MailboxName,
		DraftID:     in.DraftID,
		Subject:     in.Subject,
		To:          in.To,
		Body:        in.Body,
		Actor:       actor,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, draft)
}

func (s *Server) discardDraft(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	svc, err := s.serviceForClaims(r.Context(), claims, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		DraftID string `json:"draft_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	actor, err := s.verifyActorEnvelope(r.Context(), r.Header, svc.MailboxID, "send", "discard_draft", strings.TrimSpace(in.DraftID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	draft, err := s.discardDraftOp(r.Context(), claims, sendFromDraftParams{
		MailboxName: svc.MailboxName,
		DraftID:     in.DraftID,
		Actor:       actor,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, draft)
}

func (s *Server) sendDraft(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	svc, err := s.serviceForClaims(r.Context(), claims, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		DraftID            string `json:"draft_id"`
		ReviewedRevisionID string `json:"reviewed_revision_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	draftID := strings.TrimSpace(in.DraftID)
	reviewedRevisionID := strings.TrimSpace(in.ReviewedRevisionID)
	if draftID == "" || reviewedRevisionID == "" {
		http.Error(w, "draft_id and reviewed_revision_id are required", http.StatusBadRequest)
		return
	}
	actor, err := s.verifyActorEnvelope(r.Context(), r.Header, svc.MailboxID, "send", "send_from_draft", draftApprovalTarget(draftID, reviewedRevisionID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	result, err := s.sendFromDraftOp(r.Context(), claims, sendFromDraftParams{
		MailboxName:        svc.MailboxName,
		DraftID:            draftID,
		ReviewedRevisionID: reviewedRevisionID,
		Actor:              actor,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"status": "queued", "job_id": result.Outbound.ID, "draft": result.Draft, "idempotent": result.Idempotent})
}

func (s *Server) runAgent(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		ConversationID *string `json:"conversation_id"`
		MessageID      *string `json:"message_id"`
		Prompt         string  `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	svc, err := s.serviceForClaims(r.Context(), claims, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	target := svc.MailboxID
	if in.MessageID != nil && strings.TrimSpace(*in.MessageID) != "" {
		target = strings.TrimSpace(*in.MessageID)
	}
	actor, err := s.verifyActorEnvelope(r.Context(), r.Header, svc.MailboxID, "send", "run_agent", target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	result, err := s.runAgentOp(r.Context(), claims, runAgentParams{
		MailboxName:    svc.MailboxName,
		ConversationID: in.ConversationID,
		MessageID:      in.MessageID,
		Prompt:         in.Prompt,
		Actor:          actor,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, result)
}

func (s *Server) listAgentConversations(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	var messageID *string
	if v := strings.TrimSpace(r.URL.Query().Get("message_id")); v != "" {
		messageID = &v
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	items, err := s.listAgentConversationsOp(r.Context(), claims, listAgentConversationsParams{
		MailboxName: s.requestedMailboxName(r), SourceMessageID: messageID, Limit: limit,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, items)
}

func (s *Server) getAgentConversation(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	item, err := s.getAgentConversationOp(r.Context(), claims, draftIDParams{
		MailboxName: s.requestedMailboxName(r),
		DraftID:     r.URL.Query().Get("id"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, item)
}

func (s *Server) mailboxAgentSettings(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	switch r.Method {
	case http.MethodGet:
		if claims.AliasID != nil || !claims.IsAdmin() {
			writeErr(w, store.ErrUnauthorized)
			return
		}
		svc, err := s.serviceForClaims(r.Context(), claims, r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if _, err := s.verifyActorEnvelope(r.Context(), r.Header, svc.MailboxID, "admin", "read_mailbox_agent_settings", svc.MailboxID); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		settings, err := svc.GetMailboxAgentSettings(r.Context(), claims)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, settings)
	case http.MethodPost:
		if claims.AliasID != nil {
			writeErr(w, store.ErrUnauthorized)
			return
		}
		svc, err := s.serviceForClaims(r.Context(), claims, r)
		if err != nil {
			writeErr(w, err)
			return
		}
		actor, err := s.verifyActorEnvelope(r.Context(), r.Header, svc.MailboxID, "admin", "update_mailbox_agent_settings", svc.MailboxID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		var in struct {
			SystemPrompt     string   `json:"system_prompt"`
			EnabledTools     []string `json:"enabled_tools"`
			ModelOverride    *string  `json:"model_override"`
			AutoDraftEnabled bool     `json:"auto_draft_enabled"`
			MaxToolSteps     int      `json:"max_tool_steps"`
			ToolTimeoutSec   int      `json:"tool_timeout_sec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, err)
			return
		}
		settings, err := s.updateMailboxAgentSettingsOp(r.Context(), claims, svc.MailboxName, store.UpdateMailboxAgentSettingsInput{
			SystemPrompt:     in.SystemPrompt,
			EnabledTools:     in.EnabledTools,
			ModelOverride:    in.ModelOverride,
			AutoDraftEnabled: in.AutoDraftEnabled,
			MaxToolSteps:     in.MaxToolSteps,
			ToolTimeoutSec:   in.ToolTimeoutSec,
			Actor:            actor,
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, settings)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) mailboxAgentContext(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	context, err := s.getMailboxAgentContextOp(r.Context(), claims, s.requestedMailboxName(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, context)
}

func (s *Server) sendRaw(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		From           string   `json:"from"`
		To             []string `json:"to"`
		RawBase64      string   `json:"raw_base64"`
		IdempotencyKey string   `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	raw, err := decodeRawBase64(in.RawBase64)
	if err != nil {
		writeErr(w, fmt.Errorf("decode raw_base64: %w", err))
		return
	}
	job, err := s.sendRawMessageOp(r.Context(), claims, sendRawMessageParams{
		MailboxName:    s.requestedMailboxName(r),
		From:           in.From,
		To:             in.To,
		RawRFC822:      raw,
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "queued", "job_id": job.ID})
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		MessageID      string `json:"message_id"`
		Body           string `json:"body"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, err)
		return
	}
	job, err := s.replyMessageOp(r.Context(), claims, replyMessageParams{
		MailboxName:    s.requestedMailboxName(r),
		MessageID:      in.MessageID,
		Body:           in.Body,
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "queued", "job_id": job.ID})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	items, err := s.searchMessagesOp(r.Context(), claims, s.requestedMailboxName(r), r.URL.Query().Get("q"), r.URL.Query().Get("folder"), atoiDefault(r.URL.Query().Get("limit")))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, items)
}

func (s *Server) searchStructured(w http.ResponseWriter, r *http.Request, claims model.TokenClaims) {
	var req model.SearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	s.searchWithRequest(w, r, claims, req)
}

func (s *Server) searchWithRequest(w http.ResponseWriter, r *http.Request, claims model.TokenClaims, req model.SearchRequest) {
	items, err := s.searchStructuredOp(r.Context(), claims, s.requestedMailboxName(r), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, items)
}

func atoiDefault(v string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, store.ErrUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrAgentRunConflict):
		status = http.StatusConflict
	case errors.Is(err, store.ErrAgentCredentialExists):
		status = http.StatusConflict
	}
	http.Error(w, err.Error(), status)
}

func summarizeSearchRequest(req model.SearchRequest, resultCount int) string {
	parts := []string{fmt.Sprintf("mode=%s", req.NormalizedMode())}
	if parser := strings.TrimSpace(req.ParserMode); parser != "" {
		parts = append(parts, "parser="+parser)
	}
	if folder := strings.TrimSpace(req.Folder); folder != "" {
		parts = append(parts, "folder="+folder)
	}
	if req.Filters.Date.InternalSince != nil {
		parts = append(parts, "since="+req.Filters.Date.InternalSince.Format("2006-01-02"))
	}
	if req.Filters.Date.InternalBefore != nil {
		parts = append(parts, "before="+req.Filters.Date.InternalBefore.Format("2006-01-02"))
	}
	if len(req.Filters.Flags.With) > 0 {
		parts = append(parts, "flags+"+strings.Join(req.Filters.Flags.With, ","))
	}
	if len(req.Filters.Flags.Without) > 0 {
		parts = append(parts, "flags-"+strings.Join(req.Filters.Flags.Without, ","))
	}
	if len(req.Filters.Headers.Subject) > 0 {
		parts = append(parts, "subject="+strings.Join(req.Filters.Headers.Subject, "|"))
	}
	if len(req.Filters.Headers.From) > 0 {
		parts = append(parts, "from="+strings.Join(req.Filters.Headers.From, "|"))
	}
	if len(req.Filters.Headers.To) > 0 {
		parts = append(parts, "to="+strings.Join(req.Filters.Headers.To, "|"))
	}
	text := strings.TrimSpace(req.Intent.Text)
	if text != "" {
		parts = append(parts, fmt.Sprintf("text=%q", text))
	}
	for _, term := range req.Intent.BodyTerms {
		if trimmed := strings.TrimSpace(term); trimmed != "" {
			parts = append(parts, "body="+trimmed)
		}
	}
	parts = append(parts, fmt.Sprintf("hits=%d", resultCount))
	if structured := compactSearchRequest(req); structured != "" {
		parts = append(parts, "request="+structured)
	}
	return strings.Join(parts, " ")
}

func compactSearchRequest(req model.SearchRequest) string {
	buf, err := json.Marshal(req)
	if err != nil {
		return ""
	}
	return string(buf)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
