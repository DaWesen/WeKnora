package model_provider

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/models/provider"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPluginConfigMapFlattensAllFields(t *testing.T) {
	got := pluginConfigMap("key", "https://example.com", "gpt-test", "model-1",
		map[string]string{"region": "us", "version": "2"},
		map[string]string{"X-Trace-Id": "abc"})

	assert.Equal(t, "key", got["api_key"])
	assert.Equal(t, "https://example.com", got["base_url"])
	assert.Equal(t, "gpt-test", got["model_name"])
	assert.Equal(t, "model-1", got["model_id"])
	assert.Equal(t, "us", got["region"])
	assert.Equal(t, "2", got["version"])
	assert.Equal(t, "abc", got["X-Trace-Id"])
}

func TestPluginConfigMapOmitsEmptyValues(t *testing.T) {
	got := pluginConfigMap("", "", "", "", nil, nil)
	_, hasKey := got["api_key"]
	_, hasURL := got["base_url"]
	_, hasName := got["model_name"]
	_, hasID := got["model_id"]
	assert.False(t, hasKey)
	assert.False(t, hasURL)
	assert.False(t, hasName)
	assert.False(t, hasID)
}

func TestToPluginMessagesMapsAllFields(t *testing.T) {
	messages := []chat.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello", Images: []string{"data:image/png;base64,abc"}, ReasoningContent: "thinking"},
	}
	result := toPluginMessages(messages)
	require.Len(t, result, 2)
	assert.Equal(t, "system", result[0].GetRole())
	assert.Equal(t, "You are helpful.", result[0].GetContent())
	assert.Equal(t, "user", result[1].GetRole())
	assert.Equal(t, "Hello", result[1].GetContent())
	assert.Equal(t, "thinking", result[1].GetReasoningContent())
	assert.Equal(t, []string{"data:image/png;base64,abc"}, result[1].GetImages())
}

func TestNewCapabilityFactoryByModelType(t *testing.T) {
	manager := &plugin.Manager{} // not used by factory creation, only captured for later
	tests := []struct {
		name      string
		modelType types.ModelType
		wantNil   bool
	}{
		{"chat", types.ModelTypeKnowledgeQA, false},
		{"vlm", types.ModelTypeVLLM, false},
		{"embedding", types.ModelTypeEmbedding, false},
		{"rerank", types.ModelTypeRerank, false},
		{"asr", types.ModelTypeASR, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			factory, err := newCapabilityFactory(tc.modelType, manager, "test-plugin", "test-provider")
			require.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, factory)
				return
			}
			require.NotNil(t, factory)
		})
	}
}

func TestNewCapabilityFactoryRejectsUnknownType(t *testing.T) {
	_, err := newCapabilityFactory(types.ModelType("Unknown"), nil, "p", "prov")
	assert.Error(t, err)
}

func TestChatFactoryReturnsPluginChat(t *testing.T) {
	manager := &plugin.Manager{}
	factory, err := newCapabilityFactory(types.ModelTypeKnowledgeQA, manager, "chat-plugin", "chat-prov")
	require.NoError(t, err)
	require.NotNil(t, factory)

	result, err := factory(&chat.ChatConfig{
		ModelName: "test-chat-model", ModelID: "m1", APIKey: "k", BaseURL: "u",
		Provider: "chat-prov", Source: types.ModelSourceRemote,
	})
	require.NoError(t, err)

	pc, ok := result.(*pluginChat)
	require.True(t, ok, "expected *pluginChat, got %T", result)
	assert.Equal(t, "test-chat-model", pc.GetModelName())
	assert.Equal(t, "m1", pc.GetModelID())
	assert.Equal(t, "k", pc.config["api_key"])
	assert.Equal(t, "test-chat-model", pc.config["model_name"])
}

func TestEmbeddingFactoryReturnsPluginEmbedder(t *testing.T) {
	manager := &plugin.Manager{}
	factory, err := newCapabilityFactory(types.ModelTypeEmbedding, manager, "emb-plugin", "emb-prov")
	require.NoError(t, err)
	require.NotNil(t, factory)

	result, err := factory(embedding.Config{
		ModelName: "test-emb-model", ModelID: "e1", APIKey: "k", Dimensions: 768,
		Provider: "emb-prov", Source: types.ModelSourceRemote,
	})
	require.NoError(t, err)

	pe, ok := result.(*pluginEmbedder)
	require.True(t, ok, "expected *pluginEmbedder, got %T", result)
	assert.Equal(t, "test-emb-model", pe.GetModelName())
	assert.Equal(t, "e1", pe.GetModelID())
	assert.Equal(t, 768, pe.GetDimensions())
}

func TestRerankFactoryReturnsPluginReranker(t *testing.T) {
	manager := &plugin.Manager{}
	factory, err := newCapabilityFactory(types.ModelTypeRerank, manager, "rr-plugin", "rr-prov")
	require.NoError(t, err)
	require.NotNil(t, factory)

	result, err := factory(&rerank.RerankerConfig{
		ModelName: "test-rr-model", ModelID: "r1", APIKey: "k",
		Provider: "rr-prov", Source: types.ModelSourceRemote,
	})
	require.NoError(t, err)

	pr, ok := result.(*pluginReranker)
	require.True(t, ok, "expected *pluginReranker, got %T", result)
	assert.Equal(t, "test-rr-model", pr.GetModelName())
	assert.Equal(t, "r1", pr.GetModelID())
}

func TestPluginProviderValidateConfig(t *testing.T) {
	p := &pluginProvider{info: provider.ProviderInfo{RequiresAuth: true}}
	assert.Error(t, p.ValidateConfig(nil))
	assert.Error(t, p.ValidateConfig(&provider.Config{ModelName: ""}))
	assert.Error(t, p.ValidateConfig(&provider.Config{ModelName: "m", APIKey: ""}))
	assert.NoError(t, p.ValidateConfig(&provider.Config{ModelName: "m", APIKey: "k"}))
}
