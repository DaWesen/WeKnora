package model_provider

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/models/provider"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeChat is a minimal chat.Chat stand-in for hook verification.
type fakeChat struct{ name, id string }

func (f *fakeChat) Chat(context.Context, []chat.Message, *chat.ChatOptions) (*types.ChatResponse, error) {
	return nil, nil
}
func (f *fakeChat) ChatStream(context.Context, []chat.Message, *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	return nil, nil
}
func (f *fakeChat) GetModelName() string { return f.name }
func (f *fakeChat) GetModelID() string   { return f.id }

// TestChatHookConsultsCapabilityRegistry verifies that NewRemoteChat delegates
// to a registered capability factory instead of falling through to the
// built-in OpenAI-compatible adapter.
func TestChatHookConsultsCapabilityRegistry(t *testing.T) {
	provName := provider.ProviderName("test-chat-hook-prov")
	factory := func(config any) (any, error) {
		cc := config.(*chat.ChatConfig)
		return &fakeChat{name: cc.ModelName, id: cc.ModelID}, nil
	}
	require.NoError(t, provider.RegisterCapabilityFactory(provName, types.ModelTypeKnowledgeQA, factory))

	result, err := chat.NewRemoteChat(&chat.ChatConfig{
		Provider: string(provName), ModelName: "hook-test", ModelID: "h1",
		Source: types.ModelSourceRemote,
	})
	require.NoError(t, err)
	fc, ok := result.(*fakeChat)
	require.True(t, ok, "expected *fakeChat from hook, got %T", result)
	assert.Equal(t, "hook-test", fc.GetModelName())
	assert.Equal(t, "h1", fc.GetModelID())
}

// fakeEmbedder is a minimal embedding.Embedder stand-in.
type fakeEmbedder struct{ name, id string; dims int }

func (f *fakeEmbedder) Embed(context.Context, string) ([]float32, error)              { return nil, nil }
func (f *fakeEmbedder) BatchEmbed(context.Context, []string) ([][]float32, error)      { return nil, nil }
func (f *fakeEmbedder) GetModelName() string                                            { return f.name }
func (f *fakeEmbedder) GetDimensions() int                                              { return f.dims }
func (f *fakeEmbedder) GetModelID() string                                              { return f.id }
func (f *fakeEmbedder) BatchEmbedWithPool(context.Context, embedding.Embedder, []string) ([][]float32, error) {
	return nil, nil
}

func TestEmbeddingHookConsultsCapabilityRegistry(t *testing.T) {
	provName := provider.ProviderName("test-emb-hook-prov")
	factory := func(config any) (any, error) {
		ec := config.(embedding.Config)
		return &fakeEmbedder{name: ec.ModelName, id: ec.ModelID, dims: ec.Dimensions}, nil
	}
	require.NoError(t, provider.RegisterCapabilityFactory(provName, types.ModelTypeEmbedding, factory))

	result, err := embedding.NewEmbedder(embedding.Config{
		Provider: string(provName), ModelName: "emb-hook", ModelID: "e1", Dimensions: 512,
		Source: types.ModelSourceRemote,
	}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "emb-hook", result.GetModelName())
	assert.Equal(t, "e1", result.GetModelID())
}

// fakeReranker is a minimal rerank.Reranker stand-in.
type fakeReranker struct{ name, id string }

func (f *fakeReranker) Rerank(context.Context, string, []string) ([]rerank.RankResult, error) {
	return nil, nil
}
func (f *fakeReranker) GetModelName() string { return f.name }
func (f *fakeReranker) GetModelID() string   { return f.id }

func TestRerankHookConsultsCapabilityRegistry(t *testing.T) {
	provName := provider.ProviderName("test-rr-hook-prov")
	factory := func(config any) (any, error) {
		rc := config.(*rerank.RerankerConfig)
		return &fakeReranker{name: rc.ModelName, id: rc.ModelID}, nil
	}
	require.NoError(t, provider.RegisterCapabilityFactory(provName, types.ModelTypeRerank, factory))

	result, err := rerank.NewReranker(&rerank.RerankerConfig{
		Provider: string(provName), ModelName: "rr-hook", ModelID: "r1",
		Source: types.ModelSourceRemote,
	})
	require.NoError(t, err)
	assert.Equal(t, "rr-hook", result.GetModelName())
	assert.Equal(t, "r1", result.GetModelID())
}
