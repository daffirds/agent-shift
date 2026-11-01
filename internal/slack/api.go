// Package slack is a small Slack Web API + Socket Mode client tailored to acting as a user:
// it uses a user token (xoxp) for API calls and an app-level token (xapp) for events.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiBase = "https://slack.com/api/"

// APIError is a Slack `ok: false` response.
type APIError struct {
	Method   string
	Code     string
	Needed   string
	Provided string
	Warnings []string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("slack %s: %s", e.Method, e.Code)
	if e.Needed != "" {
		s += fmt.Sprintf(" (needs scope %s)", e.Needed)
	}
	return s
}

// IsCode reports whether err is a Slack API error with one of the given codes.
func IsCode(err error, codes ...string) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.Code == c {
			return true
		}
	}
	return false
}

// Client calls the Slack Web API with a single token.
type Client struct {
	token string
	http  *http.Client
}

func NewClient(token string) *Client {
	return &Client{token: token, http: &http.Client{Timeout: 60 * time.Second}}
}

type baseResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Needed   string `json:"needed"`
	Provided string `json:"provided"`
	Metadata struct {
		Warnings []string `json:"warnings"`
	} `json:"response_metadata"`
}

// Call invokes a Web API method. params is either url.Values (form-encoded) or any
// JSON-marshalable value (sent as application/json). out receives the decoded response.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	for attempt := 0; ; attempt++ {
		body, err := c.do(ctx, method, params)
		var rl *rateLimited
		if errors.As(err, &rl) && attempt < 5 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(rl.wait):
				continue
			}
		}
		if err != nil {
			return err
		}
		var base baseResponse
		if err := json.Unmarshal(body, &base); err != nil {
			return fmt.Errorf("slack %s: decode: %w", method, err)
		}
		if !base.OK {
			return &APIError{Method: method, Code: base.Error, Needed: base.Needed, Provided: base.Provided, Warnings: base.Metadata.Warnings}
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("slack %s: decode: %w", method, err)
			}
		}
		return nil
	}
}

type rateLimited struct{ wait time.Duration }

func (r *rateLimited) Error() string { return "rate limited" }

func (c *Client) do(ctx context.Context, method string, params any) ([]byte, error) {
	var (
		body        io.Reader
		contentType string
	)
	switch p := params.(type) {
	case nil:
		contentType = "application/x-www-form-urlencoded"
		body = strings.NewReader("")
	case url.Values:
		contentType = "application/x-www-form-urlencoded"
		body = strings.NewReader(p.Encode())
	default:
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		contentType = "application/json; charset=utf-8"
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+method, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		wait := 5 * time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			wait = time.Duration(s)*time.Second + 250*time.Millisecond
		}
		return nil, &rateLimited{wait: wait}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("slack %s: http %d: %s", method, resp.StatusCode, truncate(string(data), 200))
	}
	return data, nil
}

// Scopes returns the OAuth scopes granted to the token (auth.test's X-OAuth-Scopes header).
func (c *Client) Scopes(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"auth.test", strings.NewReader(""))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	var out []string
	for _, s := range strings.Split(resp.Header.Get("X-OAuth-Scopes"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// Download fetches a private file URL (url_private / url_private_download) with the token.
func (c *Client) Download(ctx context.Context, fileURL string, w io.Writer, limit int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download: http %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		// Slack serves its login page instead of the file when the token lacks files:read.
		return 0, errors.New("download: got an HTML page instead of the file (missing files:read scope?)")
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("download: file is larger than %d bytes", limit)
	}
	return n, err
}

// upload PUTs raw bytes to a pre-signed upload URL (files.getUploadURLExternal).
func (c *Client) upload(ctx context.Context, uploadURL string, r io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload: http %d: %s", resp.StatusCode, b)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
