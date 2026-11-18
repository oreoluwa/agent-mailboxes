package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	ModelName() string
}

type OpenAICompat struct {
	BaseURL string
	Model   string
	APIKey  string
	Client  *http.Client
}

type BatchEmbedder interface {
	Embedder
	SubmitEmbeddingBatch(ctx context.Context, jobs []BatchEmbeddingJob) (string, error)
	GetBatchStatus(ctx context.Context, batchID string) (BatchStatus, error)
	GetBatchEmbeddingResults(ctx context.Context, outputFileID string) (map[string][]float32, map[string]string, error)
}

type BatchEmbeddingJob struct {
	JobID string
	Input string
}

type BatchStatus struct {
	ID           string
	Status       string
	OutputFileID string
	ErrorFileID  string
}

type openAICompatReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openAICompatResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

type openAIFileResp struct {
	ID string `json:"id"`
}

type openAIBatchResp struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	OutputFileID string `json:"output_file_id"`
	ErrorFileID  string `json:"error_file_id"`
}

func NewOpenAICompat(baseURL, model, apiKey string) *OpenAICompat {
	baseURL = strings.TrimRight(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	return &OpenAICompat{
		BaseURL: baseURL,
		Model:   strings.TrimSpace(model),
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 45 * time.Second},
	}
}

func (o *OpenAICompat) ModelName() string {
	return o.Model
}

func (o *OpenAICompat) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if strings.TrimSpace(o.Model) == "" {
		return nil, fmt.Errorf("embedding model is not configured")
	}
	payload, _ := json.Marshal(openAICompatReq{
		Model: o.Model,
		Input: inputs,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/v1/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	o.applyAuthHeaders(req)
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openai-compatible status %d: %s", resp.StatusCode, string(body))
	}
	var out openAICompatResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode embeddings envelope: %w", err)
	}
	if len(out.Data) != len(inputs) {
		return nil, fmt.Errorf("embeddings count mismatch: got=%d want=%d", len(out.Data), len(inputs))
	}
	vectors := make([][]float32, 0, len(out.Data))
	for _, d := range out.Data {
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embedding response contains empty vector")
		}
		vectors = append(vectors, d.Embedding)
	}
	return vectors, nil
}

func (o *OpenAICompat) SubmitEmbeddingBatch(ctx context.Context, jobs []BatchEmbeddingJob) (string, error) {
	if strings.TrimSpace(o.Model) == "" {
		return "", fmt.Errorf("embedding model is not configured")
	}
	if len(jobs) == 0 {
		return "", fmt.Errorf("no jobs to submit")
	}
	var jsonl bytes.Buffer
	for _, j := range jobs {
		req := map[string]any{
			"custom_id": j.JobID,
			"method":    "POST",
			"url":       "/v1/embeddings",
			"body": map[string]any{
				"model": o.Model,
				"input": j.Input,
			},
		}
		line, _ := json.Marshal(req)
		jsonl.Write(line)
		jsonl.WriteByte('\n')
	}
	fileID, err := o.uploadBatchFile(ctx, jsonl.Bytes())
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]any{
		"input_file_id":     fileID,
		"endpoint":          "/v1/embeddings",
		"completion_window": "24h",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/v1/batches", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	o.applyAuthHeaders(req)
	resp, err := o.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("openai-compatible batches status %d: %s", resp.StatusCode, string(body))
	}
	var out openAIBatchResp
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode batches envelope: %w", err)
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", fmt.Errorf("batch response missing id")
	}
	return out.ID, nil
}

func (o *OpenAICompat) GetBatchStatus(ctx context.Context, batchID string) (BatchStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.BaseURL+"/v1/batches/"+batchID, nil)
	if err != nil {
		return BatchStatus{}, err
	}
	o.applyAuthHeaders(req)
	resp, err := o.Client.Do(req)
	if err != nil {
		return BatchStatus{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return BatchStatus{}, fmt.Errorf("openai-compatible batch get status %d: %s", resp.StatusCode, string(body))
	}
	var out openAIBatchResp
	if err := json.Unmarshal(body, &out); err != nil {
		return BatchStatus{}, fmt.Errorf("decode batch response: %w", err)
	}
	return BatchStatus{
		ID:           out.ID,
		Status:       out.Status,
		OutputFileID: out.OutputFileID,
		ErrorFileID:  out.ErrorFileID,
	}, nil
}

func (o *OpenAICompat) GetBatchEmbeddingResults(ctx context.Context, outputFileID string) (map[string][]float32, map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.BaseURL+"/v1/files/"+outputFileID+"/content", nil)
	if err != nil {
		return nil, nil, err
	}
	o.applyAuthHeaders(req)
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("openai-compatible file content status %d: %s", resp.StatusCode, string(body))
	}
	success := map[string][]float32{}
	failures := map[string]string{}
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row struct {
			CustomID string `json:"custom_id"`
			Error    *struct {
				Message string `json:"message"`
			} `json:"error"`
			Response *struct {
				StatusCode int `json:"status_code"`
				Body       struct {
					Data []struct {
						Embedding []float32 `json:"embedding"`
					} `json:"data"`
				} `json:"body"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, nil, fmt.Errorf("parse batch output jsonl: %w", err)
		}
		if row.CustomID == "" {
			continue
		}
		if row.Error != nil {
			failures[row.CustomID] = strings.TrimSpace(row.Error.Message)
			continue
		}
		if row.Response == nil || row.Response.StatusCode >= 300 || len(row.Response.Body.Data) == 0 || len(row.Response.Body.Data[0].Embedding) == 0 {
			failures[row.CustomID] = "missing or invalid embedding response"
			continue
		}
		success[row.CustomID] = row.Response.Body.Data[0].Embedding
	}
	return success, failures, nil
}

func (o *OpenAICompat) uploadBatchFile(ctx context.Context, content []byte) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("purpose", "batch"); err != nil {
		return "", err
	}
	fw, err := writer.CreateFormFile("file", "embedding-batch.jsonl")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(content); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/v1/files", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	o.applyAuthHeaders(req)
	resp, err := o.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("openai-compatible files status %d: %s", resp.StatusCode, string(respBody))
	}
	var out openAIFileResp
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode files response: %w", err)
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", fmt.Errorf("file upload response missing id")
	}
	return out.ID, nil
}

func (o *OpenAICompat) applyAuthHeaders(req *http.Request) {
	if o.APIKey == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
}
