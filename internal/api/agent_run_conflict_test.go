package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

func TestAgentRunConflictMapsToConflictResponses(t *testing.T) {
	if got := connect.CodeOf(connectError(store.ErrAgentRunConflict)); got != connect.CodeAborted {
		t.Fatalf("Connect conflict code = %v, want %v", got, connect.CodeAborted)
	}
	recorder := httptest.NewRecorder()
	writeErr(recorder, store.ErrAgentRunConflict)
	if got := recorder.Code; got != http.StatusConflict {
		t.Fatalf("HTTP conflict status = %d, want %d", got, http.StatusConflict)
	}
}
