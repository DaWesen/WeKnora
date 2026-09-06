package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// startTestServer starts the plugin's gRPC server on an ephemeral port and
// returns a gRPC client. It mirrors how the host connects to a plugin.
func startTestServer(t *testing.T) (pluginpb.WebSearchPluginClient, func()) {
	t.Helper()
	impl := &server{
		Lifecycle: pluginsdk.Lifecycle{
			Metadata: pluginsdk.Metadata{
				ID:             "com.weknora.web-search-ddg",
				Version:        "0.1.0",
				ExtensionTypes: []string{"web_search"},
			},
		},
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := pluginsdk.New(impl, pluginsdk.WebSearchService(impl))
	go grpcServer.Serve(listener)
	conn, err := grpc.NewClient(
		listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	cleanup := func() { conn.Close(); grpcServer.GracefulStop() }
	return pluginpb.NewWebSearchPluginClient(conn), cleanup
}

func TestDescribeReturnsDuckDuckGoMetadata(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Describe(context.Background(), &pluginpb.WebSearchDescribeRequest{})
	require.NoError(t, err)
	assert.Equal(t, "duckduckgo", response.GetProviderType())
	assert.Equal(t, "DuckDuckGo", response.GetDisplayName())
	assert.False(t, response.GetRequiresApiKey())
	assert.True(t, response.GetSupportsProxy())
}

func TestSearchRejectsEmptyQuery(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	_, err := client.Search(context.Background(), &pluginpb.WebSearchRequest{
		Query: "   ", MaxResults: 5,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query is empty")
}

func TestSearchReturnsResults(t *testing.T) {
	// This test hits the real DuckDuckGo API. Skip if no network.
	conn, err := net.DialTimeout("tcp", "api.duckduckgo.com:443", 3*time.Second)
	if err != nil {
		t.Skip("no network access to api.duckduckgo.com; skipping live search test")
	}
	_ = conn.Close()

	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	response, err := client.Search(ctx, &pluginpb.WebSearchRequest{
		Query: "Go programming language", MaxResults: 3,
	})
	if err != nil {
		t.Skipf("duckduckgo API unavailable: %v", err)
	}
	// DuckDuckGo Instant Answer API may return zero results for some queries;
	// we verify the call chain works, not that results are non-empty.
	for _, result := range response.GetResults() {
		assert.NotEmpty(t, result.GetSnippet(), "result should have a snippet")
	}
}

func TestCollectResultsExtractsAbstractAndTopics(t *testing.T) {
	ddg := &ddgResponse{
		AbstractText:   "Go is a statically typed, compiled programming language.",
		AbstractURL:    "https://en.wikipedia.org/wiki/Go_(programming_language)",
		AbstractSource: "Wikipedia",
		Heading:        "Go (programming language)",
		RelatedTopics: []json.RawMessage{
			json.RawMessage(`{"Text":"Go is efficient - Wikipedia","FirstURL":"https://en.wikipedia.org/wiki/Go"}`),
			json.RawMessage(`{"Text":"Go Blog","FirstURL":"https://go.dev/blog"}`),
			json.RawMessage(`{"not_valid":"missing text"}`),
		},
	}
	results := collectResults(ddg, 5)
	require.Len(t, results, 3, "abstract + 2 valid topics")
	assert.Equal(t, "Go (programming language)", results[0].GetTitle())
	assert.Equal(t, "https://en.wikipedia.org/wiki/Go_(programming_language)", results[0].GetUrl())
	assert.Equal(t, "Wikipedia", results[0].GetSource())
	assert.Equal(t, "en.wikipedia.org", results[1].GetTitle())
	assert.Contains(t, results[1].GetSnippet(), "Go is efficient")
}

func TestCollectResultsRespectsMaxResults(t *testing.T) {
	ddg := &ddgResponse{
		RelatedTopics: []json.RawMessage{
			json.RawMessage(`{"Text":"result 1","FirstURL":"https://a.com"}`),
			json.RawMessage(`{"Text":"result 2","FirstURL":"https://b.com"}`),
			json.RawMessage(`{"Text":"result 3","FirstURL":"https://c.com"}`),
			json.RawMessage(`{"Text":"result 4","FirstURL":"https://d.com"}`),
		},
	}
	results := collectResults(ddg, 2)
	require.Len(t, results, 2)
}

func TestCollectResultsNoData(t *testing.T) {
	ddg := &ddgResponse{}
	results := collectResults(ddg, 5)
	assert.Empty(t, results)
}

func TestExtractTitleFromURL(t *testing.T) {
	assert.Equal(t, "en.wikipedia.org", extractTitle("some text", "https://en.wikipedia.org/wiki/Go"))
	assert.Equal(t, "fallback text", extractTitle("fallback text", ""))
	assert.Equal(t, "invalid url text", extractTitle("invalid url text", "://bad"))
}
