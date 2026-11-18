package attachmentstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type S3Config struct {
	Endpoint           string
	Region             string
	Bucket             string
	AccessKey          string
	SecretKey          string
	Prefix             string
	PathStyle          bool
	InsecureSkipVerify bool
}

type S3Store struct {
	cfg      S3Config
	endpoint *url.URL
	client   *http.Client
}

func NewS3(cfg S3Config) (*S3Store, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("s3 endpoint is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("s3 bucket is required")
	}
	if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, fmt.Errorf("s3 access and secret keys are required")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		cfg.Region = "us-east-1"
	}
	ep, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse s3 endpoint: %w", err)
	}
	if ep.Scheme == "" {
		ep.Scheme = "https"
	}
	if ep.Host == "" {
		return nil, fmt.Errorf("invalid s3 endpoint host")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if ep.Scheme == "https" && cfg.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &S3Store{
		cfg:      cfg,
		endpoint: ep,
		client:   &http.Client{Transport: transport, Timeout: 60 * time.Second},
	}, nil
}

func (s *S3Store) Put(ctx context.Context, key string, content []byte, contentType string) (string, error) {
	objectKey := strings.TrimPrefix(strings.TrimSpace(key), "/")
	if s.cfg.Prefix != "" {
		objectKey = strings.TrimPrefix(path.Join(s.cfg.Prefix, objectKey), "/")
	}
	if objectKey == "" {
		return "", fmt.Errorf("empty object key")
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	reqURL, host, canonicalURI := s.requestTarget(objectKey)
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	shortDate := amzDate[:8]
	payloadHash := hexSHA256(content)
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalReq := strings.Join([]string{
		http.MethodPut,
		canonicalURI,
		"",
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", shortDate, s.cfg.Region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalReq)),
	}, "\n")
	signature := s.sign(shortDate, stringToSign)
	authorization := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKey, scope, signedHeaders, signature,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, bytes.NewReader(content))
	if err != nil {
		return "", err
	}
	req.Header.Set("Host", host)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Authorization", authorization)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("s3 put failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return fmt.Sprintf("s3://%s/%s", s.cfg.Bucket, objectKey), nil
}

func (s *S3Store) requestTarget(objectKey string) (requestURL string, host string, canonicalURI string) {
	basePath := strings.TrimSuffix(s.endpoint.EscapedPath(), "/")
	if s.cfg.PathStyle {
		host = s.endpoint.Host
		objectPath := "/" + encodePath(path.Join(s.cfg.Bucket, objectKey))
		canonicalURI = basePath + objectPath
		if canonicalURI == "" {
			canonicalURI = "/"
		}
		u := *s.endpoint
		u.Path = canonicalURI
		return u.String(), host, canonicalURI
	}
	host = s.cfg.Bucket + "." + s.endpoint.Host
	objectPath := "/" + encodePath(objectKey)
	canonicalURI = basePath + objectPath
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	u := *s.endpoint
	u.Host = host
	u.Path = canonicalURI
	return u.String(), host, canonicalURI
}

func (s *S3Store) sign(shortDate, stringToSign string) string {
	dateKey := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), shortDate)
	regionKey := hmacSHA256(dateKey, s.cfg.Region)
	serviceKey := hmacSHA256(regionKey, "s3")
	signingKey := hmacSHA256(serviceKey, "aws4_request")
	return hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
}

func hmacSHA256(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(msg))
	return mac.Sum(nil)
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func encodePath(v string) string {
	parts := strings.Split(strings.TrimPrefix(v, "/"), "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(url.PathEscape(p), "+", "%20")
	}
	return strings.Join(parts, "/")
}
