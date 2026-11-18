package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	Mailbox string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func NewForMailbox(baseURL, token, mailbox string) *Client {
	c := New(baseURL, token)
	c.Mailbox = strings.TrimSpace(mailbox)
	return c
}

func (c *Client) requestURL(path string, query map[string]string) (string, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", fmt.Errorf("invalid email API URL")
	}
	rel, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(rel.Path, "/") || rel.IsAbs() || rel.Host != "" {
		return "", fmt.Errorf("invalid email API path")
	}
	// Legacy deployments may mount the API below a reverse-proxy prefix.
	// Join validated URLs without ResolveReference, which discards that prefix
	// for the absolute API paths used by the client.
	u, err := url.Parse(strings.TrimRight(base.String(), "/") + rel.String())
	if err != nil {
		return "", fmt.Errorf("invalid email API path")
	}
	q := u.Query()
	for k, v := range query {
		q.Set(k, v)
	}
	if c.Mailbox != "" {
		q.Set("mailbox", c.Mailbox)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Client) Get(path string, query map[string]string, out any) error {
	requestURL, err := c.requestURL(path, query)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("create email API request: %w", err)
	}
	return c.do(req, out)
}

func (c *Client) Post(path string, in any, out any) error {
	buf := bytes.NewBuffer(nil)
	if in != nil {
		if err := json.NewEncoder(buf).Encode(in); err != nil {
			return err
		}
	}
	requestURL, err := c.requestURL(path, nil)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, requestURL, buf)
	if err != nil {
		return fmt.Errorf("create email API request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}
