package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/mail"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/oreoluwa/agent-mailboxes/internal/attachanalyze"
	"github.com/oreoluwa/agent-mailboxes/internal/auth"
	"github.com/oreoluwa/agent-mailboxes/internal/embed"
	"github.com/oreoluwa/agent-mailboxes/internal/imapingest"
	"github.com/oreoluwa/agent-mailboxes/internal/mimeparse"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/router"
	"github.com/oreoluwa/agent-mailboxes/internal/searchparse"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
	"github.com/oreoluwa/agent-mailboxes/internal/summarize"
)

type MailSender interface {
	Send(from string, to []string, subject string, body string, inReplyTo *string, references *string) error
	SendRaw(from string, to []string, raw []byte) error
}

type AttachmentStore interface {
	Put(ctx context.Context, key string, content []byte, contentType string) (string, error)
}

type MailboxEvent struct {
	MailboxID   string
	MailboxName string
	// OwningAliasID identifies events about an alias-owned resource, such as a
	// draft or outbound delivery. Audience identifies all aliases allowed to
	// observe a shared message event. Events with neither are admin-only.
	OwningAliasID string
	Audience      []string
	EventType     string
	MessageID     string
	OccurredAt    time.Time
	Cursor        string
	Subject       string
}

type EventPublisher interface {
	Publish(MailboxEvent)
}

type FolderFetcher interface {
	FetchSinceFolder(folder string, lastUID uint32) ([]imapingest.Message, uint32, uint32, error)
}

type CheckpointFetcher interface {
	FetchSinceCheckpoint(folder string, uidValidity, lastUID uint32) ([]imapingest.Message, uint32, uint32, error)
}

type FolderIdleFetcher interface {
	WaitForChangeFolder(ctx context.Context, folder string, timeout time.Duration) (bool, error)
}

type FolderLister interface {
	ListFolders() ([]string, error)
}

type Service struct {
	Store                  store.Store
	Summarizer             summarize.Summarizer
	Embedder               embed.Embedder
	SummarizeWorkers       int
	EmbeddingBatchSize     int
	EmbeddingFlushInterval time.Duration
	AttachmentAnalyzer     attachanalyze.Analyzer
	Fetcher                imapingest.Fetcher
	Sender                 MailSender
	AgentPlanner           AgentPlanner
	AgentRunTimeout        time.Duration
	SearchParser           searchparse.Parser
	SearchDefaultTimezone  string
	Attachments            AttachmentStore
	MailboxID              string
	MailboxName            string
	SyncFolders            []string
	CatchallAdmin          bool
	ThreadContextMode      string
	ThreadContextMaxChars  int
	Events                 EventPublisher
	autoDraftLease         time.Duration
	autoDraftRenewInterval time.Duration
	autoDraftRenewTimeout  time.Duration
	embeddingDrainMu       sync.Mutex
	embeddingLastDrain     time.Time
	queueWorkersOnce       sync.Once
}

type summarizeRetryPayload struct {
	Input summarize.Input `json:"input"`
}

func (s *Service) Authenticate(ctx context.Context, bearer string, masterKeys []string) (model.TokenClaims, error) {
	tok, err := auth.ParseBearer(bearer)
	if err != nil {
		return model.TokenClaims{}, err
	}
	for _, key := range masterKeys {
		h := auth.HashToken(key, tok)
		claims, err := s.Store.AuthenticateToken(ctx, h)
		if err == nil {
			return claims, nil
		}
		if !errors.Is(err, store.ErrUnauthorized) {
			return model.TokenClaims{}, err
		}
	}
	return model.TokenClaims{}, store.ErrUnauthorized
}

func (s *Service) SyncOnce(ctx context.Context) error {
	aliases, err := s.Store.ListAliases(ctx, s.MailboxID)
	if err != nil {
		return err
	}
	folders := s.foldersForSync()
	var firstErr error
	for _, folder := range folders {
		if err := s.syncFolder(ctx, folder, aliases); err != nil {
			log.Printf("sync folder=%s error: %v", folder, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *Service) syncFolder(ctx context.Context, folder string, aliases []router.Alias) error {
	uidv, last, folderID, err := s.Store.GetFolderCheckpoint(ctx, s.MailboxID, folder)
	if err != nil {
		return err
	}
	var msgs []imapingest.Message
	var fetchedUIDV, newLast uint32
	checkpointFetcher, checkpointAware := s.Fetcher.(CheckpointFetcher)
	if checkpointAware {
		msgs, fetchedUIDV, newLast, err = checkpointFetcher.FetchSinceCheckpoint(folder, uidv, last)
	} else {
		msgs, fetchedUIDV, newLast, err = s.fetchSince(folder, last)
	}
	if err != nil {
		return err
	}
	if !checkpointAware && uidv != 0 && fetchedUIDV != 0 && fetchedUIDV != uidv {
		// UIDVALIDITY changed (mailbox reset/rotated), so restart from UID 1.
		msgs, fetchedUIDV, newLast, err = s.fetchSince(folder, 0)
		if err != nil {
			return err
		}
	}
	if folderID == "" || (checkpointAware && uidv != fetchedUIDV) {
		baseline := last
		if checkpointAware {
			// Persist the chosen history floor before processing. A failed insert
			// must not turn the next attempt into an unbounded history import.
			baseline = newLast
			for _, msg := range msgs {
				if msg.UID > 0 && msg.UID-1 < baseline {
					baseline = msg.UID - 1
				}
			}
		}
		folderID, err = s.Store.UpsertFolderCheckpoint(ctx, s.MailboxID, folder, fetchedUIDV, baseline)
		if err != nil {
			return err
		}
	}
	for _, rm := range msgs {
		parsed, err := mimeparse.ParseRawEmail(rm.Raw, rm.UID, rm.InternalDate)
		if err != nil {
			log.Printf("parse uid=%d: %v", rm.UID, err)
			continue
		}
		parsed.IMAPUIDValidity = int64(fetchedUIDV)
		seenRecipients := make([]string, 0, len(parsed.Recipients))
		for _, r := range parsed.Recipients {
			seenRecipients = append(seenRecipients, strings.ToLower(r.Email))
		}
		audience := router.ResolveAudience(seenRecipients, parsed.RawHeaders, aliases, s.CatchallAdmin)
		if len(audience) == 0 {
			log.Printf("skip uid=%d no audience", rm.UID)
			continue
		}
		msgID, inserted, err := s.Store.InsertMessage(ctx, s.MailboxID, folderID, parsed, audience)
		if err != nil {
			return err
		}
		if !inserted {
			// Message already exists; do not re-run attachment analysis/summarization/embedding queueing.
			continue
		}
		s.publishAudienceEvent("message_added", msgID, deref(parsed.Subject), audience)
		s.uploadAttachments(ctx, msgID, rm.UID, parsed.Attachments)
		attachNames := []string{}
		for _, a := range parsed.Attachments {
			attachNames = append(attachNames, a.Filename)
		}
		attachmentSummaries := []summarize.AttachmentSummary{}
		attachmentIssues := []string{}
		if s.Summarizer != nil && s.AttachmentAnalyzer != nil && len(parsed.Attachments) > 0 {
			analysis, err := s.AttachmentAnalyzer.Analyze(ctx, attachanalyze.Input{Attachments: parsed.Attachments})
			if err != nil {
				log.Printf("attachment analysis failed msg=%s uid=%d: %v", msgID, rm.UID, err)
				attachmentIssues = append(attachmentIssues, fmt.Sprintf("attachment analysis failed: %v", err))
			} else {
				attachmentIssues = append(attachmentIssues, analysis.Issues...)
				for _, item := range analysis.Summaries {
					attachmentSummaries = append(attachmentSummaries, summarize.AttachmentSummary{
						Filename:   item.Filename,
						MIME:       item.MIME,
						Source:     item.Source,
						Summary:    item.Summary,
						Confidence: item.Confidence,
					})
				}
			}
		}
		if s.Summarizer != nil {
			threadCtx := []model.ThreadContextMessage{}
			if strings.ToLower(strings.TrimSpace(s.ThreadContextMode)) != "none" {
				mailboxID := s.MailboxID
				threadCtx, err = s.Store.GetThreadContextForMessage(ctx, model.TokenClaims{MailboxID: &mailboxID}, s.MailboxID, msgID)
				if err != nil {
					return fmt.Errorf("fetch thread context %s: %w", msgID, err)
				}
				threadCtx = capThreadContextBody(threadCtx, s.ThreadContextMaxChars)
			}
			input := summarize.Input{
				Subject:             deref(parsed.Subject),
				FromName:            deref(parsed.FromName),
				From:                parsed.FromEmail,
				Text:                parsed.ExtractedText,
				Attachments:         attachNames,
				AttachmentSummaries: attachmentSummaries,
				AttachmentIssues:    attachmentIssues,
				Thread:              threadCtx,
			}
			// Queue summarization and continue scanning, so slow/failed summarizer calls
			// never block mailbox checkpoint progression.
			if err := s.queueSummarizeRetry(ctx, msgID, input, nil); err != nil {
				log.Printf("queue summarize job failed msg=%s uid=%d: %v", msgID, rm.UID, err)
				return err
			}
		}
	}
	_, err = s.Store.UpsertFolderCheckpoint(ctx, s.MailboxID, folder, fetchedUIDV, newLast)
	return err
}

func (s *Service) RunPoller(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	s.StartQueueWorkers(ctx)
	const maxLinearRetry = 10 * time.Minute
	idler, useIdle := s.Fetcher.(imapingest.IdleFetcher)
	credentialFailures := 0
	for {
		folders := s.foldersForSync()
		if err := s.SyncOnce(ctx); err != nil {
			if isCredentialAuthError(err) {
				credentialFailures++
				delay := linearRetryDelay(interval, credentialFailures, maxLinearRetry)
				log.Printf("poller paused mailbox=%s auth failure=%d retry_in=%s err=%v", s.MailboxID, credentialFailures, delay, err)
				if !waitOrDone(ctx, delay) {
					return
				}
				continue
			}
			log.Printf("sync error mailbox=%s: %v", s.MailboxID, err)
		} else {
			credentialFailures = 0
		}
		if ctx.Err() != nil {
			return
		}
		if useIdle && len(folders) == 1 {
			changed, err := s.waitForChange(ctx, idler, folders[0], interval)
			if err != nil && ctx.Err() == nil {
				if isCredentialAuthError(err) {
					credentialFailures++
					delay := linearRetryDelay(interval, credentialFailures, maxLinearRetry)
					log.Printf("poller paused mailbox=%s idle auth failure=%d retry_in=%s err=%v", s.MailboxID, credentialFailures, delay, err)
					if !waitOrDone(ctx, delay) {
						return
					}
					continue
				}
				log.Printf("idle wait error mailbox=%s: %v", s.MailboxID, err)
				if !waitOrDone(ctx, 5*time.Second) {
					return
				}
			}
			if err == nil {
				credentialFailures = 0
			}
			if changed {
				continue
			}
			continue
		}
		if !waitOrDone(ctx, interval) {
			return
		}
	}
}

func (s *Service) StartQueueWorkers(ctx context.Context) {
	s.queueWorkersOnce.Do(func() {
		s.RunQueueWorkers(ctx, 2*time.Second, 10*time.Second, 2*time.Second)
	})
}

func (s *Service) RunQueueWorkers(ctx context.Context, summarizeInterval, embeddingInterval, outboundInterval time.Duration) {
	go s.runSummarizeWorker(ctx, summarizeInterval)
	go s.runEmbeddingWorker(ctx, embeddingInterval)
	go s.runOutboundWorker(ctx, outboundInterval)
	go s.runAutoDraftWorker(ctx, 5*time.Second)
}

func (s *Service) runSummarizeWorker(ctx context.Context, interval time.Duration) {
	if s.Summarizer == nil {
		return
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	workers := s.effectiveSummarizeWorkerCount()
	perWorkerMaxJobs := int(math.Ceil(64.0 / float64(workers)))
	for i := 0; i < workers; i++ {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				if err := s.flushSummarizeQueue(ctx, perWorkerMaxJobs); err != nil && ctx.Err() == nil {
					log.Printf("summarize worker flush failed mailbox=%s: %v", s.MailboxID, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	<-ctx.Done()
}

func (s *Service) runEmbeddingWorker(ctx context.Context, interval time.Duration) {
	if s.Embedder == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.flushEmbeddingQueueIfDue(ctx, true); err != nil && ctx.Err() == nil {
			log.Printf("embedding worker flush failed mailbox=%s: %v", s.MailboxID, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) runOutboundWorker(ctx context.Context, interval time.Duration) {
	if s.Sender == nil {
		return
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.flushOutboundQueue(ctx, 16); err != nil && ctx.Err() == nil {
			log.Printf("outbound worker flush failed mailbox=%s: %v", s.MailboxID, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) fetchSince(folder string, lastUID uint32) ([]imapingest.Message, uint32, uint32, error) {
	if ff, ok := s.Fetcher.(FolderFetcher); ok {
		return ff.FetchSinceFolder(folder, lastUID)
	}
	return s.Fetcher.FetchSince(lastUID)
}

func (s *Service) waitForChange(ctx context.Context, idler imapingest.IdleFetcher, folder string, timeout time.Duration) (bool, error) {
	if ff, ok := s.Fetcher.(FolderIdleFetcher); ok {
		return ff.WaitForChangeFolder(ctx, folder, timeout)
	}
	return idler.WaitForChange(ctx, timeout)
}

func (s *Service) foldersForSync() []string {
	out := normalizeFolders(s.SyncFolders)
	if len(out) > 0 {
		return out
	}
	if fl, ok := s.Fetcher.(FolderLister); ok {
		if folders, err := fl.ListFolders(); err == nil {
			out = normalizeFolders(append(folders, "INBOX"))
			if len(out) > 0 {
				return out
			}
		} else {
			log.Printf("list folders error: %v", err)
		}
	}
	return []string{"INBOX"}
}

func normalizeFolders(folders []string) []string {
	out := make([]string, 0, len(folders)+1)
	seen := map[string]struct{}{}
	for _, folder := range folders {
		name := strings.TrimSpace(folder)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}
	return out
}

func (s *Service) MintToken(ctx context.Context, aliasEmail, name, masterKey string, scopes []string) (token string, tokenID string, err error) {
	aliasID, err := s.Store.GetAliasByEmail(ctx, s.MailboxID, aliasEmail)
	if err != nil {
		return "", "", err
	}
	raw, err := auth.GenerateToken()
	if err != nil {
		return "", "", err
	}
	h := auth.HashToken(masterKey, raw)
	id, err := s.Store.MintToken(ctx, &aliasID, name, h, scopes)
	if err != nil {
		return "", "", err
	}
	return raw, id, nil
}

func (s *Service) MintAdminToken(ctx context.Context, name, masterKey string) (token string, tokenID string, err error) {
	raw, err := auth.GenerateToken()
	if err != nil {
		return "", "", err
	}
	h := auth.HashToken(masterKey, raw)
	id, err := s.Store.MintToken(ctx, nil, name, h, []string{string(model.ScopeAdmin)})
	if err != nil {
		return "", "", err
	}
	return raw, id, nil
}

func (s *Service) Send(ctx context.Context, claims model.TokenClaims, to []string, subject, body string) error {
	_, err := s.SendWithIdempotency(ctx, claims, to, subject, body, "")
	return err
}

func (s *Service) SendWithIdempotency(ctx context.Context, claims model.TokenClaims, to []string, subject, body, idempotencyKey string) (store.OutboundMessage, error) {
	if !claims.Has(model.ScopeSend) && !claims.IsAdmin() {
		return store.OutboundMessage{}, store.ErrUnauthorized
	}
	if claims.AliasID == nil {
		return store.OutboundMessage{}, store.ErrUnauthorized
	}
	from, err := s.Store.GetAliasEmailByID(ctx, *claims.AliasID)
	if err != nil {
		return store.OutboundMessage{}, err
	}
	raw := buildSimpleRawMessage(from, to, subject, body, nil, nil)
	return s.enqueueOutbound(ctx, claims, from, to, raw, idempotencyKey)
}

func (s *Service) Reply(ctx context.Context, claims model.TokenClaims, messageID, body string) error {
	_, err := s.ReplyWithIdempotency(ctx, claims, messageID, body, "")
	return err
}

func (s *Service) ReplyWithIdempotency(ctx context.Context, claims model.TokenClaims, messageID, body, idempotencyKey string) (store.OutboundMessage, error) {
	if !claims.Has(model.ScopeSend) && !claims.IsAdmin() {
		return store.OutboundMessage{}, store.ErrUnauthorized
	}
	if claims.AliasID == nil {
		return store.OutboundMessage{}, store.ErrUnauthorized
	}
	from, err := s.Store.GetAliasEmailByID(ctx, *claims.AliasID)
	if err != nil {
		return store.OutboundMessage{}, err
	}
	inReplyTo, refs, subject, err := s.Store.GetMessageThreadMeta(ctx, claims, messageID)
	if err != nil {
		return store.OutboundMessage{}, err
	}
	replyTo, err := s.Store.GetMessageReplyTo(ctx, claims, messageID)
	if err != nil {
		return store.OutboundMessage{}, err
	}
	subj := "Re:"
	if subject != nil {
		subj = "Re: " + *subject
	}
	raw := buildSimpleRawMessage(from, []string{replyTo}, subj, body, inReplyTo, refs)
	return s.enqueueOutbound(ctx, claims, from, []string{replyTo}, raw, idempotencyKey)
}

func (s *Service) SendRaw(ctx context.Context, claims model.TokenClaims, from string, to []string, raw []byte) error {
	_, err := s.SendRawWithIdempotency(ctx, claims, from, to, raw, "")
	return err
}

func (s *Service) SendRawWithIdempotency(ctx context.Context, claims model.TokenClaims, from string, to []string, raw []byte, idempotencyKey string) (store.OutboundMessage, error) {
	if !claims.Has(model.ScopeSend) && !claims.IsAdmin() {
		return store.OutboundMessage{}, store.ErrUnauthorized
	}
	if claims.AliasID == nil {
		return store.OutboundMessage{}, store.ErrUnauthorized
	}
	aliasFrom, err := s.Store.GetAliasEmailByID(ctx, *claims.AliasID)
	if err != nil {
		return store.OutboundMessage{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(from), strings.TrimSpace(aliasFrom)) {
		return store.OutboundMessage{}, fmt.Errorf("from must match alias token")
	}
	return s.enqueueOutbound(ctx, claims, aliasFrom, to, raw, idempotencyKey)
}

func buildSimpleRawMessage(from string, to []string, subject string, body string, inReplyTo *string, references *string) []byte {
	headers := []string{
		fmt.Sprintf("From: %s", from),
		fmt.Sprintf("To: %s", strings.Join(to, ", ")),
		fmt.Sprintf("Subject: %s", subject),
		fmt.Sprintf("Date: %s", time.Now().Format(time.RFC1123Z)),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	}
	if inReplyTo != nil && strings.TrimSpace(*inReplyTo) != "" {
		headers = append(headers, "In-Reply-To: "+strings.TrimSpace(*inReplyTo))
	}
	if references != nil && strings.TrimSpace(*references) != "" {
		headers = append(headers, "References: "+strings.TrimSpace(*references))
	}
	return []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + body)
}

func makeIdempotencyKey(mailboxID, aliasID, from string, to []string, raw []byte, provided string) string {
	if strings.TrimSpace(provided) != "" {
		return strings.TrimSpace(provided)
	}
	sum := sha256.Sum256([]byte(mailboxID + "\n" + aliasID + "\n" + strings.ToLower(strings.TrimSpace(from)) + "\n" + strings.Join(to, "\n") + "\n" + string(raw)))
	return fmt.Sprintf("auto:%x", sum[:16])
}

func normalizeOutboundRecipients(to []string) []string {
	out := make([]string, 0, len(to))
	for _, item := range to {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func ensureRawSender(raw []byte, from string) error {
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return nil
	}
	got := strings.TrimSpace(msg.Header.Get("From"))
	if got == "" {
		return nil
	}
	if !strings.Contains(strings.ToLower(got), strings.ToLower(strings.TrimSpace(from))) {
		return fmt.Errorf("raw From header must match alias token")
	}
	return nil
}

func (s *Service) enqueueOutbound(ctx context.Context, claims model.TokenClaims, from string, to []string, raw []byte, idempotencyKey string) (store.OutboundMessage, error) {
	if claims.AliasID == nil {
		return store.OutboundMessage{}, store.ErrUnauthorized
	}
	recipients := normalizeOutboundRecipients(to)
	if len(recipients) == 0 {
		return store.OutboundMessage{}, fmt.Errorf("at least one recipient is required")
	}
	if len(raw) == 0 {
		return store.OutboundMessage{}, fmt.Errorf("raw message is required")
	}
	if err := ensureRawSender(raw, from); err != nil {
		return store.OutboundMessage{}, err
	}
	key := makeIdempotencyKey(s.MailboxID, *claims.AliasID, from, recipients, raw, idempotencyKey)
	if message, parseErr := mail.ReadMessage(strings.NewReader(string(raw))); parseErr == nil && strings.TrimSpace(message.Header.Get("Message-ID")) == "" {
		raw = append([]byte("Message-ID: <"+uuid.NewString()+"@agent-mailboxes.invalid>\r\n"), raw...)
	}
	job, _, err := s.Store.QueueOutboundMessage(ctx, s.MailboxID, *claims.AliasID, from, recipients, raw, key)
	if err == nil {
		s.publishAliasEvent("outbound_queued", job.ID, "", *claims.AliasID)
	}
	return job, err
}

func (s *Service) publishEvent(eventType, messageID, subject string) {
	s.publishScopedEvent(eventType, messageID, subject, "", nil)
}

func (s *Service) publishAliasEvent(eventType, messageID, subject, aliasID string) {
	s.publishScopedEvent(eventType, messageID, subject, aliasID, nil)
}

func (s *Service) publishAudienceEvent(eventType, messageID, subject string, audience []string) {
	s.publishScopedEvent(eventType, messageID, subject, "", audience)
}

func (s *Service) publishScopedEvent(eventType, messageID, subject, owningAliasID string, audience []string) {
	if s.Events == nil {
		return
	}
	occurredAt := time.Now().UTC()
	cursor := strconv.FormatInt(occurredAt.UnixNano(), 10)
	s.Events.Publish(MailboxEvent{
		MailboxID:     s.MailboxID,
		MailboxName:   s.MailboxName,
		OwningAliasID: strings.TrimSpace(owningAliasID),
		Audience:      append([]string(nil), audience...),
		EventType:     eventType,
		MessageID:     strings.TrimSpace(messageID),
		OccurredAt:    occurredAt,
		Cursor:        cursor,
		Subject:       strings.TrimSpace(subject),
	})
}

func (s *Service) Search(ctx context.Context, claims model.TokenClaims, query, folder string, limit int) ([]model.ListItem, error) {
	req := model.SearchRequest{
		Folder: folder,
		Limit:  limit,
		Intent: model.SearchIntent{
			Text: strings.TrimSpace(query),
		},
		Mode: model.SearchModeHybrid,
	}
	return s.SearchStructured(ctx, claims, req)
}

func (s *Service) SearchStructured(ctx context.Context, claims model.TokenClaims, req model.SearchRequest) ([]model.ListItem, error) {
	req = s.normalizeStructuredSearch(ctx, req)
	broadReq, useStrictBoost := softenParserDerivedHybridRequest(req)
	intentText := searchIntentText(req.Intent)
	recencyIntent := hasRecencyIntent(intentText)
	mode := req.NormalizedMode()

	if isRecencyOnlyQuery(intentText) {
		req.Intent.Text = ""
		intentText = searchIntentText(req.Intent)
	}
	backendLimit := req.Limit
	if backendLimit <= 0 {
		backendLimit = 50
	}
	if recencyIntent && intentText != "" {
		backendLimit = max(backendLimit*3, 50)
		if backendLimit > 200 {
			backendLimit = 200
		}
	}
	req.Limit = backendLimit

	if !hasSearchIntent(req.Intent) {
		items, err := s.Store.SearchStructured(ctx, claims, req)
		if err != nil {
			return nil, err
		}
		return applyRecencyAndLimit(items, true, backendLimit), nil
	}

	var keyword []model.ListItem
	var err error
	if mode != model.SearchModeSemantic {
		keywordReq := req
		if useStrictBoost {
			keywordReq = broadReq
		}
		keyword, err = s.Store.SearchStructured(ctx, claims, keywordReq)
		if err != nil {
			return nil, err
		}
	}
	if s.Embedder == nil || mode == model.SearchModeKeyword || intentText == "" {
		if useStrictBoost {
			strictKeyword, err := s.Store.SearchStructured(ctx, claims, req)
			if err != nil {
				return nil, err
			}
			keyword = fuseSearchResults(strictKeyword, keyword, backendLimit)
		}
		return applyRecencyAndLimit(keyword, recencyIntent, req.Limit), nil
	}
	embeddings, err := s.Embedder.Embed(ctx, []string{intentText})
	if err != nil {
		log.Printf("embedding query failed: %v", err)
		if useStrictBoost {
			strictKeyword, strictErr := s.Store.SearchStructured(ctx, claims, req)
			if strictErr != nil {
				return nil, strictErr
			}
			keyword = fuseSearchResults(strictKeyword, keyword, backendLimit)
		}
		return applyRecencyAndLimit(keyword, recencyIntent, req.Limit), nil
	}
	semanticReq := req
	if useStrictBoost {
		semanticReq = broadReq
	}
	semantic, err := s.Store.SearchStructuredByEmbedding(ctx, claims, semanticReq, embeddings[0])
	if err != nil {
		log.Printf("semantic search failed: %v", err)
		if useStrictBoost {
			strictKeyword, strictErr := s.Store.SearchStructured(ctx, claims, req)
			if strictErr != nil {
				return nil, strictErr
			}
			keyword = fuseSearchResults(strictKeyword, keyword, backendLimit)
		}
		return applyRecencyAndLimit(keyword, recencyIntent, req.Limit), nil
	}
	if mode == model.SearchModeSemantic {
		return applyRecencyAndLimit(semantic, recencyIntent, req.Limit), nil
	}
	fused := fuseSearchResults(keyword, semantic, backendLimit)
	if useStrictBoost {
		strictKeyword, err := s.Store.SearchStructured(ctx, claims, req)
		if err != nil {
			return nil, err
		}
		fused = fuseSearchResults(strictKeyword, fused, backendLimit)
	}
	return applyRecencyAndLimit(fused, recencyIntent, req.Limit), nil
}

func (s *Service) NormalizeSearchRequest(ctx context.Context, req model.SearchRequest) model.SearchRequest {
	return s.normalizeStructuredSearch(ctx, req)
}

func (s *Service) normalizeStructuredSearch(ctx context.Context, req model.SearchRequest) model.SearchRequest {
	req.Folder = strings.TrimSpace(req.Folder)
	req.Intent.Text = strings.TrimSpace(req.Intent.Text)
	if req.Folder == "" {
		var inferred string
		inferred, req.Intent.Text = inferFolderFromQuery(req.Intent.Text)
		req.Folder = inferred
	}
	if req.Intent.Text != "" && s.SearchParser != nil {
		if parsed, err := s.SearchParser.Parse(ctx, req.Intent.Text); err == nil {
			req = mergeSearchRequests(req, parsed)
			req.ParserMode = "llm"
		} else {
			log.Printf("search parser fallback err=%v", err)
			req = parseNaturalLanguageSearch(req)
			req.ParserMode = "fallback"
		}
	} else {
		before := strings.TrimSpace(req.Intent.Text)
		req = parseNaturalLanguageSearch(req)
		if before != "" {
			req.ParserMode = "fallback"
		} else {
			req.ParserMode = "none"
		}
	}
	if req.Limit <= 0 {
		req.Limit = 50
	}
	req = s.resolveTemporalDateFilters(req)
	req.Mode = req.NormalizedMode()
	return req
}

func parseNaturalLanguageSearch(req model.SearchRequest) model.SearchRequest {
	text := strings.TrimSpace(req.Intent.Text)
	if text == "" {
		return req
	}
	lower := strings.ToLower(text)
	remaining := text

	for _, phrase := range []string{
		"i have not seen", "i've not seen", "i havent seen", "i haven't seen",
		"not seen", "unseen", "unread", "not read",
	} {
		if strings.Contains(lower, phrase) {
			req.Filters.Flags.Without = appendUniqueFold(req.Filters.Flags.Without, "seen")
			remaining = removePhraseInsensitive(remaining, phrase)
			lower = strings.ToLower(remaining)
		}
	}
	for _, phrase := range []string{"seen", "read"} {
		if strings.Contains(lower, phrase) {
			req.Filters.Flags.With = appendUniqueFold(req.Filters.Flags.With, "seen")
			remaining = removePhraseInsensitive(remaining, phrase)
			lower = strings.ToLower(remaining)
			break
		}
	}
	for _, phrase := range []string{
		"has not been responded to", "have not been responded to", "not been responded to",
		"has not been replied to", "have not been replied to", "not replied to",
		"without reply", "unanswered", "not answered",
	} {
		if strings.Contains(lower, phrase) {
			req.Filters.Flags.Without = appendUniqueFold(req.Filters.Flags.Without, "answered")
			remaining = removePhraseInsensitive(remaining, phrase)
			lower = strings.ToLower(remaining)
			break
		}
	}
	for _, phrase := range []string{"answered", "replied"} {
		if strings.Contains(lower, phrase) {
			req.Filters.Flags.With = appendUniqueFold(req.Filters.Flags.With, "answered")
			remaining = removePhraseInsensitive(remaining, phrase)
			lower = strings.ToLower(remaining)
			break
		}
	}

	fromRe := regexp.MustCompile(`(?i)\bfrom\s+([^\s,;]+@[^\s,;]+)\b`)
	for _, match := range fromRe.FindAllStringSubmatch(remaining, -1) {
		if len(match) < 2 {
			continue
		}
		addr := strings.ToLower(strings.TrimSpace(match[1]))
		req.Filters.Headers.From = appendUniqueFold(req.Filters.Headers.From, addr)
		req.Intent.HeaderTerms.From = appendUniqueFold(req.Intent.HeaderTerms.From, addr)
		remaining = removePhraseInsensitive(remaining, match[0])
	}
	lower = strings.ToLower(remaining)

	if t, matched := parseRelativeDate(lower, "since yesterday", -24*time.Hour); matched {
		req.Filters.Date.InternalSince = &t
		remaining = removePhraseInsensitive(remaining, "since yesterday")
	}
	if t, matched := parseRelativeDate(lower, "since today", 0); matched {
		req.Filters.Date.InternalSince = &t
		remaining = removePhraseInsensitive(remaining, "since today")
	}
	if t, matched := parseRelativeDate(lower, "before today", 0); matched {
		req.Filters.Date.InternalBefore = &t
		remaining = removePhraseInsensitive(remaining, "before today")
	}
	if t, matched := parseRelativeDate(lower, "before yesterday", -24*time.Hour); matched {
		req.Filters.Date.InternalBefore = &t
		remaining = removePhraseInsensitive(remaining, "before yesterday")
	}
	remaining, req = extractRelativeWindow(remaining, req)

	remaining = cleanupSearchRemainder(remaining)
	req.Intent.Text = remaining
	return req
}

func parseRelativeDate(lowerText, phrase string, offset time.Duration) (time.Time, bool) {
	if !strings.Contains(lowerText, phrase) {
		return time.Time{}, false
	}
	now := time.Now().UTC()
	base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(offset)
	return base, true
}

func extractRelativeWindow(text string, req model.SearchRequest) (string, model.SearchRequest) {
	type pattern struct {
		re    *regexp.Regexp
		apply func(int, model.SearchRequest) model.SearchRequest
	}
	patterns := []pattern{
		{
			re: regexp.MustCompile(`(?i)\bsince last (\d+) days?\b`),
			apply: func(n int, req model.SearchRequest) model.SearchRequest {
				t := midnightUTC().AddDate(0, 0, -n)
				req.Filters.Date.InternalSince = &t
				return req
			},
		},
		{
			re: regexp.MustCompile(`(?i)\bsince last (\d+) weeks?\b`),
			apply: func(n int, req model.SearchRequest) model.SearchRequest {
				t := midnightUTC().AddDate(0, 0, -(7 * n))
				req.Filters.Date.InternalSince = &t
				return req
			},
		},
		{
			re: regexp.MustCompile(`(?i)\b(last|past) (\d+) days?\b`),
			apply: func(n int, req model.SearchRequest) model.SearchRequest {
				t := midnightUTC().AddDate(0, 0, -n)
				req.Filters.Date.InternalSince = &t
				return req
			},
		},
		{
			re: regexp.MustCompile(`(?i)\b(last|past) (\d+) weeks?\b`),
			apply: func(n int, req model.SearchRequest) model.SearchRequest {
				t := midnightUTC().AddDate(0, 0, -(7 * n))
				req.Filters.Date.InternalSince = &t
				return req
			},
		},
	}
	for _, p := range patterns {
		matches := p.re.FindAllStringSubmatch(text, -1)
		for _, match := range matches {
			if len(match) < 2 {
				continue
			}
			numIdx := len(match) - 1
			n, err := strconv.Atoi(match[numIdx])
			if err != nil || n <= 0 {
				continue
			}
			req = p.apply(n, req)
			text = strings.Replace(text, match[0], " ", 1)
		}
	}
	return text, req
}

func midnightUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

func removePhraseInsensitive(s, phrase string) string {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(phrase) + `\b`)
	return re.ReplaceAllString(s, " ")
}

func cleanupSearchRemainder(s string) string {
	replacer := strings.NewReplacer(
		"my", " ",
		"my mails", " ",
		"my mail", " ",
		"my emails", " ",
		"my email", " ",
		"emails", " ",
		"email", " ",
		"mails", " ",
		"mail", " ",
		"that i've", " ",
		"that i have", " ",
		"that", " ",
		"which", " ",
		"and", " ",
		"has", " ",
		"have", " ",
		"been", " ",
	)
	s = replacer.Replace(strings.ToLower(s))
	return strings.Join(strings.Fields(s), " ")
}

func appendUniqueFold(items []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return items
	}
	for _, item := range items {
		if strings.EqualFold(item, value) {
			return items
		}
	}
	return append(items, value)
}

func mergeSearchRequests(base, parsed model.SearchRequest) model.SearchRequest {
	if strings.TrimSpace(base.Folder) == "" && strings.TrimSpace(parsed.Folder) != "" {
		base.Folder = parsed.Folder
	}
	if base.Limit <= 0 && parsed.Limit > 0 {
		base.Limit = parsed.Limit
	}
	base.Filters.Headers.Subject = mergeStringSlices(base.Filters.Headers.Subject, parsed.Filters.Headers.Subject)
	base.Filters.Headers.From = mergeStringSlices(base.Filters.Headers.From, parsed.Filters.Headers.From)
	base.Filters.Headers.To = mergeStringSlices(base.Filters.Headers.To, parsed.Filters.Headers.To)
	base.Filters.Headers.MessageID = mergeStringSlices(base.Filters.Headers.MessageID, parsed.Filters.Headers.MessageID)
	if base.Filters.Headers.Generic == nil && len(parsed.Filters.Headers.Generic) > 0 {
		base.Filters.Headers.Generic = map[string][]string{}
	}
	for k, vals := range parsed.Filters.Headers.Generic {
		base.Filters.Headers.Generic[k] = mergeStringSlices(base.Filters.Headers.Generic[k], vals)
	}
	base.Filters.Flags.With = mergeStringSlices(base.Filters.Flags.With, parsed.Filters.Flags.With)
	base.Filters.Flags.Without = mergeStringSlices(base.Filters.Flags.Without, parsed.Filters.Flags.Without)
	if base.Filters.Date.InternalSince == nil && parsed.Filters.Date.InternalSince != nil {
		base.Filters.Date.InternalSince = parsed.Filters.Date.InternalSince
	}
	if base.Filters.Date.InternalBefore == nil && parsed.Filters.Date.InternalBefore != nil {
		base.Filters.Date.InternalBefore = parsed.Filters.Date.InternalBefore
	}
	if base.Filters.Date.SentSince == nil && parsed.Filters.Date.SentSince != nil {
		base.Filters.Date.SentSince = parsed.Filters.Date.SentSince
	}
	if base.Filters.Date.SentBefore == nil && parsed.Filters.Date.SentBefore != nil {
		base.Filters.Date.SentBefore = parsed.Filters.Date.SentBefore
	}
	if strings.TrimSpace(base.Filters.Date.Field) == "" && strings.TrimSpace(parsed.Filters.Date.Field) != "" {
		base.Filters.Date.Field = strings.TrimSpace(parsed.Filters.Date.Field)
	}
	if base.Filters.Date.Range == nil && parsed.Filters.Date.Range != nil {
		tmp := *parsed.Filters.Date.Range
		base.Filters.Date.Range = &tmp
	}
	if base.Filters.Date.Temporal == nil && parsed.Filters.Date.Temporal != nil {
		tmp := *parsed.Filters.Date.Temporal
		base.Filters.Date.Temporal = &tmp
	}
	if base.Filters.Size.Larger == nil && parsed.Filters.Size.Larger != nil {
		base.Filters.Size.Larger = parsed.Filters.Size.Larger
	}
	if base.Filters.Size.Smaller == nil && parsed.Filters.Size.Smaller != nil {
		base.Filters.Size.Smaller = parsed.Filters.Size.Smaller
	}
	base.Filters.Logic.Not = append(base.Filters.Logic.Not, parsed.Filters.Logic.Not...)
	base.Filters.Logic.Or = append(base.Filters.Logic.Or, parsed.Filters.Logic.Or...)
	if strings.TrimSpace(parsed.Intent.Text) != "" {
		base.Intent.Text = strings.TrimSpace(parsed.Intent.Text)
	}
	base.Intent.BodyTerms = mergeStringSlices(base.Intent.BodyTerms, parsed.Intent.BodyTerms)
	base.Intent.HeaderTerms.Subject = mergeStringSlices(base.Intent.HeaderTerms.Subject, parsed.Intent.HeaderTerms.Subject)
	base.Intent.HeaderTerms.From = mergeStringSlices(base.Intent.HeaderTerms.From, parsed.Intent.HeaderTerms.From)
	base.Intent.HeaderTerms.To = mergeStringSlices(base.Intent.HeaderTerms.To, parsed.Intent.HeaderTerms.To)
	if base.Mode == "" && parsed.Mode != "" {
		base.Mode = parsed.Mode
	}
	return base
}

func mergeStringSlices(base, parsed []string) []string {
	out := append([]string(nil), base...)
	for _, v := range parsed {
		out = appendUniqueFold(out, v)
	}
	return out
}

func hasSearchIntent(intent model.SearchIntent) bool {
	if strings.TrimSpace(intent.Text) != "" {
		return true
	}
	if len(intent.BodyTerms) > 0 || len(intent.HeaderTerms.Subject) > 0 || len(intent.HeaderTerms.From) > 0 || len(intent.HeaderTerms.To) > 0 {
		return true
	}
	return false
}

func searchIntentText(intent model.SearchIntent) string {
	parts := []string{}
	if text := strings.TrimSpace(intent.Text); text != "" {
		parts = append(parts, text)
	}
	for _, term := range intent.BodyTerms {
		if trimmed := strings.TrimSpace(term); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	for _, term := range intent.HeaderTerms.Subject {
		if trimmed := strings.TrimSpace(term); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	for _, term := range intent.HeaderTerms.From {
		if trimmed := strings.TrimSpace(term); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	for _, term := range intent.HeaderTerms.To {
		if trimmed := strings.TrimSpace(term); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, " ")
}

func softenParserDerivedHybridRequest(req model.SearchRequest) (model.SearchRequest, bool) {
	if req.NormalizedMode() != model.SearchModeHybrid {
		return req, false
	}
	if req.ParserMode != "llm" && req.ParserMode != "fallback" {
		return req, false
	}
	if strings.TrimSpace(req.Intent.Text) == "" {
		return req, false
	}
	if len(req.Filters.Headers.Subject) == 0 && len(req.Intent.BodyTerms) == 0 && len(req.Intent.HeaderTerms.Subject) == 0 {
		return req, false
	}
	broad := req
	broad.Filters.Headers.Subject = nil
	broad.Intent.BodyTerms = nil
	broad.Intent.HeaderTerms.Subject = nil
	return broad, true
}

func (s *Service) resolveTemporalDateFilters(req model.SearchRequest) model.SearchRequest {
	if req.Filters.Date.Range == nil && req.Filters.Date.Temporal == nil {
		return req
	}
	loc := time.Local
	if tz := strings.TrimSpace(s.SearchDefaultTimezone); tz != "" {
		if loaded, err := time.LoadLocation(tz); err == nil {
			loc = loaded
		} else {
			log.Printf("search timezone load failed default_tz=%q err=%v", tz, err)
		}
	}
	field := strings.ToLower(strings.TrimSpace(req.Filters.Date.Field))
	if field == "" {
		if req.Filters.Date.Temporal != nil {
			field = strings.ToLower(strings.TrimSpace(req.Filters.Date.Temporal.Field))
		}
		if field == "" {
			field = "internal"
		}
	}
	now := time.Now().In(loc)
	assign := func(since, before time.Time) {
		sinceUTC := since.UTC()
		beforeUTC := before.UTC()
		if field == "sent" {
			req.Filters.Date.SentSince = &sinceUTC
			req.Filters.Date.SentBefore = &beforeUTC
			return
		}
		req.Filters.Date.InternalSince = &sinceUTC
		req.Filters.Date.InternalBefore = &beforeUTC
	}
	if req.Filters.Date.Range != nil {
		start, end, ok := s.resolveDateRange(*req.Filters.Date.Range, loc, now)
		if ok {
			assign(start, end)
			return req
		}
	}
	if req.Filters.Date.Temporal != nil {
		legacy := legacyTemporalToRange(*req.Filters.Date.Temporal)
		start, end, ok := s.resolveDateRange(legacy, loc, now)
		if ok {
			assign(start, end)
		}
	}
	return req
}

func (s *Service) resolveDateRange(rng model.SearchDateRange, baseLoc *time.Location, now time.Time) (time.Time, time.Time, bool) {
	startPoint := rng.Start
	endPoint := rng.End
	if startPoint == nil && endPoint == nil {
		return time.Time{}, time.Time{}, false
	}
	if start, end, ok := resolveNamedWeekRange(startPoint, endPoint, now); ok {
		if rng.EndInclusive {
			end = end.Add(time.Nanosecond)
		}
		return start, end, true
	}
	if startPoint == nil {
		startPoint = endPoint
	}
	if endPoint == nil {
		endPoint = startPoint
	}
	start, ok := s.resolveTemporalPoint(startPoint, baseLoc, now, false)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	end, ok := s.resolveTemporalPoint(endPoint, baseLoc, now, true)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	if rng.EndInclusive {
		end = end.Add(time.Nanosecond)
	}
	return start, end, true
}

func resolveNamedWeekRange(startPoint, endPoint *model.SearchTemporalPoint, now time.Time) (time.Time, time.Time, bool) {
	if startPoint == nil || endPoint == nil {
		return time.Time{}, time.Time{}, false
	}
	if strings.ToLower(strings.TrimSpace(startPoint.Type)) != "relative_named_weekday" || strings.ToLower(strings.TrimSpace(endPoint.Type)) != "relative_named_weekday" {
		return time.Time{}, time.Time{}, false
	}
	if strings.ToLower(strings.TrimSpace(startPoint.Weekday)) != "monday" || strings.ToLower(strings.TrimSpace(endPoint.Weekday)) != "sunday" {
		return time.Time{}, time.Time{}, false
	}
	startScope := strings.ToLower(strings.TrimSpace(startPoint.Direction))
	if startScope == "" {
		startScope = strings.ToLower(strings.TrimSpace(startPoint.Value))
	}
	endScope := strings.ToLower(strings.TrimSpace(endPoint.Direction))
	if endScope == "" {
		endScope = strings.ToLower(strings.TrimSpace(endPoint.Value))
	}
	if startScope == "" || startScope != endScope {
		return time.Time{}, time.Time{}, false
	}
	return periodBounds(now, "week", startScope)
}

func (s *Service) resolveTemporalPoint(point *model.SearchTemporalPoint, baseLoc *time.Location, now time.Time, isEnd bool) (time.Time, bool) {
	if point == nil {
		return time.Time{}, false
	}
	loc := baseLoc
	if tz := strings.TrimSpace(point.Timezone); tz != "" {
		if loaded, err := time.LoadLocation(tz); err == nil {
			loc = loaded
		} else {
			log.Printf("search timezone load failed point_tz=%q err=%v", tz, err)
		}
	}
	localNow := now.In(loc)
	switch strings.ToLower(strings.TrimSpace(point.Type)) {
	case "now":
		return localNow, true
	case "absolute_timestamp":
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05 -07:00"} {
			if t, err := time.Parse(layout, strings.TrimSpace(point.Timestamp)); err == nil {
				return t.In(loc), true
			}
		}
		return time.Time{}, false
	case "absolute_local_day":
		start, end, ok := resolveAbsoluteLocalDay(strings.TrimSpace(point.Date), loc)
		if !ok {
			return time.Time{}, false
		}
		if isEnd {
			return end, true
		}
		return start, true
	case "relative_day":
		start, end, ok := resolveRelativeDay(strings.ToLower(strings.TrimSpace(point.Value)), localNow)
		if !ok {
			return time.Time{}, false
		}
		if isEnd {
			return end, true
		}
		return start, true
	case "relative_offset":
		return resolveRelativeOffset(point, localNow, isEnd)
	case "relative_named_weekday":
		return resolveRelativeNamedWeekday(point, localNow, isEnd)
	case "named_period_boundary":
		return resolveNamedPeriodBoundary(point, localNow)
	default:
		return time.Time{}, false
	}
}

func resolveAbsoluteLocalDay(raw string, loc *time.Location) (time.Time, time.Time, bool) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, time.Time{}, false
	}
	day, err := time.ParseInLocation("2006-01-02", raw, loc)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return day, day.Add(24 * time.Hour), true
}

func resolveRelativeDay(value string, now time.Time) (time.Time, time.Time, bool) {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch value {
	case "today":
		return start, start.Add(24 * time.Hour), true
	case "yesterday":
		start = start.Add(-24 * time.Hour)
		return start, start.Add(24 * time.Hour), true
	case "tomorrow":
		start = start.Add(24 * time.Hour)
		return start, start.Add(24 * time.Hour), true
	default:
		return time.Time{}, time.Time{}, false
	}
}

func resolveRelativeOffset(point *model.SearchTemporalPoint, now time.Time, isEnd bool) (time.Time, bool) {
	if point.Amount <= 0 {
		return time.Time{}, false
	}
	sign := -1
	if strings.EqualFold(strings.TrimSpace(point.Direction), "future") || strings.EqualFold(strings.TrimSpace(point.Direction), "next") {
		sign = 1
	}
	target := addTemporalOffset(now, sign*point.Amount, strings.ToLower(strings.TrimSpace(point.Unit)))
	switch strings.ToLower(strings.TrimSpace(point.Snap)) {
	case "start_of_day":
		target = time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, target.Location())
	case "end_of_day":
		target = time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, target.Location()).Add(24 * time.Hour)
	case "", "none":
	default:
		return time.Time{}, false
	}
	if isEnd && strings.EqualFold(strings.TrimSpace(point.Snap), "start_of_day") {
		return target, true
	}
	return target, true
}

func resolveRelativeNamedWeekday(point *model.SearchTemporalPoint, now time.Time, isEnd bool) (time.Time, bool) {
	weekday, ok := parseWeekday(point.Weekday)
	if !ok {
		weekday, ok = parseWeekday(point.Value)
		if !ok {
			return time.Time{}, false
		}
	}
	current := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	delta := int(weekday - current.Weekday())
	scope := strings.ToLower(strings.TrimSpace(point.Direction))
	if scope == "" {
		scope = strings.ToLower(strings.TrimSpace(point.Value))
	}
	switch scope {
	case "last", "past":
		delta -= 7
	case "next", "future":
		delta += 7
	default:
	}
	target := current.AddDate(0, 0, delta)
	if isEnd {
		return target.Add(24 * time.Hour), true
	}
	return target, true
}

func resolveNamedPeriodBoundary(point *model.SearchTemporalPoint, now time.Time) (time.Time, bool) {
	scope := strings.ToLower(strings.TrimSpace(point.Value))
	if scope == "" {
		scope = strings.ToLower(strings.TrimSpace(point.Direction))
	}
	boundary := strings.ToLower(strings.TrimSpace(point.Boundary))
	if boundary == "" {
		boundary = "start"
	}
	start, end, ok := periodBounds(now, strings.ToLower(strings.TrimSpace(point.Period)), scope)
	if !ok {
		return time.Time{}, false
	}
	if boundary == "end" {
		return end, true
	}
	return start, true
}

func addTemporalOffset(now time.Time, amount int, unit string) time.Time {
	switch unit {
	case "hour", "hours":
		return now.Add(time.Duration(amount) * time.Hour)
	case "day", "days":
		return now.AddDate(0, 0, amount)
	case "week", "weeks":
		return now.AddDate(0, 0, 7*amount)
	case "month", "months":
		return now.AddDate(0, amount, 0)
	case "year", "years":
		return now.AddDate(amount, 0, 0)
	default:
		return now
	}
}

func parseWeekday(raw string) (time.Weekday, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "sunday":
		return time.Sunday, true
	case "monday":
		return time.Monday, true
	case "tuesday":
		return time.Tuesday, true
	case "wednesday":
		return time.Wednesday, true
	case "thursday":
		return time.Thursday, true
	case "friday":
		return time.Friday, true
	case "saturday":
		return time.Saturday, true
	default:
		return time.Sunday, false
	}
}

func periodBounds(now time.Time, period, scope string) (time.Time, time.Time, bool) {
	base := now
	switch scope {
	case "last":
		switch period {
		case "day":
			base = now.AddDate(0, 0, -1)
		case "week":
			base = now.AddDate(0, 0, -7)
		case "month":
			base = now.AddDate(0, -1, 0)
		case "quarter":
			base = now.AddDate(0, -3, 0)
		case "year":
			base = now.AddDate(-1, 0, 0)
		}
	case "next":
		switch period {
		case "day":
			base = now.AddDate(0, 0, 1)
		case "week":
			base = now.AddDate(0, 0, 7)
		case "month":
			base = now.AddDate(0, 1, 0)
		case "quarter":
			base = now.AddDate(0, 3, 0)
		case "year":
			base = now.AddDate(1, 0, 0)
		}
	}
	switch period {
	case "day":
		start := time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, base.Location())
		return start, start.Add(24 * time.Hour), true
	case "week":
		start := time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, base.Location())
		for start.Weekday() != time.Monday {
			start = start.AddDate(0, 0, -1)
		}
		return start, start.AddDate(0, 0, 7), true
	case "month":
		start := time.Date(base.Year(), base.Month(), 1, 0, 0, 0, 0, base.Location())
		return start, start.AddDate(0, 1, 0), true
	case "quarter":
		month := ((int(base.Month())-1)/3)*3 + 1
		start := time.Date(base.Year(), time.Month(month), 1, 0, 0, 0, 0, base.Location())
		return start, start.AddDate(0, 3, 0), true
	case "year":
		start := time.Date(base.Year(), time.January, 1, 0, 0, 0, 0, base.Location())
		return start, start.AddDate(1, 0, 0), true
	default:
		return time.Time{}, time.Time{}, false
	}
}

func legacyTemporalToRange(legacy model.SearchTemporalFilter) model.SearchDateRange {
	fieldTZ := strings.TrimSpace(legacy.Timezone)
	switch strings.ToLower(strings.TrimSpace(legacy.Kind)) {
	case "relative_day":
		return model.SearchDateRange{
			Start: &model.SearchTemporalPoint{Type: "relative_day", Value: legacy.Value, Timezone: fieldTZ},
			End:   &model.SearchTemporalPoint{Type: "relative_day", Value: legacy.Value, Timezone: fieldTZ},
		}
	case "relative_window":
		return model.SearchDateRange{
			Start: &model.SearchTemporalPoint{Type: "relative_offset", Direction: "past", Amount: legacy.Amount, Unit: legacy.Unit, Snap: "none", Timezone: fieldTZ},
			End:   &model.SearchTemporalPoint{Type: "now", Timezone: fieldTZ},
		}
	case "absolute_local_day":
		return model.SearchDateRange{
			Start: &model.SearchTemporalPoint{Type: "absolute_local_day", Date: legacy.Date, Timezone: fieldTZ},
			End:   &model.SearchTemporalPoint{Type: "absolute_local_day", Date: legacy.Date, Timezone: fieldTZ},
		}
	default:
		return model.SearchDateRange{}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func capThreadContextBody(items []model.ThreadContextMessage, maxChars int) []model.ThreadContextMessage {
	if maxChars <= 0 {
		return items
	}
	out := make([]model.ThreadContextMessage, 0, len(items))
	used := 0
	for _, item := range items {
		next := item
		if next.ExtractedText != nil {
			body := *next.ExtractedText
			if used >= maxChars {
				next.ExtractedText = nil
			} else if len(body) > maxChars-used {
				cut := body[:maxChars-used]
				next.ExtractedText = &cut
				used = maxChars
			} else {
				used += len(body)
			}
		}
		out = append(out, next)
	}
	return out
}

var attachmentFilenameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func (s *Service) uploadAttachments(ctx context.Context, messageID string, uid uint32, attachments []model.Attachment) {
	if s.Attachments == nil || len(attachments) == 0 {
		return
	}
	for idx, a := range attachments {
		if len(a.Content) == 0 {
			continue
		}
		key := buildAttachmentKey(s.MailboxID, messageID, idx, a.Filename)
		mimeType := a.MIME
		if strings.TrimSpace(mimeType) == "" {
			mimeType = "application/octet-stream"
		}
		storagePath, err := s.Attachments.Put(ctx, key, a.Content, mimeType)
		if err != nil {
			log.Printf("attachment upload failed msg=%s uid=%d idx=%d: %v", messageID, uid, idx, err)
			continue
		}
		if err := s.Store.UpdateAttachmentStorage(ctx, messageID, idx, storagePath); err != nil {
			log.Printf("attachment storage persist failed msg=%s uid=%d idx=%d: %v", messageID, uid, idx, err)
		}
	}
}

func buildAttachmentKey(mailboxID, messageID string, idx int, filename string) string {
	name := strings.TrimSpace(filename)
	if name == "" {
		name = "attachment.bin"
	}
	name = attachmentFilenameSanitizer.ReplaceAllString(path.Base(name), "_")
	return fmt.Sprintf("%s/message-%s/%03d-%s", mailboxID, messageID, idx+1, name)
}

func (s *Service) queueSummarizeRetry(ctx context.Context, messageID string, input summarize.Input, summarizeErr error) error {
	payload, err := json.Marshal(summarizeRetryPayload{Input: input})
	if err != nil {
		return err
	}
	lastErr := ""
	if summarizeErr != nil {
		lastErr = summarizeErr.Error()
	}
	return s.Store.QueueSummarizeJob(ctx, messageID, payload, lastErr)
}

func (s *Service) flushSummarizeQueue(ctx context.Context, maxJobs int) error {
	if s.Summarizer == nil {
		return nil
	}
	if maxJobs <= 0 {
		maxJobs = 16
	}
	jobs, err := s.Store.ClaimSummarizeJobs(ctx, s.MailboxID, maxJobs, 5*time.Minute)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	const statusFlushSize = 8
	doneIDs := make([]string, 0, len(jobs))
	retryIDs := make([]string, 0, len(jobs))
	flushDone := func() error {
		if len(doneIDs) == 0 {
			return nil
		}
		if err := s.Store.MarkSummarizeJobsDone(ctx, doneIDs); err != nil {
			return err
		}
		doneIDs = doneIDs[:0]
		return nil
	}
	flushRetry := func() error {
		if len(retryIDs) == 0 {
			return nil
		}
		if err := s.Store.MarkSummarizeJobsRetry(ctx, retryIDs, "summarize retry failed", 2*time.Minute); err != nil {
			return err
		}
		retryIDs = retryIDs[:0]
		return nil
	}
	for _, job := range jobs {
		var payload summarizeRetryPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			log.Printf("summarize retry payload decode failed job=%s msg=%s: %v", job.ID, job.MessageID, err)
			retryIDs = append(retryIDs, job.ID)
			if len(retryIDs) >= statusFlushSize {
				if err := flushRetry(); err != nil {
					return err
				}
			}
			continue
		}
		digest, err := s.Summarizer.Summarize(ctx, payload.Input)
		if err != nil {
			log.Printf("summarize retry failed job=%s msg=%s: %v", job.ID, job.MessageID, err)
			retryIDs = append(retryIDs, job.ID)
			if len(retryIDs) >= statusFlushSize {
				if err := flushRetry(); err != nil {
					return err
				}
			}
			continue
		}
		digest.MessageID = job.MessageID
		if err := s.Store.SaveDigest(ctx, job.MessageID, digest); err != nil {
			log.Printf("summarize retry save digest failed job=%s msg=%s: %v", job.ID, job.MessageID, err)
			retryIDs = append(retryIDs, job.ID)
			if len(retryIDs) >= statusFlushSize {
				if err := flushRetry(); err != nil {
					return err
				}
			}
			continue
		}
		if s.Embedder != nil {
			parsed := model.ParsedEmail{
				Subject:       ptrValue(payload.Input.Subject),
				ExtractedText: payload.Input.Text,
			}
			segments := buildEmbeddingSegments(parsed, digest)
			if len(segments) > 0 {
				if err := s.Store.QueueEmbeddingJobs(ctx, job.MessageID, s.Embedder.ModelName(), segments); err != nil {
					log.Printf("summarize retry queue embeddings failed job=%s msg=%s: %v", job.ID, job.MessageID, err)
				}
			}
		}
		if err := s.enqueueAutoDraftJob(ctx, job.MessageID); err != nil {
			log.Printf("summarize retry queue auto draft failed job=%s msg=%s: %v", job.ID, job.MessageID, err)
			retryIDs = append(retryIDs, job.ID)
			if len(retryIDs) >= statusFlushSize {
				if err := flushRetry(); err != nil {
					return err
				}
			}
			continue
		}
		doneIDs = append(doneIDs, job.ID)
		if len(doneIDs) >= statusFlushSize {
			if err := flushDone(); err != nil {
				return err
			}
		}
	}
	if err := flushDone(); err != nil {
		return err
	}
	if err := flushRetry(); err != nil {
		return err
	}
	return nil
}

func outboundRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exp := attempt - 1
	if exp > 5 {
		exp = 5
	}
	return time.Minute * time.Duration(1<<exp)
}

func isPermanentOutboundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(msg, " 550 "), strings.Contains(msg, " 551 "), strings.Contains(msg, " 552 "), strings.Contains(msg, " 553 "), strings.Contains(msg, " 554 "):
		return true
	case strings.Contains(msg, "no recipients"), strings.Contains(msg, "from must match alias token"):
		return true
	default:
		return false
	}
}

func (s *Service) flushOutboundQueue(ctx context.Context, maxJobs int) error {
	if s.Sender == nil {
		return nil
	}
	if maxJobs <= 0 {
		maxJobs = 16
	}
	jobs, err := s.Store.ClaimOutboundMessages(ctx, s.MailboxID, maxJobs, 5*time.Minute)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	doneIDs := make([]string, 0, len(jobs))
	deadIDs := make([]string, 0, len(jobs))
	retryGroups := map[time.Duration][]string{}
	retryErrs := map[time.Duration]string{}
	deadErrText := ""
	for _, job := range jobs {
		if err := s.Sender.SendRaw(job.FromEmail, job.To, job.RawRFC822); err != nil {
			if isPermanentOutboundError(err) || job.Attempts >= 9 {
				deadIDs = append(deadIDs, job.ID)
				deadErrText = err.Error()
				continue
			}
			delay := outboundRetryDelay(job.Attempts + 1)
			retryGroups[delay] = append(retryGroups[delay], job.ID)
			retryErrs[delay] = err.Error()
			continue
		}
		doneIDs = append(doneIDs, job.ID)
	}
	if err := s.Store.MarkOutboundMessagesSent(ctx, doneIDs); err != nil {
		return err
	}
	for _, job := range jobs {
		if containsString(doneIDs, job.ID) {
			s.publishAliasEvent("outbound_sent", job.ID, "", job.AliasID)
		}
	}
	for delay, ids := range retryGroups {
		if err := s.Store.MarkOutboundMessagesRetry(ctx, ids, retryErrs[delay], delay); err != nil {
			return err
		}
	}
	if err := s.Store.MarkOutboundMessagesDead(ctx, deadIDs, deadErrText); err != nil {
		return err
	}
	for _, job := range jobs {
		if containsString(deadIDs, job.ID) {
			s.publishAliasEvent("outbound_dead", job.ID, "", job.AliasID)
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func ptrValue[T any](v T) *T { return &v }

func (s *Service) effectiveSummarizeWorkerCount() int {
	if s.SummarizeWorkers <= 0 {
		return 4
	}
	if s.SummarizeWorkers > 32 {
		return 32
	}
	return s.SummarizeWorkers
}

func (s *Service) effectiveEmbeddingBatchSize() int {
	if s.EmbeddingBatchSize <= 0 {
		return 96
	}
	return s.EmbeddingBatchSize
}

func (s *Service) effectiveEmbeddingFlushInterval() time.Duration {
	if s.EmbeddingFlushInterval < 0 {
		return 0
	}
	if s.EmbeddingFlushInterval == 0 {
		return 30 * time.Minute
	}
	return s.EmbeddingFlushInterval
}

func (s *Service) flushEmbeddingQueueIfDue(ctx context.Context, force bool) error {
	if s.Embedder == nil {
		return nil
	}
	batcher, ok := s.Embedder.(embed.BatchEmbedder)
	if !ok {
		return nil
	}
	interval := s.effectiveEmbeddingFlushInterval()
	now := time.Now()

	s.embeddingDrainMu.Lock()
	last := s.embeddingLastDrain
	if !force && !last.IsZero() && now.Sub(last) < interval {
		s.embeddingDrainMu.Unlock()
		return nil
	}
	s.embeddingLastDrain = now
	s.embeddingDrainMu.Unlock()

	if err := s.reconcileSubmittedEmbeddingBatches(ctx, batcher); err != nil {
		return err
	}

	for {
		jobs, err := s.Store.ClaimEmbeddingJobs(ctx, s.MailboxID, s.Embedder.ModelName(), s.effectiveEmbeddingBatchSize(), 10*time.Minute)
		if err != nil {
			return err
		}
		if len(jobs) == 0 {
			return nil
		}
		if err := s.submitClaimedEmbeddingBatch(ctx, batcher, jobs); err != nil {
			return err
		}
		if !force {
			// Process one claimed batch per interval tick to keep regular sync latency bounded.
			return nil
		}
	}
}

func (s *Service) submitClaimedEmbeddingBatch(ctx context.Context, batcher embed.BatchEmbedder, jobs []store.EmbeddingJob) error {
	if len(jobs) == 0 {
		return nil
	}
	reqs := make([]embed.BatchEmbeddingJob, 0, len(jobs))
	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		reqs = append(reqs, embed.BatchEmbeddingJob{
			JobID: job.ID,
			Input: job.Segment.Text,
		})
		ids = append(ids, job.ID)
	}
	if len(reqs) == 0 {
		return nil
	}
	batchID, err := batcher.SubmitEmbeddingBatch(ctx, reqs)
	if err != nil {
		_ = s.Store.MarkEmbeddingJobsRetry(ctx, ids, err.Error(), 5*time.Minute)
		return err
	}
	if err := s.Store.MarkEmbeddingJobsSubmitted(ctx, ids, batchID); err != nil {
		return err
	}
	return nil
}

func (s *Service) reconcileSubmittedEmbeddingBatches(ctx context.Context, batcher embed.BatchEmbedder) error {
	batchIDs, err := s.Store.ListEmbeddingSubmittedBatchIDs(ctx, s.MailboxID, s.Embedder.ModelName(), 25)
	if err != nil {
		return err
	}
	for _, batchID := range batchIDs {
		st, err := batcher.GetBatchStatus(ctx, batchID)
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(st.Status)) {
		case "validating", "in_progress", "finalizing":
			continue
		case "completed":
			if err := s.applyCompletedEmbeddingBatch(ctx, batchID, st.OutputFileID, batcher); err != nil {
				return err
			}
		case "failed", "expired", "cancelled", "canceled":
			jobs, err := s.Store.GetEmbeddingJobsByBatch(ctx, s.MailboxID, batchID)
			if err != nil {
				return err
			}
			ids := make([]string, 0, len(jobs))
			for _, j := range jobs {
				ids = append(ids, j.ID)
			}
			if err := s.Store.MarkEmbeddingJobsRetry(ctx, ids, "remote batch "+st.Status, 5*time.Minute); err != nil {
				return err
			}
		default:
			continue
		}
	}
	return nil
}

func (s *Service) applyCompletedEmbeddingBatch(ctx context.Context, batchID, outputFileID string, batcher embed.BatchEmbedder) error {
	jobsInBatch, err := s.Store.GetEmbeddingJobsByBatch(ctx, s.MailboxID, batchID)
	if err != nil {
		return err
	}
	if len(jobsInBatch) == 0 {
		return nil
	}
	allIDs := make([]string, 0, len(jobsInBatch))
	for _, j := range jobsInBatch {
		allIDs = append(allIDs, j.ID)
	}
	if strings.TrimSpace(outputFileID) == "" {
		return s.Store.MarkEmbeddingJobsRetry(ctx, allIDs, "batch completed without output_file_id", 5*time.Minute)
	}
	success, failures, err := batcher.GetBatchEmbeddingResults(ctx, outputFileID)
	if err != nil {
		return s.Store.MarkEmbeddingJobsRetry(ctx, allIDs, err.Error(), 5*time.Minute)
	}
	successIDs := make([]string, 0, len(success))
	for id := range success {
		successIDs = append(successIDs, id)
	}
	jobsForSuccess, err := s.Store.GetEmbeddingJobsByIDs(ctx, s.MailboxID, successIDs)
	if err != nil {
		return err
	}
	type packed struct {
		segments []store.EmbeddingSegment
		vectors  [][]float32
		jobIDs   []string
	}
	perMessage := map[string]packed{}
	seenSuccess := map[string]struct{}{}
	for _, j := range jobsForSuccess {
		vec, ok := success[j.ID]
		if !ok {
			continue
		}
		p := perMessage[j.MessageID]
		p.segments = append(p.segments, j.Segment)
		p.vectors = append(p.vectors, vec)
		p.jobIDs = append(p.jobIDs, j.ID)
		perMessage[j.MessageID] = p
		seenSuccess[j.ID] = struct{}{}
	}

	doneIDs := []string{}
	retryIDs := []string{}
	for messageID, p := range perMessage {
		if err := s.Store.SaveMessageEmbeddings(ctx, messageID, s.Embedder.ModelName(), p.segments, p.vectors); err != nil {
			log.Printf("persist embeddings failed msg=%s: %v", messageID, err)
			retryIDs = append(retryIDs, p.jobIDs...)
			continue
		}
		doneIDs = append(doneIDs, p.jobIDs...)
	}
	for _, j := range jobsInBatch {
		if _, ok := seenSuccess[j.ID]; ok {
			continue
		}
		if _, failed := failures[j.ID]; failed {
			retryIDs = append(retryIDs, j.ID)
			continue
		}
		// Not present in output; retry.
		retryIDs = append(retryIDs, j.ID)
	}
	if err := s.Store.MarkEmbeddingJobsDone(ctx, doneIDs); err != nil {
		return err
	}
	if err := s.Store.MarkEmbeddingJobsRetry(ctx, retryIDs, "batch output incomplete or persist failed", 5*time.Minute); err != nil {
		return err
	}
	return nil
}

func buildEmbeddingSegments(parsed model.ParsedEmail, digest model.Digest) []store.EmbeddingSegment {
	segments := make([]store.EmbeddingSegment, 0, 10)
	if subject := strings.TrimSpace(deref(parsed.Subject)); subject != "" {
		segments = append(segments, store.EmbeddingSegment{Segment: "subject", ChunkIndex: 0, Text: subject})
	}
	if summary := strings.TrimSpace(digest.SummaryShort); summary != "" {
		segments = append(segments, store.EmbeddingSegment{Segment: "summary", ChunkIndex: 0, Text: summary})
	}
	body := strings.TrimSpace(parsed.ExtractedText)
	if body == "" {
		return segments
	}
	for i, chunk := range splitForEmbedding(body, 1200, 200) {
		segments = append(segments, store.EmbeddingSegment{
			Segment:    "body_chunk",
			ChunkIndex: i,
			Text:       chunk,
		})
	}
	return segments
}

func splitForEmbedding(text string, chunkSize, overlap int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if chunkSize <= 0 {
		chunkSize = 1200
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= chunkSize {
		overlap = chunkSize / 4
	}
	runes := []rune(text)
	if len(runes) <= chunkSize {
		return []string{text}
	}
	step := chunkSize - overlap
	if step <= 0 {
		step = chunkSize
	}
	out := make([]string, 0, (len(runes)/step)+1)
	for start := 0; start < len(runes); start += step {
		end := start + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunk := strings.TrimSpace(string(runes[start:end]))
		if chunk != "" {
			out = append(out, chunk)
		}
		if end == len(runes) {
			break
		}
	}
	return out
}

func fuseSearchResults(keyword, semantic []model.ListItem, limit int) []model.ListItem {
	if limit <= 0 {
		limit = 50
	}
	if len(keyword) == 0 {
		if len(semantic) > limit {
			return semantic[:limit]
		}
		return semantic
	}
	if len(semantic) == 0 {
		if len(keyword) > limit {
			return keyword[:limit]
		}
		return keyword
	}
	type scored struct {
		item  model.ListItem
		score float64
	}
	scores := map[string]scored{}
	const (
		kwWeight = 0.55
		smWeight = 0.45
	)
	for idx, it := range keyword {
		s := 1.0 / float64(idx+1)
		cur := scores[it.MsgID]
		cur.item = it
		cur.score += kwWeight * s
		scores[it.MsgID] = cur
	}
	for idx, it := range semantic {
		s := 1.0 / float64(idx+1)
		cur := scores[it.MsgID]
		if cur.item.MsgID == "" {
			cur.item = it
		}
		cur.score += smWeight * s
		scores[it.MsgID] = cur
	}
	list := make([]scored, 0, len(scores))
	for _, v := range scores {
		ageHours := time.Since(v.item.ReceivedAt).Hours()
		if ageHours > 0 {
			// Mild recency bump, capped to avoid dominating retrieval relevance.
			v.score += math.Min(0.08, 24.0/ageHours*0.01)
		}
		list = append(list, v)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].score == list[j].score {
			return list[i].item.ReceivedAt.After(list[j].item.ReceivedAt)
		}
		return list[i].score > list[j].score
	})
	out := make([]model.ListItem, 0, min(limit, len(list)))
	for _, it := range list {
		out = append(out, it.item)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func applyRecencyAndLimit(items []model.ListItem, recencyIntent bool, limit int) []model.ListItem {
	if limit <= 0 {
		limit = 50
	}
	if recencyIntent {
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].ReceivedAt.Equal(items[j].ReceivedAt) {
				return items[i].MsgID > items[j].MsgID
			}
			return items[i].ReceivedAt.After(items[j].ReceivedAt)
		})
	}
	if len(items) > limit {
		return items[:limit]
	}
	return items
}

func hasRecencyIntent(query string) bool {
	if query == "" {
		return true
	}
	for _, token := range tokenizeSearchQuery(query) {
		switch token {
		case "recent", "latest", "newest", "last":
			return true
		}
	}
	return false
}

func isRecencyOnlyQuery(query string) bool {
	tokens := tokenizeSearchQuery(query)
	if len(tokens) == 0 {
		return true
	}
	allowed := map[string]struct{}{
		"my": {}, "me": {}, "recent": {}, "latest": {}, "newest": {}, "last": {},
		"emails": {}, "email": {}, "messages": {}, "message": {}, "inbox": {}, "mails": {}, "mail": {},
	}
	for _, token := range tokens {
		if _, ok := allowed[token]; !ok {
			return false
		}
	}
	return true
}

func tokenizeSearchQuery(query string) []string {
	parts := strings.Fields(strings.ToLower(query))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		token := strings.Trim(part, ".,!?;:'\"()[]{}")
		if token != "" {
			out = append(out, token)
		}
	}
	return out
}

func inferFolderFromQuery(query string) (folder string, stripped string) {
	tokens := tokenizeSearchQuery(query)
	if len(tokens) == 0 {
		return "", query
	}
	detected := ""
	cleaned := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		switch tok {
		case "junk", "spam":
			if detected == "" {
				detected = "junk"
			}
			continue
		case "inbox":
			if detected == "" {
				detected = "inbox"
			}
			continue
		case "sent":
			if detected == "" {
				detected = "sent"
			}
			continue
		}
		cleaned = append(cleaned, tok)
	}
	return detected, strings.Join(cleaned, " ")
}

func waitOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func linearRetryDelay(base time.Duration, failures int, max time.Duration) time.Duration {
	if base <= 0 {
		base = 30 * time.Second
	}
	if failures < 1 {
		failures = 1
	}
	delay := base * time.Duration(failures)
	if max > 0 && delay > max {
		return max
	}
	return delay
}

func isCredentialAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	signatures := []string{
		"authentication failed",
		"authenticationfailed",
		"invalid credentials",
		"login failed",
		"command login failed",
		"auth failed",
		"username and password not accepted",
	}
	for _, sig := range signatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}
