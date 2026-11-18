package store

import (
	"context"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func TestBuildReplyThreadMeta(t *testing.T) {
	tests := []struct {
		name             string
		rfcMessageID     *string
		parentInReplyTo  *string
		parentReferences *string
		wantInReplyTo    string
		wantReferences   string
	}{
		{
			name:           "root message",
			rfcMessageID:   stringPtr("<root@example.test>"),
			wantInReplyTo:  "<root@example.test>",
			wantReferences: "<root@example.test>",
		},
		{
			name:             "nested reply",
			rfcMessageID:     stringPtr("<child@example.test>"),
			parentInReplyTo:  stringPtr("<root@example.test>"),
			parentReferences: stringPtr("<root@example.test>"),
			wantInReplyTo:    "<child@example.test>",
			wantReferences:   "<root@example.test> <child@example.test>",
		},
		{
			name:             "missing RFC message id preserves known chain",
			parentInReplyTo:  stringPtr("<root@example.test>"),
			parentReferences: stringPtr("<root@example.test> <parent@example.test>"),
			wantReferences:   "<root@example.test> <parent@example.test>",
		},
		{
			name:            "missing RFC message id falls back to parent",
			parentInReplyTo: stringPtr("<root@example.test>"),
			wantReferences:  "<root@example.test>",
		},
		{
			name:             "existing source reference is not duplicated",
			rfcMessageID:     stringPtr("<child@example.test>"),
			parentReferences: stringPtr("<root@example.test> <child@example.test> <child@example.test>"),
			wantInReplyTo:    "<child@example.test>",
			wantReferences:   "<root@example.test> <child@example.test>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inReplyTo, references := buildReplyThreadMeta(tt.rfcMessageID, tt.parentInReplyTo, tt.parentReferences)
			if got := stringValue(inReplyTo); got != tt.wantInReplyTo {
				t.Fatalf("In-Reply-To = %q, want %q", got, tt.wantInReplyTo)
			}
			if got := stringValue(references); got != tt.wantReferences {
				t.Fatalf("References = %q, want %q", got, tt.wantReferences)
			}
		})
	}
}

func TestGetMessageThreadMetaBuildsReplyHeadersFromSourceMessage(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Postgres{DB: db}
	aliasID := "a1"
	claims := model.TokenClaims{AliasID: &aliasID, Scopes: map[model.Scope]bool{model.ScopeRead: true}}
	mock.ExpectQuery("SELECT EXISTS").WithArgs("msg-1", aliasID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT message_id AS rfc_message_id, in_reply_to AS in_reply_to, "references" AS references, subject AS subject FROM messages WHERE \(id = 'msg-1'\) LIMIT 1`).
		WillReturnRows(sqlmock.NewRows([]string{"rfc_message_id", "in_reply_to", "references", "subject"}).
			AddRow("<child@example.test>", "<root@example.test>", "<root@example.test>", "Status"))

	inReplyTo, references, subject, err := p.GetMessageThreadMeta(context.Background(), claims, "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := stringValue(inReplyTo); got != "<child@example.test>" {
		t.Fatalf("In-Reply-To = %q", got)
	}
	if got := stringValue(references); got != "<root@example.test> <child@example.test>" {
		t.Fatalf("References = %q", got)
	}
	if got := stringValue(subject); got != "Status" {
		t.Fatalf("Subject = %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func stringPtr(value string) *string { return &value }

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
