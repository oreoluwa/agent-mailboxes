package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

func TestActorReplayCleansExpiredRowsBeforeRecording(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "active-replay"}[duplicate], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			p := NewPostgres(db)
			mock.ExpectBegin()
			mock.ExpectExec(`DELETE FROM actor_request_replays WHERE ctid IN \(SELECT ctid FROM actor_request_replays WHERE expires_at < CURRENT_TIMESTAMP ORDER BY expires_at LIMIT 1000 FOR UPDATE SKIP LOCKED\)`).WillReturnResult(sqlmock.NewResult(0, 2))
			insert := mock.ExpectExec("INSERT INTO actor_request_replays")
			if duplicate {
				insert.WillReturnError(&pq.Error{Code: "23505"})
				mock.ExpectRollback()
			} else {
				insert.WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			}
			err = p.RecordActorReplay(context.Background(), "mailbox", "request", "nonce", "send", "draft", time.Now().Add(time.Minute))
			if duplicate && !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("expected replay denial, got %v", err)
			}
			if !duplicate && err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestActorReplayCleanupFailureDoesNotRecord(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := NewPostgres(db)
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM actor_request_replays").WillReturnError(errors.New("cleanup unavailable"))
	mock.ExpectRollback()
	if err := p.RecordActorReplay(context.Background(), "mailbox", "request", "nonce", "send", "draft", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("must fail closed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
