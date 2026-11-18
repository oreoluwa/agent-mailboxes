package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/attachanalyze"
	"github.com/oreoluwa/agent-mailboxes/internal/auth"
	"github.com/oreoluwa/agent-mailboxes/internal/imapingest"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/router"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
	"github.com/oreoluwa/agent-mailboxes/internal/summarize"
)

type fakeFetcher struct{ msgs []imapingest.Message }

func (f fakeFetcher) FetchSince(lastUID uint32) ([]imapingest.Message, uint32, uint32, error) {
	out := []imapingest.Message{}
	newLast := lastUID
	for _, m := range f.msgs {
		if m.UID > lastUID {
			out = append(out, m)
			if m.UID > newLast {
				newLast = m.UID
			}
		}
	}
	return out, 1, newLast, nil
}

type fakeSummarizer struct{}

func (fakeSummarizer) Summarize(ctx context.Context, in summarize.Input) (model.Digest, error) {
	entities, _ := json.Marshal(map[string]any{"emails": []string{"test@example.com"}})
	return model.Digest{
		ModelName:         "fake-local",
		SummaryShort:      "short",
		SummaryBullets:    []string{"b1", "b2", "b3"},
		Intent:            "support",
		Priority:          "med",
		RequiredAction:    "reply",
		SuggestedNextStep: "respond",
		ExtractedEntities: entities,
		Confidence:        0.8,
	}, nil
}

type selectiveFailSummarizer struct {
	failOnSubject string
}

type transientAutoDraftEnqueueStore struct {
	*fakeStore
	failuresRemaining int
	enqueueCalls      int
	enqueued          int
}

func (s *transientAutoDraftEnqueueStore) GetMailboxAgentSettings(context.Context, string) (model.MailboxAgentSettings, error) {
	return model.MailboxAgentSettings{AutoDraftEnabled: true, PolicyVersion: 7}, nil
}

func (s *transientAutoDraftEnqueueStore) QueueAutoDraftJob(context.Context, string, string, int) (store.AutoDraftJob, bool, error) {
	s.enqueueCalls++
	if s.failuresRemaining > 0 {
		s.failuresRemaining--
		return store.AutoDraftJob{}, false, errors.New("transient enqueue failure")
	}
	s.enqueued++
	return store.AutoDraftJob{ID: "auto-1"}, true, nil
}

func (s selectiveFailSummarizer) Summarize(ctx context.Context, in summarize.Input) (model.Digest, error) {
	if strings.Contains(in.Subject, s.failOnSubject) {
		return model.Digest{}, errors.New("upstream timeout")
	}
	entities, _ := json.Marshal(map[string]any{"emails": []string{"test@example.com"}})
	return model.Digest{
		ModelName:         "fake-local",
		SummaryShort:      "short",
		SummaryBullets:    []string{"b1", "b2", "b3"},
		Intent:            "support",
		Priority:          "med",
		RequiredAction:    "reply",
		SuggestedNextStep: "respond",
		ExtractedEntities: entities,
		Confidence:        0.8,
	}, nil
}

type fakeStore struct {
	autoDraftCompleted   map[string]map[string]bool
	folderID             string
	lastUID              uint32
	lastByFolder         map[string]uint32
	folderByName         map[string]string
	insertedByUID        map[int64]string
	uidValidityByUID     map[int64]int64
	rawByUID             map[int64][]byte
	digestByMsg          map[string]model.Digest
	embeddingQueuedByMsg map[string]int
	summarizeQueuedByMsg map[string]int
	summarizeJobs        map[string]store.SummarizeJob
	threadByMsg          map[string][]model.ThreadContextMessage
	threadClaimsByMsg    map[string]model.TokenClaims
	claimsByHash         map[string]model.TokenClaims
	searchStructuredFn   func(ctx context.Context, claims model.TokenClaims, req model.SearchRequest) ([]model.ListItem, error)
	searchSemanticFn     func(ctx context.Context, claims model.TokenClaims, req model.SearchRequest, embedding []float32) ([]model.ListItem, error)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		folderID:             "f1",
		lastByFolder:         map[string]uint32{},
		folderByName:         map[string]string{"INBOX": "f1"},
		insertedByUID:        map[int64]string{},
		uidValidityByUID:     map[int64]int64{},
		rawByUID:             map[int64][]byte{},
		digestByMsg:          map[string]model.Digest{},
		embeddingQueuedByMsg: map[string]int{},
		summarizeQueuedByMsg: map[string]int{},
		summarizeJobs:        map[string]store.SummarizeJob{},
		threadByMsg:          map[string][]model.ThreadContextMessage{},
		threadClaimsByMsg:    map[string]model.TokenClaims{},
		claimsByHash:         map[string]model.TokenClaims{},
	}
}

func (f *fakeStore) CreateMailbox(ctx context.Context, name string, creds store.MailboxCreds) (store.Mailbox, error) {
	return store.Mailbox{}, errors.New("not implemented")
}
func (f *fakeStore) OnboardMailbox(ctx context.Context, input store.OnboardMailboxInput) (store.OnboardMailboxResult, error) {
	return store.OnboardMailboxResult{}, errors.New("not implemented")
}
func (f *fakeStore) ProvisionMailboxAgent(ctx context.Context, input store.ProvisionMailboxAgentInput) (store.ProvisionMailboxAgentResult, error) {
	return store.ProvisionMailboxAgentResult{}, errors.New("not implemented")
}
func (f *fakeStore) ListMailboxes(ctx context.Context) ([]store.Mailbox, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeStore) GetMailboxByName(ctx context.Context, name string) (store.Mailbox, error) {
	return store.Mailbox{}, errors.New("not implemented")
}
func (f *fakeStore) SetMailboxEnabled(ctx context.Context, name string, enabled bool) error {
	return errors.New("not implemented")
}
func (f *fakeStore) GetFolderCheckpoint(ctx context.Context, mailboxID, folder string) (uint32, uint32, string, error) {
	last := f.lastUID
	if v, ok := f.lastByFolder[folder]; ok {
		last = v
	}
	folderID := f.folderID
	if v, ok := f.folderByName[folder]; ok {
		folderID = v
	}
	return 1, last, folderID, nil
}
func (f *fakeStore) UpsertFolderCheckpoint(ctx context.Context, mailboxID, folder string, uidValidity uint32, lastUID uint32) (string, error) {
	f.lastByFolder[folder] = lastUID
	if strings.EqualFold(folder, "INBOX") {
		f.lastUID = lastUID
	}
	if id, ok := f.folderByName[folder]; ok {
		return id, nil
	}
	id := fmt.Sprintf("f-%s", strings.ReplaceAll(strings.ToLower(folder), "/", "-"))
	f.folderByName[folder] = id
	return id, nil
}
func (f *fakeStore) ListAliases(ctx context.Context, mailboxID string) ([]router.Alias, error) {
	return []router.Alias{{ID: "a1", Email: "x.ai@example.com", Enabled: true}}, nil
}
func (f *fakeStore) AddAlias(ctx context.Context, mailboxID, email string) error { return nil }
func (f *fakeStore) MintToken(ctx context.Context, aliasID *string, name string, tokenHash string, scopes []string) (string, error) {
	return "", errors.New("not implemented")
}
func (f *fakeStore) RevokeToken(ctx context.Context, tokenID string) error { return nil }
func (f *fakeStore) AuthenticateToken(ctx context.Context, tokenHash string) (model.TokenClaims, error) {
	if c, ok := f.claimsByHash[tokenHash]; ok {
		return c, nil
	}
	return model.TokenClaims{}, store.ErrUnauthorized
}
func (f *fakeStore) InsertMessage(ctx context.Context, mailboxID, folderID string, msg model.ParsedEmail, audience []string) (string, bool, error) {
	f.uidValidityByUID[msg.IMAPUID] = msg.IMAPUIDValidity
	if id, ok := f.insertedByUID[msg.IMAPUID]; ok {
		return id, false, nil
	}
	id := fmt.Sprintf("msg-%d", msg.IMAPUID)
	f.insertedByUID[msg.IMAPUID] = id
	f.rawByUID[msg.IMAPUID] = append([]byte(nil), msg.RawRFC822...)
	return id, true, nil
}
func (f *fakeStore) UpdateAttachmentStorage(ctx context.Context, messageID string, ordinal int, storagePath string) error {
	return nil
}
func (f *fakeStore) SaveDigest(ctx context.Context, messageID string, digest model.Digest) error {
	f.digestByMsg[messageID] = digest
	return nil
}
func (f *fakeStore) ListFolders(ctx context.Context, claims model.TokenClaims) ([]model.FolderSummary, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeStore) ListInbox(ctx context.Context, claims model.TokenClaims, folder string, unreadOnly bool, limit int) ([]model.ListItem, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeStore) ReadMessage(ctx context.Context, claims model.TokenClaims, messageID string) (model.Message, error) {
	return model.Message{}, errors.New("not implemented")
}
func (f *fakeStore) GetBody(ctx context.Context, claims model.TokenClaims, messageID string) (model.Body, error) {
	return model.Body{}, errors.New("not implemented")
}
func (f *fakeStore) GetRawMessage(ctx context.Context, claims model.TokenClaims, messageID string) ([]byte, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeStore) AckMessage(ctx context.Context, claims model.TokenClaims, messageID string) error {
	return nil
}
func (f *fakeStore) UnackMessage(ctx context.Context, claims model.TokenClaims, messageID string) error {
	return nil
}
func (f *fakeStore) QueueOutboundMessage(ctx context.Context, mailboxID, aliasID, from string, to []string, raw []byte, idempotencyKey string) (store.OutboundMessage, bool, error) {
	return store.OutboundMessage{ID: "out-1", MailboxID: mailboxID, AliasID: aliasID, FromEmail: from, To: append([]string(nil), to...), RawRFC822: append([]byte(nil), raw...), IdempotencyKey: idempotencyKey, Status: "pending"}, true, nil
}
func (f *fakeStore) ClaimOutboundMessages(ctx context.Context, mailboxID string, limit int, lease time.Duration) ([]store.OutboundMessage, error) {
	return nil, nil
}
func (f *fakeStore) MarkOutboundMessagesSent(ctx context.Context, ids []string) error { return nil }
func (f *fakeStore) MarkOutboundMessagesRetry(ctx context.Context, ids []string, lastErr string, retryAfter time.Duration) error {
	return nil
}
func (f *fakeStore) MarkOutboundMessagesDead(ctx context.Context, ids []string, lastErr string) error {
	return nil
}
func (f *fakeStore) SaveMessageEmbeddings(ctx context.Context, messageID, modelName string, segments []store.EmbeddingSegment, embeddings [][]float32) error {
	return nil
}
func (f *fakeStore) QueueEmbeddingJobs(ctx context.Context, messageID, modelName string, segments []store.EmbeddingSegment) error {
	f.embeddingQueuedByMsg[messageID] += len(segments)
	return nil
}
func (f *fakeStore) ClaimEmbeddingJobs(ctx context.Context, mailboxID, modelName string, limit int, lease time.Duration) ([]store.EmbeddingJob, error) {
	return nil, nil
}
func (f *fakeStore) MarkEmbeddingJobsSubmitted(ctx context.Context, ids []string, batchID string) error {
	return nil
}
func (f *fakeStore) ListEmbeddingSubmittedBatchIDs(ctx context.Context, mailboxID, modelName string, limit int) ([]string, error) {
	return nil, nil
}
func (f *fakeStore) GetEmbeddingJobsByBatch(ctx context.Context, mailboxID, batchID string) ([]store.EmbeddingJob, error) {
	return nil, nil
}
func (f *fakeStore) GetEmbeddingJobsByIDs(ctx context.Context, mailboxID string, ids []string) ([]store.EmbeddingJob, error) {
	return nil, nil
}
func (f *fakeStore) MarkEmbeddingJobsDone(ctx context.Context, ids []string) error {
	return nil
}
func (f *fakeStore) MarkEmbeddingJobsRetry(ctx context.Context, ids []string, lastErr string, retryAfter time.Duration) error {
	return nil
}
func (f *fakeStore) QueueSummarizeJob(ctx context.Context, messageID string, payload []byte, lastErr string) error {
	f.summarizeQueuedByMsg[messageID]++
	f.summarizeJobs[messageID] = store.SummarizeJob{
		ID:        "sj-" + messageID,
		MessageID: messageID,
		Payload:   append([]byte(nil), payload...),
	}
	return nil
}
func (f *fakeStore) ClaimSummarizeJobs(ctx context.Context, mailboxID string, limit int, lease time.Duration) ([]store.SummarizeJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	out := make([]store.SummarizeJob, 0, min(limit, len(f.summarizeJobs)))
	for _, job := range f.summarizeJobs {
		out = append(out, job)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
func (f *fakeStore) MarkSummarizeJobsDone(ctx context.Context, ids []string) error {
	for _, id := range ids {
		msgID := strings.TrimPrefix(id, "sj-")
		delete(f.summarizeJobs, msgID)
	}
	return nil
}
func (f *fakeStore) MarkSummarizeJobsRetry(ctx context.Context, ids []string, lastErr string, retryAfter time.Duration) error {
	return nil
}
func (f *fakeStore) Search(ctx context.Context, claims model.TokenClaims, query, folder string, limit int) ([]model.ListItem, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeStore) SearchByEmbedding(ctx context.Context, claims model.TokenClaims, embedding []float32, folder string, limit int) ([]model.ListItem, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeStore) SearchStructured(ctx context.Context, claims model.TokenClaims, req model.SearchRequest) ([]model.ListItem, error) {
	if f.searchStructuredFn != nil {
		return f.searchStructuredFn(ctx, claims, req)
	}
	return nil, errors.New("not implemented")
}
func (f *fakeStore) SearchStructuredByEmbedding(ctx context.Context, claims model.TokenClaims, req model.SearchRequest, embedding []float32) ([]model.ListItem, error) {
	if f.searchSemanticFn != nil {
		return f.searchSemanticFn(ctx, claims, req, embedding)
	}
	return nil, errors.New("not implemented")
}
func (f *fakeStore) RecordAudit(ctx context.Context, tokenID, action string, messageID *string, target *string) error {
	return nil
}
func (f *fakeStore) GetAliasByEmail(ctx context.Context, mailboxID, email string) (string, error) {
	return "a1", nil
}
func (f *fakeStore) GetAliasEmailByID(ctx context.Context, aliasID string) (string, error) {
	return "x.ai@example.com", nil
}
func (f *fakeStore) GetMailboxIDByAliasID(ctx context.Context, aliasID string) (string, error) {
	return "m1", nil
}
func (f *fakeStore) GetThreadContextForMessage(ctx context.Context, claims model.TokenClaims, mailboxID, messageID string) ([]model.ThreadContextMessage, error) {
	f.threadClaimsByMsg[messageID] = claims
	if v, ok := f.threadByMsg[messageID]; ok {
		return v, nil
	}
	return []model.ThreadContextMessage{}, nil
}
func (f *fakeStore) GetMessageThreadMeta(ctx context.Context, claims model.TokenClaims, messageID string) (inReplyTo *string, references *string, subject *string, err error) {
	return nil, nil, nil, nil
}
func (f *fakeStore) GetMessageReplyTo(ctx context.Context, claims model.TokenClaims, messageID string) (string, error) {
	return "sender@example.com", nil
}

type captureSummarizer struct {
	last summarize.Input
}

func (c *captureSummarizer) Summarize(ctx context.Context, in summarize.Input) (model.Digest, error) {
	c.last = in
	entities, _ := json.Marshal(map[string]any{})
	return model.Digest{
		ModelName:         "capture",
		SummaryShort:      "ok",
		SummaryBullets:    []string{"a", "b", "c"},
		Intent:            "other",
		Priority:          "med",
		RequiredAction:    "review",
		SuggestedNextStep: "review",
		ExtractedEntities: entities,
		Confidence:        0.9,
	}, nil
}

type stubAttachmentAnalyzer struct {
	result attachanalyze.Result
	err    error
	last   attachanalyze.Input
}

func (s *stubAttachmentAnalyzer) Analyze(ctx context.Context, in attachanalyze.Input) (attachanalyze.Result, error) {
	s.last = in
	if s.err != nil {
		return attachanalyze.Result{}, s.err
	}
	return s.result, nil
}

func (f *fakeStore) Status(ctx context.Context, mailboxID string) (store.Status, error) {
	return store.Status{}, nil
}

func TestSyncDedupeOnRestart(t *testing.T) {
	raw := []byte("From: Sender <sender@example.com>\r\nTo: x.ai@example.com\r\nSubject: Hello\r\nMessage-ID: <1@test>\r\nDate: Mon, 2 Jan 2006 15:04:05 -0700\r\n\r\nBody")
	fstore := newFakeStore()
	svc := &Service{
		Store:      fstore,
		Summarizer: fakeSummarizer{},
		Fetcher:    fakeFetcher{msgs: []imapingest.Message{{UID: 1, InternalDate: time.Now(), Raw: raw}}},
		MailboxID:  "m1",
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fstore.insertedByUID) != 1 {
		t.Fatalf("expected one message inserted, got %d", len(fstore.insertedByUID))
	}
	if got := fstore.summarizeQueuedByMsg["msg-1"]; got != 1 {
		t.Fatalf("expected summarize queued once for existing message dedupe, got %d", got)
	}
}

func TestSyncStoresDigest(t *testing.T) {
	raw := []byte("From: Sender <sender@example.com>\r\nTo: x.ai@example.com\r\nSubject: Invoice 123\r\nMessage-ID: <2@test>\r\nDate: Mon, 2 Jan 2006 15:04:05 -0700\r\n\r\nPlease pay $25 by Friday")
	fstore := newFakeStore()
	svc := &Service{
		Store:      fstore,
		Summarizer: fakeSummarizer{},
		Fetcher:    fakeFetcher{msgs: []imapingest.Message{{UID: 2, InternalDate: time.Now(), Raw: raw}}},
		MailboxID:  "m1",
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.flushSummarizeQueue(context.Background(), 16); err != nil {
		t.Fatal(err)
	}
	if len(fstore.digestByMsg) != 1 {
		t.Fatalf("expected digest stored")
	}
	for _, d := range fstore.digestByMsg {
		if d.SummaryShort == "" || len(d.SummaryBullets) == 0 {
			t.Fatalf("invalid digest: %+v", d)
		}
	}
}

func TestSummarizeJobRetriesWhenAutoDraftEnqueueFails(t *testing.T) {
	base := newFakeStore()
	payload, err := json.Marshal(summarizeRetryPayload{Input: summarize.Input{Text: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	base.summarizeJobs["msg-1"] = store.SummarizeJob{
		ID:        "sj-msg-1",
		MessageID: "msg-1",
		Payload:   payload,
	}
	fstore := &transientAutoDraftEnqueueStore{fakeStore: base, failuresRemaining: 1}
	svc := &Service{Store: fstore, Summarizer: fakeSummarizer{}, MailboxID: "m1"}

	if err := svc.flushSummarizeQueue(context.Background(), 16); err != nil {
		t.Fatal(err)
	}
	if _, ok := base.summarizeJobs["msg-1"]; !ok {
		t.Fatal("summarize job was marked done after auto-draft enqueue failure")
	}
	if err := svc.flushSummarizeQueue(context.Background(), 16); err != nil {
		t.Fatal(err)
	}
	if _, ok := base.summarizeJobs["msg-1"]; ok {
		t.Fatal("summarize job was not completed after auto-draft enqueue retry")
	}
	if fstore.enqueueCalls != 2 || fstore.enqueued != 1 {
		t.Fatalf("auto-draft enqueue calls=%d successful=%d, want 2 calls and one successful enqueue", fstore.enqueueCalls, fstore.enqueued)
	}
}

func TestSyncStoresMessageUIDValidityEpoch(t *testing.T) {
	raw := []byte("From: Sender <sender@example.com>\r\nTo: x.ai@example.com\r\nSubject: Epoch\r\nMessage-ID: <epoch@test>\r\nDate: Mon, 2 Jan 2006 15:04:05 -0700\r\n\r\nBody")
	fstore := newFakeStore()
	svc := &Service{
		Store:     fstore,
		Fetcher:   fakeFetcher{msgs: []imapingest.Message{{UID: 13, InternalDate: time.Now(), Raw: raw}}},
		MailboxID: "m1",
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fstore.uidValidityByUID[13]; got != 1 {
		t.Fatalf("stored UIDVALIDITY = %d, want fetch epoch 1", got)
	}
}

func TestSyncWithoutSummarizerDoesNotQueueSummarization(t *testing.T) {
	raw := []byte("From: Sender <sender@example.com>\r\nTo: x.ai@example.com\r\nSubject: No AI\r\nMessage-ID: <no-ai@test>\r\nDate: Mon, 2 Jan 2006 15:04:05 -0700\r\n\r\nStored without a model")
	fstore := newFakeStore()
	svc := &Service{
		Store:     fstore,
		Fetcher:   fakeFetcher{msgs: []imapingest.Message{{UID: 12, InternalDate: time.Now(), Raw: raw}}},
		MailboxID: "m1",
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fstore.summarizeQueuedByMsg["msg-12"]; got != 0 {
		t.Fatalf("expected no summarize job when summarizer is disabled, got %d", got)
	}
}

func TestSyncStoresRawRFC822(t *testing.T) {
	raw := []byte("From: Sender <sender@example.com>\r\nTo: x.ai@example.com\r\nSubject: Raw RFC822\r\nMessage-ID: <raw@test>\r\nDate: Mon, 2 Jan 2006 15:04:05 -0700\r\n\r\nBody")
	fstore := newFakeStore()
	svc := &Service{
		Store:      fstore,
		Summarizer: fakeSummarizer{},
		Fetcher:    fakeFetcher{msgs: []imapingest.Message{{UID: 9, InternalDate: time.Now(), Raw: raw}}},
		MailboxID:  "m1",
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := string(fstore.rawByUID[9]); got != string(raw) {
		t.Fatalf("expected raw RFC822 to be stored, got %q", got)
	}
}

func TestSyncQueuesSummarizeRetryWithoutBlockingOtherMessages(t *testing.T) {
	rawFail := []byte("From: Sender <sender@example.com>\r\nTo: x.ai@example.com\r\nSubject: Fail me\r\nMessage-ID: <10@test>\r\nDate: Mon, 2 Jan 2006 15:04:05 -0700\r\n\r\nBody")
	rawOK := []byte("From: Sender <sender@example.com>\r\nTo: x.ai@example.com\r\nSubject: Process me\r\nMessage-ID: <11@test>\r\nDate: Mon, 2 Jan 2006 15:04:05 -0700\r\n\r\nBody")
	fstore := newFakeStore()
	svc := &Service{
		Store:      fstore,
		Summarizer: selectiveFailSummarizer{failOnSubject: "Fail me"},
		Fetcher: fakeFetcher{msgs: []imapingest.Message{
			{UID: 10, InternalDate: time.Now(), Raw: rawFail},
			{UID: 11, InternalDate: time.Now(), Raw: rawOK},
		}},
		MailboxID: "m1",
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.flushSummarizeQueue(context.Background(), 16); err != nil {
		t.Fatal(err)
	}
	if _, ok := fstore.summarizeQueuedByMsg["msg-10"]; !ok {
		t.Fatalf("expected failed message summarize to be queued")
	}
	if _, ok := fstore.digestByMsg["msg-11"]; !ok {
		t.Fatalf("expected second message to still be digested")
	}
}

func TestSyncIncludesThreadContextWhenEnabled(t *testing.T) {
	raw := []byte("From: Sender <sender@example.com>\r\nTo: x.ai@example.com\r\nSubject: Follow up\r\nMessage-ID: <3@test>\r\nDate: Mon, 2 Jan 2006 15:04:05 -0700\r\n\r\nSee latest status")
	fstore := newFakeStore()
	s := &captureSummarizer{}
	fstore.threadByMsg["msg-3"] = []model.ThreadContextMessage{
		{
			MessageID:     "msg-1",
			RFCMessageID:  "<1@test>",
			InternalDate:  time.Now().Add(-2 * time.Hour),
			FromEmail:     "sender@example.com",
			ExtractedText: ptr("Earlier context"),
		},
		{
			MessageID:     "msg-3",
			RFCMessageID:  "<3@test>",
			InternalDate:  time.Now(),
			FromEmail:     "sender@example.com",
			ExtractedText: ptr("See latest status"),
			IsCurrent:     true,
		},
	}
	svc := &Service{
		Store:             fstore,
		Summarizer:        s,
		Fetcher:           fakeFetcher{msgs: []imapingest.Message{{UID: 3, InternalDate: time.Now(), Raw: raw}}},
		MailboxID:         "m1",
		ThreadContextMode: "full",
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.flushSummarizeQueue(context.Background(), 16); err != nil {
		t.Fatal(err)
	}
	if got := len(s.last.Thread); got != 2 {
		t.Fatalf("expected thread context to include full thread, got %d entries", got)
	}
	claims := fstore.threadClaimsByMsg["msg-3"]
	if claims.TokenID != "" || claims.AliasID != nil || claims.MailboxID == nil || *claims.MailboxID != "m1" {
		t.Fatalf("expected mailbox-scoped internal thread claims, got %#v", claims)
	}
}

func TestSyncPassesAttachmentFindingsToSummarizer(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"From: Sender <sender@example.com>",
		"To: x.ai@example.com",
		"Subject: With Attachment",
		"Message-ID: <att@test>",
		"MIME-Version: 1.0",
		"Content-Type: multipart/mixed; boundary=\"b\"",
		"",
		"--b",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Body",
		"--b",
		"Content-Type: text/plain; name=\"report.txt\"",
		"Content-Disposition: attachment; filename=\"report.txt\"",
		"",
		"line one",
		"--b--",
		"",
	}, "\r\n"))
	fstore := newFakeStore()
	s := &captureSummarizer{}
	an := &stubAttachmentAnalyzer{
		result: attachanalyze.Result{
			Summaries: []attachanalyze.Summary{
				{
					Filename:   "report.txt",
					MIME:       "text/plain",
					Source:     "text-extract",
					Summary:    "line one",
					Confidence: 0.8,
				},
			},
			Issues: []string{"attachment 2 skipped"},
		},
	}
	svc := &Service{
		Store:              fstore,
		Summarizer:         s,
		AttachmentAnalyzer: an,
		Fetcher:            fakeFetcher{msgs: []imapingest.Message{{UID: 6, InternalDate: time.Now(), Raw: raw}}},
		MailboxID:          "m1",
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.flushSummarizeQueue(context.Background(), 16); err != nil {
		t.Fatal(err)
	}
	if got := len(an.last.Attachments); got != 1 {
		t.Fatalf("expected analyzer to receive 1 attachment, got %d", got)
	}
	if got := len(s.last.AttachmentSummaries); got != 1 {
		t.Fatalf("expected summarizer to receive 1 attachment summary, got %d", got)
	}
	if got := len(s.last.AttachmentIssues); got != 1 {
		t.Fatalf("expected summarizer to receive attachment issues, got %d", got)
	}
}

func TestSyncContinuesWhenAttachmentAnalysisFails(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"From: Sender <sender@example.com>",
		"To: x.ai@example.com",
		"Subject: With Attachment Error",
		"Message-ID: <att-err@test>",
		"MIME-Version: 1.0",
		"Content-Type: multipart/mixed; boundary=\"b\"",
		"",
		"--b",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Body",
		"--b",
		"Content-Type: image/png; name=\"photo.png\"",
		"Content-Disposition: attachment; filename=\"photo.png\"",
		"",
		"img",
		"--b--",
		"",
	}, "\r\n"))
	fstore := newFakeStore()
	s := &captureSummarizer{}
	an := &stubAttachmentAnalyzer{err: errors.New("vision unavailable")}
	svc := &Service{
		Store:              fstore,
		Summarizer:         s,
		AttachmentAnalyzer: an,
		Fetcher:            fakeFetcher{msgs: []imapingest.Message{{UID: 7, InternalDate: time.Now(), Raw: raw}}},
		MailboxID:          "m1",
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.flushSummarizeQueue(context.Background(), 16); err != nil {
		t.Fatal(err)
	}
	if len(fstore.digestByMsg) != 1 {
		t.Fatalf("expected digest save despite analysis failure")
	}
	if got := len(s.last.AttachmentIssues); got == 0 {
		t.Fatalf("expected failure issue to be passed into summarizer")
	}
}

func ptr[T any](v T) *T { return &v }

func TestAuthenticateAcceptsPrimaryKey(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{Store: fs}
	token := "t-primary"
	key := "primary-key"
	fs.claimsByHash[auth.HashToken(key, token)] = model.TokenClaims{
		TokenID: "tok1",
		Scopes:  map[model.Scope]bool{model.ScopeRead: true},
	}

	claims, err := svc.Authenticate(context.Background(), "Bearer "+token, []string{key})
	if err != nil {
		t.Fatalf("authenticate failed: %v", err)
	}
	if claims.TokenID != "tok1" {
		t.Fatalf("unexpected token id: %s", claims.TokenID)
	}
}

func TestAuthenticateAcceptsOldKeyFallback(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{Store: fs}
	token := "t-old"
	oldKey := "old-key"
	fs.claimsByHash[auth.HashToken(oldKey, token)] = model.TokenClaims{
		TokenID: "tok2",
		Scopes:  map[model.Scope]bool{model.ScopeRead: true},
	}

	claims, err := svc.Authenticate(context.Background(), "Bearer "+token, []string{"new-key", oldKey})
	if err != nil {
		t.Fatalf("authenticate failed: %v", err)
	}
	if claims.TokenID != "tok2" {
		t.Fatalf("unexpected token id: %s", claims.TokenID)
	}
}

func TestAuthenticateRejectsWhenNoKeysMatch(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{Store: fs}

	_, err := svc.Authenticate(context.Background(), "Bearer missing", []string{"new-key", "old-key"})
	if !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("expected unauthorized, got: %v", err)
	}
}

type folderFetcher struct {
	msgsByFolder map[string][]imapingest.Message
	fetched      []string
}

func (f *folderFetcher) FetchSince(lastUID uint32) ([]imapingest.Message, uint32, uint32, error) {
	return f.FetchSinceFolder("INBOX", lastUID)
}

func (f *folderFetcher) FetchSinceFolder(folder string, lastUID uint32) ([]imapingest.Message, uint32, uint32, error) {
	f.fetched = append(f.fetched, folder)
	msgs := f.msgsByFolder[folder]
	out := []imapingest.Message{}
	newLast := lastUID
	for _, m := range msgs {
		if m.UID > lastUID {
			out = append(out, m)
			if m.UID > newLast {
				newLast = m.UID
			}
		}
	}
	return out, 1, newLast, nil
}

func (f *folderFetcher) ListFolders() ([]string, error) {
	return []string{"INBOX", "Junk", "Projects/2026"}, nil
}

func TestSyncScansDiscoveredFolders(t *testing.T) {
	raw := []byte("From: Sender <sender@example.com>\r\nTo: x.ai@example.com\r\nSubject: Multi Folder\r\nMessage-ID: <5@test>\r\nDate: Mon, 2 Jan 2006 15:04:05 -0700\r\n\r\nBody")
	fs := newFakeStore()
	ff := &folderFetcher{
		msgsByFolder: map[string][]imapingest.Message{
			"INBOX":         {{UID: 1, InternalDate: time.Now(), Raw: raw}},
			"Junk":          {{UID: 2, InternalDate: time.Now(), Raw: raw}},
			"Projects/2026": {{UID: 3, InternalDate: time.Now(), Raw: raw}},
		},
	}
	svc := &Service{
		Store:      fs,
		Summarizer: fakeSummarizer{},
		Fetcher:    ff,
		MailboxID:  "m1",
	}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(ff.fetched); got != 3 {
		t.Fatalf("expected 3 folder fetches, got %d", got)
	}
	for _, folder := range []string{"INBOX", "Junk", "Projects/2026"} {
		if _, ok := fs.lastByFolder[folder]; !ok {
			t.Fatalf("missing checkpoint for folder %s", folder)
		}
	}
}

func TestLinearRetryDelay(t *testing.T) {
	base := 10 * time.Second
	max := 40 * time.Second
	if got := linearRetryDelay(base, 1, max); got != 10*time.Second {
		t.Fatalf("attempt 1 delay mismatch: %s", got)
	}
	if got := linearRetryDelay(base, 3, max); got != 30*time.Second {
		t.Fatalf("attempt 3 delay mismatch: %s", got)
	}
	if got := linearRetryDelay(base, 9, max); got != max {
		t.Fatalf("max cap mismatch: %s", got)
	}
}

func TestIsCredentialAuthError(t *testing.T) {
	if !isCredentialAuthError(errors.New("AUTHENTICATIONFAILED Invalid credentials")) {
		t.Fatalf("expected auth error signature to be detected")
	}
	if isCredentialAuthError(errors.New("dial tcp: i/o timeout")) {
		t.Fatalf("unexpected non-auth network error detection")
	}
}

func TestSyncExplicitFoldersExcludeInbox(t *testing.T) {
	fs := newFakeStore()
	ff := &folderFetcher{msgsByFolder: map[string][]imapingest.Message{}}
	svc := &Service{Store: fs, Fetcher: ff, MailboxID: "m1", SyncFolders: []string{" Archive ", "archive", " "}}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ff.fetched) != 1 || ff.fetched[0] != "Archive" {
		t.Fatalf("explicit folders fetched %v, want Archive only", ff.fetched)
	}
}

type archiveOnlyFolderFetcher struct{ *folderFetcher }

func (f archiveOnlyFolderFetcher) ListFolders() ([]string, error) { return []string{"Archive"}, nil }

func TestDiscoveredFoldersStillIncludeInboxFallback(t *testing.T) {
	ff := &folderFetcher{msgsByFolder: map[string][]imapingest.Message{}}
	svc := &Service{Store: newFakeStore(), Fetcher: archiveOnlyFolderFetcher{ff}, MailboxID: "m1"}
	if err := svc.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ff.fetched) != 2 || ff.fetched[0] != "Archive" || ff.fetched[1] != "INBOX" {
		t.Fatalf("discovered folders fetched %v, want Archive and INBOX", ff.fetched)
	}
}
