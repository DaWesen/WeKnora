package main

import (
	"context"
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
// returns a gRPC client plus the underlying server instance (for direct state
// inspection). It mirrors how the host connects to a plugin.
func startTestServer(t *testing.T) (pluginpb.RetrieverPluginClient, *server, func()) {
	t.Helper()
	impl := &server{records: make(map[string][]*pluginpb.IndexRecord)}
	impl.Lifecycle = pluginsdk.Lifecycle{
		Metadata: pluginsdk.Metadata{
			ID:             "com.weknora.retriever-memory",
			Version:        "0.1.0",
			ExtensionTypes: []string{"retriever"},
		},
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := pluginsdk.New(impl, pluginsdk.RetrieverService(impl))
	go grpcServer.Serve(listener)
	conn, err := grpc.NewClient(
		listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	cleanup := func() { conn.Close(); grpcServer.GracefulStop() }
	return pluginpb.NewRetrieverPluginClient(conn), impl, cleanup
}

// record is a helper to build a valid index record with a vector.
func record(kbID, chunkID, content string, vector []float32) *pluginpb.IndexRecord {
	return &pluginpb.IndexRecord{
		Id: kbID + ":" + chunkID, Content: content,
		ChunkId: chunkID, KnowledgeBaseId: kbID, KnowledgeId: "k-" + chunkID,
		IsEnabled: true, Embedding: vector,
	}
}

func TestDescribeDeclaresIndexVectorEmbedding(t *testing.T) {
	client, _, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Describe(context.Background(), &pluginpb.RetrieverDescribeRequest{})
	require.NoError(t, err)
	assert.Equal(t, "memory-vector", response.GetEngineType())
	assert.Equal(t, []string{"index", "vector", "embedding"}, response.GetCapabilities())
	assert.Equal(t, []string{"vector"}, response.GetRetrieverTypes())
}

func TestIndexThenRetrieveRanksByCosineSimilarity(t *testing.T) {
	client, _, cleanup := startTestServer(t)
	defer cleanup()
	ctx := context.Background()

	// Orthogonal unit basis vectors; the query exactly matches doc-1.
	_, err := client.BatchSaveIndex(ctx, &pluginpb.BatchSaveIndexRequest{Indices: []*pluginpb.IndexRecord{
		record("kb1", "chunk-1", "closest document", []float32{1, 0, 0}),
		record("kb1", "chunk-2", "orthogonal document", []float32{0, 1, 0}),
		record("kb1", "chunk-3", "opposite document", []float32{-1, 0, 0}),
	}})
	require.NoError(t, err)

	response, err := client.Retrieve(ctx, &pluginpb.RetrieveRequest{
		Embedding:        []float32{1, 0, 0},
		KnowledgeBaseIds: []string{"kb1"},
		TopK:             3,
	})
	require.NoError(t, err)
	require.Len(t, response.GetResults(), 3)

	assert.Equal(t, "kb1:chunk-1", response.GetResults()[0].GetId())
	assert.InDelta(t, 1.0, response.GetResults()[0].GetScore(), 1e-6)
	assert.InDelta(t, 0.0, response.GetResults()[1].GetScore(), 1e-6)
	assert.InDelta(t, -1.0, response.GetResults()[2].GetScore(), 1e-6)
}

func TestSaveIndexUpsertsByID(t *testing.T) {
	client, impl, cleanup := startTestServer(t)
	defer cleanup()
	ctx := context.Background()

	_, err := client.SaveIndex(ctx, &pluginpb.SaveIndexRequest{
		Index: record("kb1", "chunk-1", "version one", []float32{1, 0}),
	})
	require.NoError(t, err)
	_, err = client.SaveIndex(ctx, &pluginpb.SaveIndexRequest{
		Index: record("kb1", "chunk-1", "version two", []float32{0, 1}),
	})
	require.NoError(t, err)

	require.Len(t, impl.records["kb1"], 1, "same record id must upsert, not append")
	assert.Equal(t, "version two", impl.records["kb1"][0].GetContent())
}

func TestSaveIndexRejectsMissingEmbedding(t *testing.T) {
	client, _, cleanup := startTestServer(t)
	defer cleanup()

	noEmbedding := record("kb1", "chunk-1", "text only", nil)
	_, err := client.SaveIndex(context.Background(), &pluginpb.SaveIndexRequest{Index: noEmbedding})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no embedding")
}

func TestRetrieveWithoutQueryEmbeddingFails(t *testing.T) {
	client, _, cleanup := startTestServer(t)
	defer cleanup()

	_, err := client.Retrieve(context.Background(), &pluginpb.RetrieveRequest{
		KnowledgeBaseIds: []string{"kb1"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query embedding")
}

func TestRetrieveFiltersAndExcludes(t *testing.T) {
	client, _, cleanup := startTestServer(t)
	defer cleanup()
	ctx := context.Background()

	_, err := client.BatchSaveIndex(ctx, &pluginpb.BatchSaveIndexRequest{Indices: []*pluginpb.IndexRecord{
		record("kb1", "a", "match a", []float32{1, 0}),
		record("kb1", "b", "match b", []float32{1, 0}),
		record("kb1", "c", "disabled", []float32{1, 0}),
		record("kb2", "d", "other kb", []float32{1, 0}),
	}})
	require.NoError(t, err)

	// Disable chunk c.
	_, err = client.UpdateChunkEnabledStatus(ctx, &pluginpb.UpdateChunkEnabledStatusRequest{
		ChunkStatusMap: map[string]bool{"c": false},
	})
	require.NoError(t, err)

	// kb1 only, exclude chunk a.
	response, err := client.Retrieve(ctx, &pluginpb.RetrieveRequest{
		Embedding:          []float32{1, 0},
		KnowledgeBaseIds:   []string{"kb1"},
		ExcludeChunkIds:    []string{"a"},
		TopK:               10,
	})
	require.NoError(t, err)
	require.Len(t, response.GetResults(), 1)
	assert.Equal(t, "kb1:b", response.GetResults()[0].GetId())
}

func TestRetrieveAppliesThresholdAndTopK(t *testing.T) {
	client, _, cleanup := startTestServer(t)
	defer cleanup()
	ctx := context.Background()

	_, err := client.BatchSaveIndex(ctx, &pluginpb.BatchSaveIndexRequest{Indices: []*pluginpb.IndexRecord{
		record("kb1", "a", "aligned", []float32{1, 0}),
		record("kb1", "b", "angled", []float32{0.70710678, 0.70710678}),
		record("kb1", "c", "opposite", []float32{-1, 0}),
	}})
	require.NoError(t, err)

	response, err := client.Retrieve(ctx, &pluginpb.RetrieveRequest{
		Embedding:        []float32{1, 0},
		KnowledgeBaseIds: []string{"kb1"},
		TopK:             2,
	})
	require.NoError(t, err)
	require.Len(t, response.GetResults(), 2)
	assert.Equal(t, "kb1:a", response.GetResults()[0].GetId())

	response, err = client.Retrieve(ctx, &pluginpb.RetrieveRequest{
		Embedding:        []float32{1, 0},
		KnowledgeBaseIds: []string{"kb1"},
		TopK:             10,
		Threshold:        0.9,
	})
	require.NoError(t, err)
	require.Len(t, response.GetResults(), 1, "threshold must drop sub-0.9 scores")
}

func TestDeleteByChunkIDs(t *testing.T) {
	client, impl, cleanup := startTestServer(t)
	defer cleanup()
	ctx := context.Background()

	_, err := client.BatchSaveIndex(ctx, &pluginpb.BatchSaveIndexRequest{Indices: []*pluginpb.IndexRecord{
		record("kb1", "a", "keep", []float32{1, 0}),
		record("kb1", "b", "drop", []float32{1, 0}),
	}})
	require.NoError(t, err)

	_, err = client.DeleteByChunkIDs(ctx, &pluginpb.DeleteByChunkIDsRequest{
		ChunkIds: []string{"b"}, Dimension: 2,
	})
	require.NoError(t, err)
	require.Len(t, impl.records["kb1"], 1)
	assert.Equal(t, "a", impl.records["kb1"][0].GetChunkId())
}

func TestDeleteBySourceIDsAndKnowledgeIDs(t *testing.T) {
	client, impl, cleanup := startTestServer(t)
	defer cleanup()
	ctx := context.Background()

	b := record("kb1", "b", "drop by source", []float32{1, 0})
	b.SourceId = "src-1"
	c := record("kb1", "c", "drop by knowledge", []float32{1, 0})
	c.KnowledgeId = "k-drop"
	_, err := client.BatchSaveIndex(ctx, &pluginpb.BatchSaveIndexRequest{Indices: []*pluginpb.IndexRecord{
		record("kb1", "a", "keep", []float32{1, 0}), b, c,
	}})
	require.NoError(t, err)

	_, err = client.DeleteBySourceIDs(ctx, &pluginpb.DeleteBySourceIDsRequest{
		SourceIds: []string{"src-1"}, Dimension: 2,
	})
	require.NoError(t, err)
	require.Len(t, impl.records["kb1"], 2)

	_, err = client.DeleteByKnowledgeIDs(ctx, &pluginpb.DeleteByKnowledgeIDsRequest{
		KnowledgeIds: []string{"k-drop"}, Dimension: 2,
	})
	require.NoError(t, err)
	require.Len(t, impl.records["kb1"], 1)
	assert.Equal(t, "a", impl.records["kb1"][0].GetChunkId())
}

func TestCopyIndicesClonesToTargetKnowledgeBase(t *testing.T) {
	client, impl, cleanup := startTestServer(t)
	defer cleanup()
	ctx := context.Background()

	_, err := client.BatchSaveIndex(ctx, &pluginpb.BatchSaveIndexRequest{Indices: []*pluginpb.IndexRecord{
		record("kb1", "a", "doc a", []float32{1, 0}),
		record("kb1", "b", "doc b", []float32{0, 1}),
	}})
	require.NoError(t, err)

	_, err = client.CopyIndices(ctx, &pluginpb.CopyIndicesRequest{
		SourceKnowledgeBaseId: "kb1",
		TargetKnowledgeBaseId: "kb2",
		SourceToTargetChunkIdMap: map[string]string{"a": "a2", "b": "b2"},
	})
	require.NoError(t, err)

	require.Len(t, impl.records["kb2"], 2)
	byChunk := map[string]string{}
	for _, r := range impl.records["kb2"] {
		byChunk[r.GetChunkId()] = r.GetContent()
	}
	assert.Equal(t, "doc a", byChunk["a2"])
	assert.Equal(t, "doc b", byChunk["b2"])
	// Source untouched.
	require.Len(t, impl.records["kb1"], 2)
}

func TestUpdateChunkTagID(t *testing.T) {
	client, impl, cleanup := startTestServer(t)
	defer cleanup()
	ctx := context.Background()

	_, err := client.BatchSaveIndex(ctx, &pluginpb.BatchSaveIndexRequest{Indices: []*pluginpb.IndexRecord{
		record("kb1", "a", "doc", []float32{1, 0}),
	}})
	require.NoError(t, err)

	_, err = client.UpdateChunkTagID(ctx, &pluginpb.UpdateChunkTagIDRequest{
		ChunkTagMap: map[string]string{"a": "tag-9"},
	})
	require.NoError(t, err)
	assert.Equal(t, "tag-9", impl.records["kb1"][0].GetTagId())
}

func TestCosineSimilarityEdgeCases(t *testing.T) {
	score, ok := cosineSimilarity([]float32{1, 0}, []float32{0, 1})
	require.True(t, ok)
	assert.InDelta(t, 0.0, score, 1e-9)

	score, ok = cosineSimilarity([]float32{2, 0}, []float32{4, 0})
	require.True(t, ok)
	assert.InDelta(t, 1.0, score, 1e-9)

	_, ok = cosineSimilarity([]float32{0, 0}, []float32{1, 0})
	assert.False(t, ok, "zero vector has undefined direction")

	_, ok = cosineSimilarity([]float32{1, 0}, []float32{1, 0, 0})
	assert.False(t, ok, "dimension mismatch is undefined")
}
