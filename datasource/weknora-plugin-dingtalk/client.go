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
	"sync"
	"time"
)

const (
	workspacePageSize = 30
	nodePageSize       = 50
	blockPageSize      = 100
	maxPaginationPages = 1000
	maxTransientRetries = 3
	maxRetryAfter       = 30 * time.Second
	maxResponseBytes    = 16 * 1024 * 1024
)

// dingtalkClient wraps HTTP access to the DingTalk API with token caching,
// pagination, and retry.
type dingtalkClient struct {
	http *http.Client

	tokenMu     sync.Mutex
	token       string
	tokenExpiry time.Time
}

func newDingtalkClient(httpClient *http.Client) *dingtalkClient {
	return &dingtalkClient{http: httpClient}
}

// accessToken returns a cached access token, refreshing when expired.
//
// The token endpoint itself must NOT be fetched through do(), because do()
// calls accessToken() to obtain a token — that would deadlock on tokenMu.
// We therefore call requestOnce directly here, without authentication.
func (c *dingtalkClient) accessToken(ctx context.Context, cfg *dingtalkConfig) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	body, _ := json.Marshal(map[string]string{
		"appKey":    cfg.AppKey,
		"appSecret": cfg.AppSecret,
	})
	status, resp, err := c.requestOnce(ctx, http.MethodPost, "/v1.0/oauth2/accessToken", nil, body, "")
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", fmt.Errorf("access token: status %d: %s", status, truncate(string(resp), 200))
	}
	var out accessTokenResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return "", fmt.Errorf("decode access token: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("no access token, code=%s message=%s", out.Code, out.Message)
	}
	c.token = out.AccessToken
	expire := out.ExpireIn
	if expire <= 0 {
		expire = 7200
	}
	c.tokenExpiry = time.Now().Add(time.Duration(expire)*time.Second - 5*time.Minute)
	return c.token, nil
}

func (c *dingtalkClient) invalidateToken() {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.token = ""
	c.tokenExpiry = time.Time{}
}

// do performs an authenticated request with transient retry and one-shot 401
// token refresh. Returns the response body.
func (c *dingtalkClient) do(ctx context.Context, cfg *dingtalkConfig, method, path string, query url.Values, reqBody []byte) ([]byte, error) {
	authRefreshed := false
	for attempt := 0; ; attempt++ {
		token, err := c.accessToken(ctx, cfg)
		if err != nil {
			return nil, err
		}

		status, body, err := c.requestOnce(ctx, method, path, query, reqBody, token)
		if err != nil {
			return nil, err
		}

		switch {
		case status == http.StatusUnauthorized && !authRefreshed:
			// Token may have expired server-side; refresh once and retry.
			c.invalidateToken()
			authRefreshed = true
			continue
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return nil, fmt.Errorf("dingtalk auth error: status %d: %s", status, truncate(string(body), 200))
		case status == http.StatusTooManyRequests:
			// Rate limit: honor retry budget.
			if attempt >= maxTransientRetries {
				return nil, fmt.Errorf("dingtalk rate limited: status %d: %s", status, truncate(string(body), 200))
			}
			if err := sleepContext(ctx, retryDelay(attempt)); err != nil {
				return nil, err
			}
			continue
		case status >= 500:
			// A 5xx with a DingTalk business error code (e.g. "systemError")
			// is a deterministic failure, not transient — return it instead of
			// retrying, which would only slow the sync down.
			if isDingTalkBusinessError(body) {
				return nil, fmt.Errorf("dingtalk business error: status %d: %s", status, truncate(string(body), 200))
			}
			if attempt >= maxTransientRetries {
				return nil, fmt.Errorf("dingtalk transient error: status %d: %s", status, truncate(string(body), 200))
			}
			if err := sleepContext(ctx, retryDelay(attempt)); err != nil {
				return nil, err
			}
			continue
		case status >= 300:
			return nil, fmt.Errorf("dingtalk API error: status %d: %s", status, truncate(string(body), 200))
		default:
			return body, nil
		}
	}
}

// requestOnce performs a single HTTP request (no retry).
func (c *dingtalkClient) requestOnce(ctx context.Context, method, path string, query url.Values, reqBody []byte, token string) (int, []byte, error) {
	full := cfgBaseURL() + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var bodyReader io.Reader
	if reqBody != nil {
		bodyReader = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-dingtalk-access-token", token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("dingtalk request %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if len(body) > maxResponseBytes {
		return 0, nil, fmt.Errorf("dingtalk response exceeds %d bytes", maxResponseBytes)
	}
	return resp.StatusCode, body, nil
}

// listWorkspaces pages through the wiki workspace list.
func (c *dingtalkClient) listWorkspaces(ctx context.Context, cfg *dingtalkConfig) ([]workspace, error) {
	var out []workspace
	nextToken := ""
	seen := map[string]struct{}{}
	for page := 0; page < maxPaginationPages; page++ {
		q := url.Values{}
		q.Set("operatorId", cfg.UnionID)
		q.Set("maxResults", strconv.Itoa(workspacePageSize))
		if nextToken != "" {
			q.Set("nextToken", nextToken)
		}
		body, err := c.do(ctx, cfg, http.MethodGet, "/v2.0/wiki/workspaces", q, nil)
		if err != nil {
			return nil, err
		}
		var resp workspaceListResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decode workspaces: %w", err)
		}
		out = append(out, resp.Workspaces...)
		if resp.NextToken == "" {
			break
		}
		if _, dup := seen[resp.NextToken]; dup {
			return nil, fmt.Errorf("dingtalk workspace pagination cycle")
		}
		seen[resp.NextToken] = struct{}{}
		nextToken = resp.NextToken
	}
	return out, nil
}

// listNodes pages through the direct children of a parent node.
func (c *dingtalkClient) listNodes(ctx context.Context, cfg *dingtalkConfig, parentNodeID string) ([]wikiNode, error) {
	var out []wikiNode
	nextToken := ""
	seen := map[string]struct{}{}
	for page := 0; page < maxPaginationPages; page++ {
		q := url.Values{}
		q.Set("operatorId", cfg.UnionID)
		q.Set("parentNodeId", parentNodeID)
		q.Set("maxResults", strconv.Itoa(nodePageSize))
		if nextToken != "" {
			q.Set("nextToken", nextToken)
		}
		body, err := c.do(ctx, cfg, http.MethodGet, "/v2.0/wiki/nodes", q, nil)
		if err != nil {
			return nil, err
		}
		var resp nodeListResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decode nodes: %w", err)
		}
		out = append(out, resp.Nodes...)
		if resp.NextToken == "" {
			break
		}
		if _, dup := seen[resp.NextToken]; dup {
			return nil, fmt.Errorf("dingtalk node pagination cycle")
		}
		seen[resp.NextToken] = struct{}{}
		nextToken = resp.NextToken
	}
	return out, nil
}

// listDocBlocks pages through an ALIDOC document's blocks using offset
// pagination (startIndex/endIndex).
func (c *dingtalkClient) listDocBlocks(ctx context.Context, cfg *dingtalkConfig, docKey string) ([]json.RawMessage, error) {
	var out []json.RawMessage
	start := 0
	for page := 0; page < maxPaginationPages; page++ {
		end := start + blockPageSize
		q := url.Values{}
		q.Set("operatorId", cfg.UnionID)
		q.Set("startIndex", strconv.Itoa(start))
		q.Set("endIndex", strconv.Itoa(end))

		path := fmt.Sprintf("/v1.0/doc/suites/documents/%s/blocks", url.PathEscape(docKey))
		body, err := c.do(ctx, cfg, http.MethodGet, path, q, nil)
		if err != nil {
			return nil, err
		}
		var resp docBlocksResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decode doc blocks: %w", err)
		}
		out = append(out, resp.Result.Data...)
		if len(resp.Result.Data) < blockPageSize {
			break
		}
		start = end
	}
	return out, nil
}

// isDingTalkBusinessError reports whether an error body carries a DingTalk
// business error code (e.g. "systemError"), which is deterministic and should
// not be retried.
func isDingTalkBusinessError(body []byte) bool {
	var parsed struct {
		Code any `json:"code"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	if code, ok := parsed.Code.(string); ok && strings.TrimSpace(code) != "" {
		return true
	}
	return false
}

func cfgBaseURL() string {
	// The plugin always uses the official DingTalk endpoint; egress policy
	// already restricts outbound traffic to approved hosts.
	return "https://api.dingtalk.com"
}

func retryDelay(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 16*time.Second {
		return 16 * time.Second
	}
	return d
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
