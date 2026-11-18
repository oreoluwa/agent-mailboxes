package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/imapingest"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/router"
)

type historyStore struct {
	*fakeStore
	validity   uint32
	checkpoint uint32
	folder     string
	failInsert bool
}

func (s *historyStore) GetFolderCheckpoint(context.Context, string, string) (uint32, uint32, string, error) {
	return s.validity, s.checkpoint, s.folder, nil
}
func (s *historyStore) UpsertFolderCheckpoint(_ context.Context, _, _ string, validity, last uint32) (string, error) {
	s.validity, s.checkpoint, s.folder = validity, last, "history-folder"
	return s.folder, nil
}
func (s *historyStore) InsertMessage(ctx context.Context, mailboxID, folderID string, msg model.ParsedEmail, audience []string) (string, bool, error) {
	if s.failInsert {
		return "", false, errors.New("temporary insert failure")
	}
	return s.fakeStore.InsertMessage(ctx, mailboxID, folderID, msg, audience)
}

type checkpointTestFetcher struct{ checkpoints [][2]uint32 }

func (f *checkpointTestFetcher) FetchSince(uint32) ([]imapingest.Message, uint32, uint32, error) {
	return nil, 0, 0, errors.New("must use checkpoint-aware fetch")
}
func (f *checkpointTestFetcher) FetchSinceCheckpoint(_ string, validity, last uint32) ([]imapingest.Message, uint32, uint32, error) {
	f.checkpoints = append(f.checkpoints, [2]uint32{validity, last})
	return []imapingest.Message{{UID: 501, InternalDate: time.Now(), Raw: []byte("From: sender@example.com\r\nTo: x.ai@example.com\r\nSubject: history\r\n\r\nbody")}}, 2, 501, nil
}

func TestHistoryFloorSurvivesFirstInsertFailure(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "reset"}[reset], func(t *testing.T) {
			st := &historyStore{fakeStore: newFakeStore(), failInsert: true}
			if reset {
				st.validity, st.checkpoint, st.folder = 1, 999, "history-folder"
			}
			fetcher := &checkpointTestFetcher{}
			svc := &Service{Store: st, Fetcher: fetcher, MailboxID: "mailbox"}
			aliases := []router.Alias{{ID: "a1", Email: "x.ai@example.com", Enabled: true}}
			if err := svc.syncFolder(context.Background(), "INBOX", aliases); err == nil {
				t.Fatal("expected insert failure")
			}
			if st.validity != 2 || st.checkpoint != 500 {
				t.Fatalf("lost history boundary: validity=%d checkpoint=%d", st.validity, st.checkpoint)
			}
			st.failInsert = false
			if err := svc.syncFolder(context.Background(), "INBOX", aliases); err != nil {
				t.Fatal(err)
			}
			if fetcher.checkpoints[1] != [2]uint32{2, 500} {
				t.Fatalf("retry checkpoint=%v", fetcher.checkpoints[1])
			}
			if st.checkpoint != 501 {
				t.Fatalf("completion checkpoint=%d", st.checkpoint)
			}
		})
	}
}
