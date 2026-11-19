package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// clip is one row as the clip server returns it (server/clip/store.go Clip).
type clip struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	Device    string `json:"device"`
	CreatedAt int64  `json:"created_at"` // unix milliseconds
	BumpCount int64  `json:"bump_count"`
}

// clipMaxLimit is the clip server's hard cap on ?limit= (server/clip MaxLimit).
// get_clip asks for exactly this many rows; see getClip.
const clipMaxLimit = 500

// clipClient speaks the clip server's HTTP API. It holds the token and puts it
// in exactly one place: the Authorization header.
type clipClient struct {
	base  string
	token string
	http  *http.Client
}

func newClipClient(cfg config) *clipClient {
	return &clipClient{base: cfg.ClipURL, token: cfg.Token, http: &http.Client{Timeout: 15 * time.Second}}
}

// clipError is a non-2xx answer from the clip server, carrying its stable
// error code so the agent sees "400 invalid_limit: …" rather than a bare
// status.
type clipError struct {
	Status int
	Code   string
	Detail string
}

func (e *clipError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("clip server answered HTTP %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("clip server answered HTTP %d %s: %s", e.Status, e.Code, e.Detail)
}

// maxErrorDetail bounds how much of a non-JSON error body is relayed. A
// misconfigured URL can point at anything, and an HTML page is not useful to
// an agent beyond its first line.
const maxErrorDetail = 300

func (c *clipClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("clip server unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("reading clip server response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return &clipError{Status: resp.StatusCode, Code: e.Error, Detail: e.Detail}
		}
		// 🔴 REDACT BEFORE TRUNCATING. The stream-level redaction in redact.go
		// matches the WHOLE token; a cut landing inside a token would leave a
		// prefix of it that nothing downstream recognises.
		// TestTruncatedErrorBodyDoesNotLeakATokenPrefix pins the order.
		d := string(redactBytes(raw, []byte(c.token)))
		if len(d) > maxErrorDetail {
			d = strings.ToValidUTF8(d[:maxErrorDetail], "") + "…"
		}
		return &clipError{Status: resp.StatusCode, Detail: d}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("clip server answered %d with a body that is not the expected JSON (is the URL the clip server?)", resp.StatusCode)
	}
	return nil
}

type listResponse struct {
	// Clips is a pointer so that a 200 WITHOUT a clips array — something that
	// is not the clip server — is an error here instead of an empty list. The
	// clip server never omits it ("fails loud, never empty").
	Clips *[]clip `json:"clips"`
}

func (c *clipClient) list(ctx context.Context, limit int, q string) ([]clip, error) {
	v := url.Values{}
	v.Set("limit", strconv.Itoa(limit))
	if q != "" {
		v.Set("q", q)
	}
	var r listResponse
	if err := c.do(ctx, http.MethodGet, "/v1/clips?"+v.Encode(), nil, &r); err != nil {
		return nil, err
	}
	if r.Clips == nil {
		return nil, fmt.Errorf("clip server answered without a \"clips\" array (is the URL the clip server?)")
	}
	return *r.Clips, nil
}

type pushResponse struct {
	clip
	Deduped bool `json:"deduped"`
}

func (c *clipClient) push(ctx context.Context, body, device string) (pushResponse, error) {
	var r pushResponse
	err := c.do(ctx, http.MethodPost, "/v1/clips", map[string]string{"body": body, "device": device}, &r)
	if err == nil && r.ID == 0 {
		err = fmt.Errorf("clip server accepted the push but returned no clip id (is the URL the clip server?)")
	}
	return r, err
}
