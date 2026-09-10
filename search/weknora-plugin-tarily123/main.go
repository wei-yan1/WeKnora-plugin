package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
)

const (
	// defaultTarilySearchURL is the Tavily Search API endpoint used when the
	// host does not provide a base_url override.
	defaultTarilySearchURL = "https://api.tavily.com/search"
	// defaultTimeout bounds the outbound HTTP call, mirroring the built-in
	// Tavily provider's timeout.
	defaultTimeout = 15 * time.Second
	// defaultMaxResults is applied when the host sends a non-positive limit.
	defaultMaxResults = 5
	// sourceName is the honest origin label surfaced to the host for citation.
	sourceName = "TARily123"
)

// tarilySearchRequest mirrors the Tavily Search API request body.
type tarilySearchRequest struct {
	APIKey     string `json:"api_key"`
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

// tarilySearchResponse mirrors the Tavily Search API response body.
type tarilySearchResponse struct {
	Query   string `json:"query"`
	Results []struct {
		Title         string  `json:"title"`
		URL           string  `json:"url"`
		Content       string  `json:"content"`
		Score         float64 `json:"score"`
		PublishedDate string  `json:"published_date,omitempty"`
	} `json:"results"`
}

func main() {
	address := flag.String("address", os.Getenv("WEKNORA_PLUGIN_ADDR"), "gRPC listen address")
	flag.Parse()
	if *address == "" {
		*address = "127.0.0.1:9775"
	}

	handler := pluginapi.WebSearchHandler{
		PluginID:     "weknora.tarily123",
		Capabilities: []string{"search"},
		OnSearch:     search,
	}
	if err := pluginapi.ServeWebSearch(context.Background(), *address, handler); err != nil {
		panic(err)
	}
}

// search performs a web search against the Tavily API. The API key, base URL,
// and proxy URL are all forwarded per-request by the host, so the plugin keeps
// no tenant state and can be safely restarted.
func search(ctx context.Context, req pluginapi.WebSearchRequest) (pluginapi.WebSearchResponse, error) {
	if req.Query == "" {
		return pluginapi.WebSearchResponse{}, fmt.Errorf("query is required")
	}
	if req.APIKey == "" {
		return pluginapi.WebSearchResponse{}, fmt.Errorf("api_key is required")
	}

	baseURL := req.BaseURL
	if baseURL == "" {
		baseURL = defaultTarilySearchURL
	}

	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}

	// Use the SDK guarded client so outbound dials honor the host-injected
	// network policy (allowlist + anti-SSRF), rather than a raw http.Client.
	client := pluginapi.NewPluginHTTPClient()
	client.Timeout = defaultTimeout
	if req.ProxyURL != "" {
		proxy, err := url.Parse(req.ProxyURL)
		if err != nil {
			return pluginapi.WebSearchResponse{}, fmt.Errorf("invalid proxy_url: %w", err)
		}
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.Proxy = http.ProxyURL(proxy)
		}
	}

	body := tarilySearchRequest{
		APIKey:     req.APIKey,
		Query:      req.Query,
		MaxResults: maxResults,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return pluginapi.WebSearchResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return pluginapi.WebSearchResponse{}, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return pluginapi.WebSearchResponse{}, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return pluginapi.WebSearchResponse{}, fmt.Errorf("tavily API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return pluginapi.WebSearchResponse{}, fmt.Errorf("read response: %w", err)
	}

	var data tarilySearchResponse
	if err := json.Unmarshal(respBody, &data); err != nil {
		return pluginapi.WebSearchResponse{}, fmt.Errorf("unmarshal response: %w", err)
	}

	results := make([]*pluginapi.WebSearchResult, 0, len(data.Results))
	for _, item := range data.Results {
		result := &pluginapi.WebSearchResult{
			Title:   item.Title,
			URL:     item.URL,
			Snippet: item.Content,
			Content: item.Content,
			Source:  sourceName,
		}
		if req.IncludeDate && item.PublishedDate != "" {
			result.PublishedAt = item.PublishedDate
		}
		results = append(results, result)
	}
	return pluginapi.WebSearchResponse{Results: results}, nil
}

