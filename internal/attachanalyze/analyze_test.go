package attachanalyze

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestAnalyzeMIMERoutingAndLimits(t *testing.T) {
	visionCalls := 0
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			visionCalls++
			return jsonResponse(http.StatusOK, `{"choices":[{"message":{"content":"{\"summary\":\"invoice screenshot\",\"key_points\":[\"amount visible\"],\"confidence\":0.9}"}}]}`), nil
		}),
	}

	a := NewOpenAICompat("http://example.test", "vision-model", "", Config{
		Enabled:      true,
		MaxFiles:     3,
		MaxFileBytes: 8 * 1024 * 1024,
	})
	a.Client = client
	in := Input{
		Attachments: []model.Attachment{
			{Filename: "photo.png", MIME: "image/png", Content: []byte("img-bytes")},
			{Filename: "note.txt", MIME: "text/plain", Content: []byte("hello from attachment")},
			{Filename: "blob.bin", MIME: "application/octet-stream", Content: []byte{0x01, 0x02}},
			{Filename: "too-many.txt", MIME: "text/plain", Content: []byte("ignored by cap")},
		},
	}
	out, err := a.Analyze(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if visionCalls != 1 {
		t.Fatalf("expected 1 vision call, got %d", visionCalls)
	}
	if got := len(out.Summaries); got != 2 {
		t.Fatalf("expected 2 summaries (image + text), got %d", got)
	}
	if got := len(out.Issues); got == 0 {
		t.Fatalf("expected issues for unsupported mime and limit")
	}
}

func TestAnalyzeVisionFailureFallsBackToIssue(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusBadGateway, `{"error":"boom"}`), nil
		}),
	}
	a := NewOpenAICompat("http://example.test", "vision-model", "", Config{
		Enabled:      true,
		MaxFiles:     3,
		MaxFileBytes: 8 * 1024 * 1024,
	})
	a.Client = client
	out, err := a.Analyze(context.Background(), Input{
		Attachments: []model.Attachment{
			{Filename: "scan.pdf", MIME: "application/pdf", Content: []byte("pdf")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Summaries) != 0 {
		t.Fatalf("expected no summaries on failed vision call")
	}
	if len(out.Issues) != 1 || !strings.Contains(out.Issues[0], "pdf analysis failed") {
		t.Fatalf("expected vision failure issue, got %+v", out.Issues)
	}
}

func TestAnalyzeTextLikeWithoutVisionModel(t *testing.T) {
	a := NewOpenAICompat("http://example.test", "", "", Config{
		Enabled:      true,
		MaxFiles:     3,
		MaxFileBytes: 8 * 1024 * 1024,
	})
	out, err := a.Analyze(context.Background(), Input{
		Attachments: []model.Attachment{
			{Filename: "data.json", MIME: "application/json", Content: []byte(`{"a":"b"}`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Summaries) != 1 {
		t.Fatalf("expected text summary, got %d", len(out.Summaries))
	}
	if !bytes.Contains([]byte(out.Summaries[0].Summary), []byte(`{"a":"b"}`)) {
		t.Fatalf("expected text summary to include attachment content, got %q", out.Summaries[0].Summary)
	}
}

func TestVisionUsesResponsesProviderMode(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/v1/responses" {
				t.Fatalf("expected /v1/responses path, got %s", r.URL.Path)
			}
			return jsonResponse(http.StatusOK, `{"output_text":"{\"summary\":\"from responses\",\"key_points\":[\"k1\"],\"confidence\":0.8}"}`), nil
		}),
	}
	a := NewOpenAICompat("http://example.test", "vision-model", "", Config{
		Enabled:      true,
		MaxFiles:     1,
		MaxFileBytes: 1024 * 1024,
		ProviderMode: providerModeResponses,
	})
	a.Client = client
	out, err := a.Analyze(context.Background(), Input{
		Attachments: []model.Attachment{
			{Filename: "photo.png", MIME: "image/png", Content: []byte("img-bytes")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Summaries) != 1 {
		t.Fatalf("expected one summary, got %d", len(out.Summaries))
	}
	if !strings.Contains(out.Summaries[0].Summary, "from responses") {
		t.Fatalf("expected responses summary, got %q", out.Summaries[0].Summary)
	}
}

func TestVisionRetriesTransientErrors(t *testing.T) {
	var calls int32
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				return jsonResponse(http.StatusBadGateway, `{"error":"temporary"}`), nil
			}
			return jsonResponse(http.StatusOK, `{"choices":[{"message":{"content":"{\"summary\":\"ok after retry\",\"key_points\":[],\"confidence\":0.9}"}}]}`), nil
		}),
	}
	a := NewOpenAICompat("http://example.test", "vision-model", "", Config{
		Enabled:      true,
		MaxFiles:     1,
		MaxFileBytes: 1024 * 1024,
		MaxRetries:   1,
		ProviderMode: providerModeChat,
	})
	a.Client = client
	out, err := a.Analyze(context.Background(), Input{
		Attachments: []model.Attachment{
			{Filename: "photo.png", MIME: "image/png", Content: []byte("img-bytes")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("expected retry, got calls=%d", calls)
	}
	if len(out.Summaries) != 1 {
		t.Fatalf("expected one summary, got %d", len(out.Summaries))
	}
}

func TestAnalyzeDocxOOXMLExtraction(t *testing.T) {
	var docx bytes.Buffer
	zw := zip.NewWriter(&docx)
	f, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte(`<w:document><w:body><w:p><w:r><w:t>Hello Docx World</w:t></w:r></w:p></w:body></w:document>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	a := NewOpenAICompat("http://example.test", "", "", Config{
		Enabled:      true,
		MaxFiles:     1,
		MaxFileBytes: 1024 * 1024,
	})
	out, err := a.Analyze(context.Background(), Input{
		Attachments: []model.Attachment{
			{
				Filename: "file.docx",
				MIME:     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
				Content:  docx.Bytes(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Summaries) != 1 {
		t.Fatalf("expected ooxml summary, got %d", len(out.Summaries))
	}
	if !strings.Contains(strings.ToLower(out.Summaries[0].Summary), "hello docx world") {
		t.Fatalf("unexpected ooxml summary: %q", out.Summaries[0].Summary)
	}
}
