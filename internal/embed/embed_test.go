package embed

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOpenAICompatEmbed(t *testing.T) {
	e := NewOpenAICompat("http://example.test", "text-embedding", "")
	e.Client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/v1/embeddings" {
				t.Fatalf("unexpected path: %s", r.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"embedding":[0.1,0.2]},{"embedding":[0.3,0.4]}]}`)),
			}, nil
		}),
	}

	out, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || len(out[0]) != 2 {
		t.Fatalf("unexpected embedding shape: %+v", out)
	}
}

func TestOpenAICompatEmbedCountMismatch(t *testing.T) {
	e := NewOpenAICompat("http://example.test", "text-embedding", "")
	e.Client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"embedding":[0.1,0.2]}]}`)),
			}, nil
		}),
	}

	if _, err := e.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("expected count mismatch error")
	}
}
