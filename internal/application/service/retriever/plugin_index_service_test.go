package retriever

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	"github.com/stretchr/testify/require"
)

// fakeEmbedder returns one deterministic vector per distinct text so tests can
// assert the exact content-to-embedding mapping handed to the plugin.
type fakeEmbedder struct {
	mu      sync.Mutex
	byText  map[string][]float32
	fail    error
	embeds  int
	batches int
}

func newFakeEmbedder() *fakeEmbedder {
	return &fakeEmbedder{byText: make(map[string][]float32)}
}

func (f *fakeEmbedder) vectorFor(text string) []float32 {
	if vector, ok := f.byText[text]; ok {
		return vector
	}
	return []float32{float32(len(text)), 0.5, -0.25}
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.embeds++
	if f.fail != nil {
		return nil, f.fail
	}
	return f.vectorFor(text), nil
}

func (f *fakeEmbedder) BatchEmbed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches++
	if f.fail != nil {
		return nil, f.fail
	}
	vectors := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vectors = append(vectors, f.vectorFor(text))
	}
	return vectors, nil
}

func (f *fakeEmbedder) GetModelName() string { return "fake-embedding" }
func (f *fakeEmbedder) GetDimensions() int   { return 3 }
func (f *fakeEmbedder) GetModelID() string   { return "fake-embedding-id" }
func (f *fakeEmbedder) BatchEmbedWithPool(_ context.Context, _ embedding.Embedder, texts []string) ([][]float32, error) {
	return f.BatchEmbed(context.Background(), texts)
}

func indexRecord(id, content string) *pluginpb.IndexRecord {
	return &pluginpb.IndexRecord{Id: id, Content: content}
}

func TestEmbedIndexRecordsFillsVectorsForVectorRetrieval(t *testing.T) {
	embedder := newFakeEmbedder()
	records := []*pluginpb.IndexRecord{
		indexRecord("chunk-1", "first passage"),
		indexRecord("chunk-2", "second passage"),
	}

	require.NoError(t, embedIndexRecords(context.Background(), embedder, records, []types.RetrieverType{types.VectorRetrieverType}))

	require.Equal(t, embedder.vectorFor("first passage"), records[0].Embedding)
	require.Equal(t, embedder.vectorFor("second passage"), records[1].Embedding)
	require.Equal(t, 1, embedder.batches, "batch embedding must use one BatchEmbedWithPool round trip")
}

func TestEmbedIndexRecordsEmbedsSingleRecordWithoutBatching(t *testing.T) {
	embedder := newFakeEmbedder()
	records := []*pluginpb.IndexRecord{indexRecord("chunk-1", "only passage")}

	require.NoError(t, embedIndexRecords(context.Background(), embedder, records, []types.RetrieverType{types.VectorRetrieverType}))

	require.Equal(t, 1, embedder.embeds)
	require.Equal(t, 0, embedder.batches)
	require.Equal(t, embedder.vectorFor("only passage"), records[0].Embedding)
}

func TestEmbedIndexRecordsRejectsNilEmbedderForVectorRetrieval(t *testing.T) {
	records := []*pluginpb.IndexRecord{indexRecord("chunk-1", "passage")}

	err := embedIndexRecords(context.Background(), nil, records, []types.RetrieverType{types.VectorRetrieverType})

	require.ErrorContains(t, err, "requires an embedder")
	require.Empty(t, records[0].Embedding, "no record may be written without its embedding")
}

func TestEmbedIndexRecordsSkipsEmbeddingForKeywordsRetrieval(t *testing.T) {
	embedder := newFakeEmbedder()
	records := []*pluginpb.IndexRecord{indexRecord("chunk-1", "passage")}

	require.NoError(t, embedIndexRecords(context.Background(), nil, records, []types.RetrieverType{types.KeywordsRetrieverType}))

	require.Empty(t, records[0].Embedding)
	require.Zero(t, embedder.embeds)
	require.Zero(t, embedder.batches)
}

func TestEmbedIndexRecordsPropagatesEmbedderFailure(t *testing.T) {
	embedder := newFakeEmbedder()
	embedder.fail = errors.New("embedding endpoint down")
	records := []*pluginpb.IndexRecord{indexRecord("chunk-1", "passage")}

	err := embedIndexRecords(context.Background(), embedder, records, []types.RetrieverType{types.VectorRetrieverType})

	require.ErrorContains(t, err, "embedding endpoint down")
	require.Empty(t, records[0].Embedding)
}

func TestEmbedIndexRecordsRejectsVectorCountMismatch(t *testing.T) {
	// A broken embedder returning fewer vectors than requested must be caught,
	// not partially written into the records.
	embedder := &shortPoolerEmbedder{inner: newFakeEmbedder()}
	records := []*pluginpb.IndexRecord{indexRecord("chunk-1", "a"), indexRecord("chunk-2", "b")}

	err := embedIndexRecords(context.Background(), embedder, records, []types.RetrieverType{types.VectorRetrieverType})

	require.ErrorContains(t, err, "returned 1 vectors for 2 index records")
	require.Empty(t, records[0].Embedding)
	require.Empty(t, records[1].Embedding)
}

// shortPoolerEmbedder returns one fewer vector than requested, emulating a
// misbehaving remote embedding endpoint.
type shortPoolerEmbedder struct{ inner embedding.Embedder }

func (s *shortPoolerEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return s.inner.Embed(ctx, text)
}
func (s *shortPoolerEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	return s.inner.BatchEmbed(ctx, texts)
}
func (s *shortPoolerEmbedder) GetModelName() string { return s.inner.GetModelName() }
func (s *shortPoolerEmbedder) GetDimensions() int   { return s.inner.GetDimensions() }
func (s *shortPoolerEmbedder) GetModelID() string   { return s.inner.GetModelID() }
func (s *shortPoolerEmbedder) BatchEmbedWithPool(_ context.Context, _ embedding.Embedder, texts []string) ([][]float32, error) {
	vectors, err := s.inner.BatchEmbed(context.Background(), texts)
	if err != nil {
		return nil, err
	}
	return vectors[:len(vectors)-1], nil
}

func TestRequireEmbeddingCapability(t *testing.T) {
	require.NoError(t, requireEmbeddingCapability(
		[]types.RetrieverType{types.VectorRetrieverType}, []string{"index", "embedding"}))
	require.NoError(t, requireEmbeddingCapability(
		[]types.RetrieverType{types.KeywordsRetrieverType}, []string{"index"}))

	err := requireEmbeddingCapability(
		[]types.RetrieverType{types.VectorRetrieverType}, []string{"index"})
	require.ErrorContains(t, err, `"embedding" capability`)
}
