package attachanalyze

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"image/jpeg"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "image/gif"
	_ "image/png"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

const (
	providerModeAuto      = "auto"
	providerModeChat      = "chat_completions"
	providerModeResponses = "responses"
)

var (
	pdfTjRE       = regexp.MustCompile(`\(([^()]*)\)\s*Tj`)
	pdfTjArrayRE  = regexp.MustCompile(`\[(.*?)\]\s*TJ`)
	pdfArrayStrRE = regexp.MustCompile(`\(([^()]*)\)`)
)

type Analyzer interface {
	Analyze(ctx context.Context, in Input) (Result, error)
}

type Input struct {
	Attachments []model.Attachment
}

type Config struct {
	Enabled          bool
	MaxFiles         int
	MaxFileBytes     int64
	MaxImageBytes    int64
	ProviderMode     string
	MaxRetries       int
	VisionTimeout    time.Duration
	PDFRenderToolBin string
	OfficeToolBin    string
}

type Result struct {
	Summaries []Summary
	Issues    []string
}

type Summary struct {
	Filename   string
	MIME       string
	Source     string
	Summary    string
	Confidence float64
}

type OpenAICompat struct {
	BaseURL   string
	ModelName string
	APIKey    string
	Config    Config
	Client    *http.Client
}

func NewOpenAICompat(baseURL, model, apiKey string, cfg Config) *OpenAICompat {
	baseURL = strings.TrimRight(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = 3
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 8 * 1024 * 1024
	}
	if cfg.MaxImageBytes <= 0 {
		cfg.MaxImageBytes = 2 * 1024 * 1024
	}
	if strings.TrimSpace(cfg.ProviderMode) == "" {
		cfg.ProviderMode = providerModeAuto
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.VisionTimeout <= 0 {
		cfg.VisionTimeout = 60 * time.Second
	}
	if strings.TrimSpace(cfg.PDFRenderToolBin) == "" {
		cfg.PDFRenderToolBin = "pdftoppm"
	}
	if strings.TrimSpace(cfg.OfficeToolBin) == "" {
		cfg.OfficeToolBin = "soffice"
	}
	return &OpenAICompat{
		BaseURL:   baseURL,
		ModelName: strings.TrimSpace(model),
		APIKey:    apiKey,
		Config:    cfg,
		Client:    &http.Client{Timeout: cfg.VisionTimeout},
	}
}

func (o *OpenAICompat) Analyze(ctx context.Context, in Input) (Result, error) {
	if !o.Config.Enabled || len(in.Attachments) == 0 {
		return Result{}, nil
	}
	out := Result{
		Summaries: make([]Summary, 0, len(in.Attachments)),
		Issues:    []string{},
	}
	processed := 0
	for idx, a := range in.Attachments {
		if processed >= o.Config.MaxFiles {
			out.Issues = append(out.Issues, fmt.Sprintf("attachment processing limit reached: analyzed first %d attachment(s)", o.Config.MaxFiles))
			break
		}
		processed++

		name := strings.TrimSpace(a.Filename)
		if name == "" {
			name = fmt.Sprintf("attachment-%d", idx+1)
		}
		mimeType := normalizeMIME(a.MIME)
		if len(a.Content) == 0 {
			out.Issues = append(out.Issues, fmt.Sprintf("attachment %q skipped: empty content", name))
			continue
		}
		if int64(len(a.Content)) > o.Config.MaxFileBytes {
			out.Issues = append(out.Issues, fmt.Sprintf("attachment %q skipped: size %d exceeds limit %d bytes", name, len(a.Content), o.Config.MaxFileBytes))
			continue
		}

		switch {
		case isTextLike(mimeType):
			out.Summaries = append(out.Summaries, summarizeTextLike(name, mimeType, a.Content))
		case mimeType == "application/pdf":
			s, err := o.summarizePDF(ctx, name, a.Content)
			if err != nil {
				out.Issues = append(out.Issues, fmt.Sprintf("attachment %q pdf analysis failed: %v", name, err))
				continue
			}
			out.Summaries = append(out.Summaries, s)
		case strings.HasPrefix(mimeType, "image/"):
			s, err := o.summarizeVisionWithRetries(ctx, name, mimeType, a.Content)
			if err != nil {
				out.Issues = append(out.Issues, fmt.Sprintf("attachment %q vision analysis failed: %v", name, err))
				continue
			}
			out.Summaries = append(out.Summaries, s)
		case isOOXMLLike(mimeType):
			s, err := summarizeOOXML(name, mimeType, a.Content)
			if err != nil {
				out.Issues = append(out.Issues, fmt.Sprintf("attachment %q ooxml analysis failed: %v", name, err))
				continue
			}
			out.Summaries = append(out.Summaries, s)
		case isLegacyOfficeLike(mimeType):
			s, err := o.summarizeLegacyOffice(ctx, name, mimeType, a.Content)
			if err != nil {
				out.Issues = append(out.Issues, fmt.Sprintf("attachment %q legacy-office analysis failed: %v", name, err))
				continue
			}
			out.Summaries = append(out.Summaries, s)
		default:
			out.Issues = append(out.Issues, fmt.Sprintf("attachment %q skipped: unsupported mime %q", name, mimeType))
		}
	}
	return out, nil
}

func isOOXMLLike(mimeType string) bool {
	switch mimeType {
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return true
	default:
		return false
	}
}

func isLegacyOfficeLike(mimeType string) bool {
	switch mimeType {
	case "application/msword",
		"application/vnd.ms-excel",
		"application/vnd.ms-powerpoint",
		"application/vnd.ms-word.document.macroenabled.12",
		"application/vnd.ms-excel.sheet.macroenabled.12",
		"application/vnd.ms-powerpoint.presentation.macroenabled.12":
		return true
	default:
		return false
	}
}

func summarizeOOXML(name, mimeType string, content []byte) (Summary, error) {
	text, err := extractOOXMLText(content)
	if err != nil {
		return Summary{}, err
	}
	if strings.TrimSpace(text) == "" {
		return Summary{}, fmt.Errorf("ooxml text extraction returned empty content")
	}
	return summarizeTextLike(name, mimeType, []byte(text)), nil
}

func extractOOXMLText(content []byte) (string, error) {
	readerAt := bytes.NewReader(content)
	zr, err := zip.NewReader(readerAt, int64(len(content)))
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	keep := func(name string) bool {
		name = strings.ToLower(name)
		return strings.HasPrefix(name, "word/") || strings.HasPrefix(name, "ppt/slides/") || strings.HasPrefix(name, "xl/") || strings.HasPrefix(name, "docprops/")
	}
	out := make([]string, 0, 32)
	for _, f := range zr.File {
		if !keep(f.Name) || !strings.HasSuffix(strings.ToLower(f.Name), ".xml") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(rc, 2*1024*1024))
		_ = rc.Close()
		txt := extractXMLText(b)
		if txt != "" {
			out = append(out, txt)
		}
	}
	joined := normalizeWhitespace(strings.Join(out, " "))
	if len(joined) > 2000 {
		joined = joined[:2000] + "..."
	}
	return joined, nil
}

var xmlTagRE = regexp.MustCompile(`<[^>]+>`)

func extractXMLText(xmlBytes []byte) string {
	raw := string(xmlBytes)
	raw = xmlTagRE.ReplaceAllString(raw, " ")
	raw = html.UnescapeString(raw)
	raw = strings.ToValidUTF8(raw, " ")
	return normalizeWhitespace(raw)
}

func (o *OpenAICompat) summarizeLegacyOffice(ctx context.Context, filename, mimeType string, content []byte) (Summary, error) {
	bin, err := exec.LookPath(o.Config.OfficeToolBin)
	if err != nil {
		return Summary{}, fmt.Errorf("office converter %q not available", o.Config.OfficeToolBin)
	}
	tmpDir, err := os.MkdirTemp("", "attach-office-*")
	if err != nil {
		return Summary{}, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	inPath := tmpDir + "/" + sanitizeFilename(filename)
	if err := os.WriteFile(inPath, content, 0o600); err != nil {
		return Summary{}, fmt.Errorf("write office temp file: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, "--headless", "--convert-to", "txt:Text", "--outdir", tmpDir, inPath)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return Summary{}, fmt.Errorf("office conversion failed: %s", msg)
	}

	base := strings.TrimSuffix(filepath.Base(inPath), filepath.Ext(inPath))
	txtPath := tmpDir + "/" + base + ".txt"
	txt, err := os.ReadFile(txtPath)
	if err != nil {
		return Summary{}, fmt.Errorf("read converted text: %w", err)
	}
	return summarizeTextLike(filename, mimeType, txt), nil
}

var filenameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "office.bin"
	}
	name = filenameSanitizer.ReplaceAllString(name, "_")
	return name
}

func (o *OpenAICompat) summarizePDF(ctx context.Context, filename string, content []byte) (Summary, error) {
	text := extractPDFText(content)
	if strings.TrimSpace(text) != "" {
		return summarizeTextLike(filename, "application/pdf", []byte(text)), nil
	}
	img, err := o.renderPDFPageToPNG(ctx, content)
	if err != nil {
		return Summary{}, err
	}
	return o.summarizeVisionWithRetries(ctx, filename, "image/png", img)
}

func (o *OpenAICompat) renderPDFPageToPNG(ctx context.Context, content []byte) ([]byte, error) {
	bin, err := exec.LookPath(o.Config.PDFRenderToolBin)
	if err != nil {
		return nil, fmt.Errorf("pdf renderer %q not available", o.Config.PDFRenderToolBin)
	}
	tmpDir, err := os.MkdirTemp("", "attach-pdf-render-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	inPath := tmpDir + "/input.pdf"
	outPrefix := tmpDir + "/page"
	outPath := outPrefix + ".png"
	if err := os.WriteFile(inPath, content, 0o600); err != nil {
		return nil, fmt.Errorf("write temp pdf: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin, "-f", "1", "-singlefile", "-png", inPath, outPrefix)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("pdf render failed: %s", msg)
	}
	png, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read rendered pdf page: %w", err)
	}
	return png, nil
}

func extractPDFText(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	// Best-effort extraction from PDF text operators when files are text PDFs.
	raw := string(content)
	if len(raw) > 5*1024*1024 {
		raw = raw[:5*1024*1024]
	}
	chunks := make([]string, 0, 16)
	for _, m := range pdfTjRE.FindAllStringSubmatch(raw, -1) {
		if len(m) > 1 {
			chunks = append(chunks, decodePDFString(m[1]))
		}
	}
	for _, m := range pdfTjArrayRE.FindAllStringSubmatch(raw, -1) {
		if len(m) < 2 {
			continue
		}
		for _, inner := range pdfArrayStrRE.FindAllStringSubmatch(m[1], -1) {
			if len(inner) > 1 {
				chunks = append(chunks, decodePDFString(inner[1]))
			}
		}
	}
	text := normalizeWhitespace(strings.Join(chunks, " "))
	if len(text) < 24 {
		return ""
	}
	if len(text) > 2000 {
		return text[:2000] + "..."
	}
	return text
}

func decodePDFString(v string) string {
	v = strings.ReplaceAll(v, `\(`, "(")
	v = strings.ReplaceAll(v, `\)`, ")")
	v = strings.ReplaceAll(v, `\\`, `\`)
	v = strings.ReplaceAll(v, `\n`, " ")
	v = strings.ReplaceAll(v, `\r`, " ")
	v = strings.ReplaceAll(v, `\t`, " ")
	return strings.ToValidUTF8(v, " ")
}

func normalizeMIME(v string) string {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(v))
	if err == nil && mt != "" {
		return strings.ToLower(mt)
	}
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "application/octet-stream"
	}
	return v
}

func isTextLike(mimeType string) bool {
	if strings.HasPrefix(mimeType, "text/") {
		return true
	}
	switch mimeType {
	case "application/json", "application/xml", "application/x-yaml", "application/yaml", "text/csv":
		return true
	default:
		return false
	}
}

func summarizeTextLike(name, mimeType string, content []byte) Summary {
	text := strings.TrimSpace(strings.ToValidUTF8(string(content), " "))
	text = normalizeWhitespace(text)
	if text == "" {
		text = "(attachment text is empty or unreadable)"
	}
	if len(text) > 1200 {
		text = text[:1200] + "..."
	}
	return Summary{
		Filename:   name,
		MIME:       mimeType,
		Source:     "text-extract",
		Summary:    text,
		Confidence: 0.7,
	}
}

type openAICompatReq struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Temperature float64         `json:"temperature"`
}

type openAIMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type userPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type openAICompatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type responsesReq struct {
	Model string `json:"model"`
	Input []struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			ImageURL string `json:"image_url,omitempty"`
		} `json:"content"`
	} `json:"input"`
}

type responsesResp struct {
	OutputText string `json:"output_text"`
	Output     []struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

type visionSummary struct {
	Summary    string   `json:"summary"`
	KeyPoints  []string `json:"key_points"`
	Confidence float64  `json:"confidence"`
}

type visionHTTPError struct {
	Status int
	Body   string
}

func (e visionHTTPError) Error() string {
	return fmt.Sprintf("status %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (o *OpenAICompat) summarizeVisionWithRetries(ctx context.Context, filename, mimeType string, content []byte) (Summary, error) {
	if strings.TrimSpace(o.ModelName) == "" {
		return Summary{}, fmt.Errorf("VISION_MODEL_NAME is empty")
	}
	var lastErr error
	for attempt := 0; attempt <= o.Config.MaxRetries; attempt++ {
		out, err := o.summarizeVisionOnce(ctx, filename, mimeType, content)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if attempt == o.Config.MaxRetries || !isRetryableVisionError(err) {
			break
		}
		backoff := time.Duration(250*(1<<attempt)) * time.Millisecond
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Summary{}, ctx.Err()
		case <-timer.C:
		}
	}
	return Summary{}, lastErr
}

func isRetryableVisionError(err error) bool {
	if err == nil {
		return false
	}
	var he visionHTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusTooManyRequests || he.Status >= 500
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (o *OpenAICompat) summarizeVisionOnce(ctx context.Context, filename, mimeType string, content []byte) (Summary, error) {
	mimeType, content, err := o.preprocessImage(mimeType, content)
	if err != nil {
		return Summary{}, err
	}
	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(content))
	prompt := fmt.Sprintf("Summarize this attachment for an email triage operator.\nFilename: %s\nReturn strict JSON with keys summary (string), key_points (array up to 5 strings), confidence (0..1). Be concise and factual.", filename)
	mode := strings.ToLower(strings.TrimSpace(o.Config.ProviderMode))

	switch mode {
	case providerModeChat:
		return o.callChatCompletions(ctx, filename, mimeType, dataURL, prompt)
	case providerModeResponses:
		return o.callResponses(ctx, filename, mimeType, dataURL, prompt)
	default:
		// Auto mode: try chat first, then responses for provider compatibility.
		if out, err := o.callChatCompletions(ctx, filename, mimeType, dataURL, prompt); err == nil {
			return out, nil
		} else {
			out2, err2 := o.callResponses(ctx, filename, mimeType, dataURL, prompt)
			if err2 == nil {
				return out2, nil
			}
			return Summary{}, fmt.Errorf("chat failed (%v); responses failed (%v)", err, err2)
		}
	}
}

func (o *OpenAICompat) preprocessImage(mimeType string, content []byte) (string, []byte, error) {
	if !strings.HasPrefix(mimeType, "image/") {
		return mimeType, content, nil
	}
	if int64(len(content)) <= o.Config.MaxImageBytes {
		return mimeType, content, nil
	}
	img, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return "", nil, fmt.Errorf("decode oversized image: %w", err)
	}
	var out bytes.Buffer
	for _, q := range []int{75, 60, 45, 30, 20} {
		out.Reset()
		if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: q}); err != nil {
			return "", nil, fmt.Errorf("re-encode image: %w", err)
		}
		if int64(out.Len()) <= o.Config.MaxImageBytes {
			return "image/jpeg", out.Bytes(), nil
		}
	}
	return "", nil, fmt.Errorf("image remains too large after compression (%d bytes > %d)", out.Len(), o.Config.MaxImageBytes)
}

func (o *OpenAICompat) callChatCompletions(ctx context.Context, filename, mimeType, dataURL, prompt string) (Summary, error) {
	payload, _ := json.Marshal(openAICompatReq{
		Model: o.ModelName,
		Messages: []openAIMessage{
			{Role: "system", Content: "You analyze email attachments and return only valid JSON."},
			{
				Role: "user",
				Content: []userPart{
					{Type: "text", Text: prompt},
					{Type: "image_url", ImageURL: &imageURL{URL: dataURL}},
				},
			},
		},
		Temperature: 0,
	})
	body, err := o.callModelEndpoint(ctx, "/v1/chat/completions", payload)
	if err != nil {
		return Summary{}, err
	}
	var out openAICompatResp
	if err := json.Unmarshal(body, &out); err != nil {
		return Summary{}, fmt.Errorf("decode chat envelope: %w", err)
	}
	if len(out.Choices) == 0 {
		return Summary{}, fmt.Errorf("chat response missing choices")
	}
	return parseVisionSummary(filename, mimeType, out.Choices[0].Message.Content, "vision-model/chat")
}

func (o *OpenAICompat) callResponses(ctx context.Context, filename, mimeType, dataURL, prompt string) (Summary, error) {
	reqBody := responsesReq{
		Model: o.ModelName,
		Input: []struct {
			Role    string `json:"role"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text,omitempty"`
				ImageURL string `json:"image_url,omitempty"`
			} `json:"content"`
		}{
			{
				Role: "user",
				Content: []struct {
					Type     string `json:"type"`
					Text     string `json:"text,omitempty"`
					ImageURL string `json:"image_url,omitempty"`
				}{
					{Type: "input_text", Text: prompt},
					{Type: "input_image", ImageURL: dataURL},
				},
			},
		},
	}
	payload, _ := json.Marshal(reqBody)
	body, err := o.callModelEndpoint(ctx, "/v1/responses", payload)
	if err != nil {
		return Summary{}, err
	}
	var out responsesResp
	if err := json.Unmarshal(body, &out); err != nil {
		return Summary{}, fmt.Errorf("decode responses envelope: %w", err)
	}
	content := strings.TrimSpace(out.OutputText)
	if content == "" {
		for _, item := range out.Output {
			for _, c := range item.Content {
				if strings.TrimSpace(c.Text) != "" {
					content = c.Text
					break
				}
			}
			if content != "" {
				break
			}
		}
	}
	if content == "" {
		return Summary{}, fmt.Errorf("responses output missing text")
	}
	return parseVisionSummary(filename, mimeType, content, "vision-model/responses")
}

func (o *OpenAICompat) callModelEndpoint(ctx context.Context, endpoint string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, visionHTTPError{Status: resp.StatusCode, Body: string(body)}
	}
	return body, nil
}

func parseVisionSummary(filename, mimeType, content, source string) (Summary, error) {
	var parsed visionSummary
	raw := extractJSON(content)
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return Summary{}, fmt.Errorf("parse attachment json: %w", err)
	}
	summaryText := strings.TrimSpace(parsed.Summary)
	if len(parsed.KeyPoints) > 0 {
		summaryText = strings.TrimSpace(summaryText + " Key points: " + strings.Join(parsed.KeyPoints, "; "))
	}
	if summaryText == "" {
		return Summary{}, fmt.Errorf("empty summary")
	}
	if parsed.Confidence < 0 {
		parsed.Confidence = 0
	}
	if parsed.Confidence > 1 {
		parsed.Confidence = 1
	}
	return Summary{
		Filename:   filename,
		MIME:       mimeType,
		Source:     source,
		Summary:    summaryText,
		Confidence: parsed.Confidence,
	}, nil
}

func normalizeWhitespace(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

func extractJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(raw, "json") {
			raw = strings.TrimSpace(strings.TrimPrefix(raw, "json"))
		}
		if idx := strings.LastIndex(raw, "```"); idx >= 0 {
			raw = raw[:idx]
		}
		return strings.TrimSpace(raw)
	}
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			return strings.TrimSpace(raw[i : j+1])
		}
	}
	return raw
}
