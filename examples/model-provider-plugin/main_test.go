package main

import (
	"context"
	"io"
	"math"
	"net"
	"testing"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// startTestServer starts the plugin's gRPC server on an ephemeral port and
// returns a gRPC client. It mirrors how the host connects to a plugin.
func startTestServer(t *testing.T) (pluginpb.ModelProviderPluginClient, func()) {
	t.Helper()
	impl := &server{
		Lifecycle: pluginsdk.Lifecycle{
			Metadata: pluginsdk.Metadata{
				ID:             "com.weknora.model-provider-deterministic",
				Version:        "0.1.0",
				ExtensionTypes: []string{"model_provider"},
			},
		},
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := pluginsdk.New(impl, pluginsdk.ModelProviderService(impl))
	go grpcServer.Serve(listener)
	conn, err := grpc.NewClient(
		listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	cleanup := func() { conn.Close(); grpcServer.GracefulStop() }
	return pluginpb.NewModelProviderPluginClient(conn), cleanup
}

func TestDescribeReturnsDeterministicMetadata(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Describe(context.Background(), &pluginpb.ModelProviderDescribeRequest{})
	require.NoError(t, err)
	assert.Equal(t, "deterministic", response.GetProviderType())
	assert.Equal(t, []string{"chat", "embedding", "rerank"}, response.GetModelTypes())
	assert.Equal(t, []string{"chat", "embedding", "rerank"}, response.GetCapabilities())
	assert.False(t, response.GetRequiresAuth())
}

func TestListModelsReturnsThreeModels(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.ListModels(context.Background(), &pluginpb.ListModelsRequest{})
	require.NoError(t, err)
	require.Len(t, response.GetModels(), 3)
	for _, model := range response.GetModels() {
		assert.NotEmpty(t, model.GetId())
		assert.Len(t, model.GetCapabilities(), 1)
	}
}

func TestChatStreamsEchoWithUsage(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	stream, err := client.Chat(context.Background(), &pluginpb.ChatRequest{
		Model: "deterministic-chat",
		Messages: []*pluginpb.ChatMessage{
			{Role: "system", Content: "You are deterministic."},
			{Role: "user", Content: "hello deterministic world"},
		},
	})
	require.NoError(t, err)

	var content string
	var lastChunk *pluginpb.ChatChunk
	chunks := 0
	for {
		chunk, recvErr := stream.Recv()
		if recvErr != nil {
			require.ErrorIs(t, recvErr, io.EOF)
			break
		}
		content += chunk.GetContent()
		lastChunk = chunk
		chunks++
	}
	require.GreaterOrEqual(t, chunks, 2, "reply should span multiple stream chunks")
	assert.Contains(t, content, "Deterministic echo: hello deterministic world")
	require.NotNil(t, lastChunk)
	assert.Equal(t, "stop", lastChunk.GetFinishReason())
	assert.Positive(t, lastChunk.GetTotalTokens())
	assert.Equal(t, lastChunk.GetPromptTokens()+lastChunk.GetCompletionTokens(), lastChunk.GetTotalTokens())
}

func TestChatHandlesNoUserMessage(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	stream, err := client.Chat(context.Background(), &pluginpb.ChatRequest{
		Messages: []*pluginpb.ChatMessage{{Role: "system", Content: "system only"}},
	})
	require.NoError(t, err)

	var content string
	for {
		chunk, recvErr := stream.Recv()
		if recvErr != nil {
			require.ErrorIs(t, recvErr, io.EOF)
			break
		}
		content += chunk.GetContent()
	}
	assert.Contains(t, content, "(no user message)")
}

func TestEmbedDeterministicUnitVectors(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	first, err := client.Embed(ctx, &pluginpb.EmbedRequest{
		Model:  "deterministic-embedding",
		Inputs: []string{"weknora plugin framework", "weknora plugin framework"},
	})
	require.NoError(t, err)
	require.Len(t, first.GetEmbeddings(), 2)
	assert.EqualValues(t, embedDims, first.GetDimensions())

	vecA := first.GetEmbeddings()[0].GetValues()
	vecB := first.GetEmbeddings()[1].GetValues()
	require.Len(t, vecA, embedDims)
	require.Len(t, vecB, embedDims)
	for i := range vecA {
		assert.InDelta(t, vecA[i], vecB[i], 1e-6, "same input must embed identically")
	}

	norm := 0.0
	for _, value := range vecA {
		require.False(t, math.IsNaN(float64(value)), "embedding values must not be NaN")
		norm += float64(value) * float64(value)
	}
	assert.InDelta(t, 1.0, math.Sqrt(norm), 1e-5, "vector must be L2 normalized")
}

func TestEmbedSeparatesDifferentInputs(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Embed(context.Background(), &pluginpb.EmbedRequest{
		Model:  "deterministic-embedding",
		Inputs: []string{"alpha beta gamma", "完全不同的内容"},
	})
	require.NoError(t, err)
	require.Len(t, response.GetEmbeddings(), 2)

	vecA := response.GetEmbeddings()[0].GetValues()
	vecB := response.GetEmbeddings()[1].GetValues()
	dot := 0.0
	for i := range vecA {
		dot += float64(vecA[i]) * float64(vecB[i])
	}
	assert.Less(t, math.Abs(dot), 1.0-1e-6, "distinct inputs must not be identical vectors")
}

func TestEmbedEmptyInputReturnsZeroVector(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Embed(context.Background(), &pluginpb.EmbedRequest{
		Inputs: []string{"!!!"},
	})
	require.NoError(t, err)
	require.Len(t, response.GetEmbeddings(), 1)
	for _, value := range response.GetEmbeddings()[0].GetValues() {
		assert.EqualValues(t, 0, value, "no tokens must yield a zero vector without NaN")
	}
}

func TestRerankSortsByOverlap(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Rerank(context.Background(), &pluginpb.RerankRequest{
		Model: "deterministic-rerank",
		Query: "golang grpc plugin",
		Documents: []string{
			"recipe for banana bread",
			"building a golang grpc plugin with the hashing trick",
			"banana bread recipe with walnuts",
		},
	})
	require.NoError(t, err)
	require.Len(t, response.GetResults(), 3)

	// Document 1 shares every query token; documents 0 and 2 share none.
	assert.EqualValues(t, 1, response.GetResults()[0].GetIndex())
	assert.InDelta(t, 1.0, response.GetResults()[0].GetScore(), 1e-9)
	assert.InDelta(t, 0.0, response.GetResults()[1].GetScore(), 1e-9)
	assert.InDelta(t, 0.0, response.GetResults()[2].GetScore(), 1e-9)

	for i := 1; i < len(response.GetResults()); i++ {
		assert.GreaterOrEqual(t,
			response.GetResults()[i-1].GetScore(),
			response.GetResults()[i].GetScore(),
			"results must be sorted by score descending")
	}
}

func TestRerankRespectsTopN(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Rerank(context.Background(), &pluginpb.RerankRequest{
		Query:     "golang",
		Documents: []string{"golang a", "golang b", "golang c", "golang d"},
		TopN:      2,
	})
	require.NoError(t, err)
	require.Len(t, response.GetResults(), 2)
	assert.InDelta(t, 1.0, response.GetResults()[0].GetScore(), 1e-9)
	assert.InDelta(t, 1.0, response.GetResults()[1].GetScore(), 1e-9)
	// Stable sort keeps original order among equal scores.
	assert.EqualValues(t, 0, response.GetResults()[0].GetIndex())
	assert.EqualValues(t, 1, response.GetResults()[1].GetIndex())
}

func TestRerankEmptyDocuments(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Rerank(context.Background(), &pluginpb.RerankRequest{
		Query:     "anything",
		Documents: nil,
	})
	require.NoError(t, err)
	assert.Empty(t, response.GetResults())
}
