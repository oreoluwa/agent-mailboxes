package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func jsonResponse(request *http.Request, status int, headers http.Header, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     headers,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    request,
	}
}

func TestClientAppliesMailboxToGetAndPost(t *testing.T) {
	requests := 0
	client := NewForMailbox("https://mail.example.test", "secret-token", "pilot")
	client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if got := r.URL.Query().Get("mailbox"); got != "pilot" {
			t.Fatalf("mailbox query = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("authorization = %q", got)
		}
		var body bytes.Buffer
		_ = json.NewEncoder(&body).Encode(map[string]bool{"ok": true})
		return jsonResponse(r, http.StatusOK, make(http.Header), body.String()), nil
	})
	var response map[string]bool
	if err := client.Get("/read", map[string]string{"id": "1", "mailbox": "other"}, &response); err != nil {
		t.Fatal(err)
	}
	if err := client.Post("/send", map[string]string{"body": "hello"}, &response); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestClientDoesNotFollowRedirectsWithToken(t *testing.T) {
	requests := 0
	client := New("https://mail.example.test", "secret-token")
	client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		header := make(http.Header)
		header.Set("Location", "https://elsewhere.example.test/collect")
		return jsonResponse(r, http.StatusFound, header, "redirect"), nil
	})
	err := client.Get("/redirect", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "api status 302") {
		t.Fatalf("redirect error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, redirect was followed", requests)
	}
}

func TestClientRejectsMalformedBaseURL(t *testing.T) {
	client := New("::not-a-url", "secret-token")
	if err := client.Get("/inbox", nil, nil); err == nil || !strings.Contains(err.Error(), "invalid email API URL") {
		t.Fatalf("malformed URL error = %v", err)
	}
}

func TestClientPreservesLegacyAPIPathPrefixes(t *testing.T) {
	for _, base := range []string{"https://mail.example.test/mail", "https://mail.example.test/mail/", "https://mail.example.test/mail%2Ftenant"} {
		t.Run(base, func(t *testing.T) {
			client := NewForMailbox(base, "secret-token", "primary")
			requests := 0
			client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				prefix := "/mail"
				if strings.Contains(base, "%2F") {
					prefix = "/mail%2Ftenant"
				}
				if got := r.URL.EscapedPath(); got != prefix+"/v1/inbox/list" {
					t.Fatalf("path=%q want %q", got, prefix+"/v1/inbox/list")
				}
				if r.URL.Query().Get("mailbox") != "primary" || r.URL.Query().Get("limit") != "2" {
					t.Fatalf("query=%v", r.URL.Query())
				}
				return jsonResponse(r, http.StatusOK, make(http.Header), `{}`), nil
			})
			if err := client.Get("/v1/inbox/list", map[string]string{"limit": "2"}, nil); err != nil {
				t.Fatal(err)
			}
			if err := client.Post("/v1/inbox/list?limit=2", nil, nil); err != nil {
				t.Fatal(err)
			}
			if requests != 2 {
				t.Fatalf("requests=%d", requests)
			}
		})
	}
}
