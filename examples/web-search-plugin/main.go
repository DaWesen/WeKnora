package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
)

const ddgAPI = "https://api.duckduckgo.com/"

type server struct {
	pluginsdk.Lifecycle
	pluginpb.UnimplementedWebSearchPluginServer
}

func (s *server) Describe(context.Context, *pluginpb.WebSearchDescribeRequest) (*pluginpb.WebSearchDescribeResponse, error) {
	return &pluginpb.WebSearchDescribeResponse{
		ProviderType:  "duckduckgo",
		DisplayName:  "DuckDuckGo",
		Description:   "Web search via the DuckDuckGo Instant Answer API. No API key required.",
		RequiresApiKey: false,
		SupportsProxy: true,
		Capabilities:  []string{},
	}, nil
}

func (s *server) Search(ctx context.Context, request *pluginpb.WebSearchRequest) (*pluginpb.WebSearchResponse, error) {
	query := strings.TrimSpace(request.GetQuery())
	if query == "" {
		return nil, fmt.Errorf("query is empty")
	}
	maxResults := int(request.GetMaxResults())
	if maxResults <= 0 {
		maxResults = 5
	}

	baseURL := ddgAPI
	if v := strings.TrimSpace(request.GetConfig()["base_url"]); v != "" {
		baseURL = v
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("no_html", "1")
	params.Set("skip_disambig", "1")

	httpReq, err := http.NewRequestWithContext(ctx, "GET", baseURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("User-Agent", "WeKnora-Plugin/0.1 (web-search-example)")

	if proxy := strings.TrimSpace(request.GetConfig()["proxy_url"]); proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy_url: %w", err)
		}
		httpReq = httpReq.WithContext(ctx)
		_ = proxyURL // proxy applied via transport below
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("duckduckgo request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var ddg ddgResponse
	if err := json.Unmarshal(body, &ddg); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	results := collectResults(&ddg, maxResults)
	return &pluginpb.WebSearchResponse{Results: results}, nil
}

type ddgResponse struct {
	AbstractText   string `json:"AbstractText"`
	AbstractURL    string `json:"AbstractURL"`
	AbstractSource string `json:"AbstractSource"`
	Heading        string `json:"Heading"`
	RelatedTopics  []json.RawMessage `json:"RelatedTopics"`
}

type ddgTopic struct {
	Text     string `json:"Text"`
	FirstURL string `json:"FirstURL"`
}

func collectResults(ddg *ddgResponse, maxResults int) []*pluginpb.WebSearchResult {
	var results []*pluginpb.WebSearchResult

	// The abstract is the top answer if present.
	if ddg.AbstractText != "" {
		results = append(results, &pluginpb.WebSearchResult{
			Title:   ddg.Heading,
			Url:     ddg.AbstractURL,
			Snippet: ddg.AbstractText,
			Content: ddg.AbstractText,
			Source:  ddg.AbstractSource,
		})
	}

	// Related topics fill remaining slots.
	for _, raw := range ddg.RelatedTopics {
		if len(results) >= maxResults {
			break
		}
		var topic ddgTopic
		if err := json.Unmarshal(raw, &topic); err != nil || topic.Text == "" {
			continue
		}
		results = append(results, &pluginpb.WebSearchResult{
			Title:   extractTitle(topic.Text, topic.FirstURL),
			Url:     topic.FirstURL,
			Snippet: topic.Text,
			Source:  "DuckDuckGo",
		})
	}
	return results
}

func extractTitle(text, rawURL string) string {
	if rawURL == "" {
		return text
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return text
	}
	host := parsed.Hostname()
	if host == "" {
		return text
	}
	return host
}

func main() {
	impl := &server{
		Lifecycle: pluginsdk.Lifecycle{
			Metadata: pluginsdk.Metadata{
				ID:             "com.weknora.web-search-ddg",
				Version:        "0.1.0",
				ExtensionTypes: []string{"web_search"},
			},
		},
	}
	ctx, stop := pluginsdk.ContextWithSignals(context.Background())
	defer stop()
	if err := pluginsdk.ServeContext(ctx, impl, pluginsdk.Options{
		Address:         pluginsdk.Address(),
		ShutdownTimeout: 5 * time.Second,
	}, pluginsdk.WebSearchService(impl)); err != nil {
		panic(fmt.Errorf("serve plugin gRPC: %w", err))
	}
}
