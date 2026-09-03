package model_provider

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/Tencent/WeKnora/internal/types"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

// pluginConfigMap flattens the common model-config fields into the string map
// the plugin RPC expects. ExtraConfig and CustomHeaders are passed through
// as-is so plugins can read provider-specific knobs they declared in Describe.
func pluginConfigMap(apiKey, baseURL, modelName, modelID string, extraConfig, customHeaders map[string]string) map[string]string {
	config := make(map[string]string, len(extraConfig)+len(customHeaders)+4)
	if apiKey != "" {
		config["api_key"] = apiKey
	}
	if baseURL != "" {
		config["base_url"] = baseURL
	}
	if modelName != "" {
		config["model_name"] = modelName
	}
	if modelID != "" {
		config["model_id"] = modelID
	}
	for k, v := range extraConfig {
		config[k] = v
	}
	for k, v := range customHeaders {
		config[k] = v
	}
	return config
}

func toPluginMessages(messages []chat.Message) []*pluginpb.ChatMessage {
	result := make([]*pluginpb.ChatMessage, 0, len(messages))
	for _, msg := range messages {
		result = append(result, &pluginpb.ChatMessage{
			Role:             msg.Role,
			Content:          msg.Content,
			ReasoningContent: msg.ReasoningContent,
			Images:           msg.Images,
		})
	}
	return result
}

// --- Chat ---

type pluginChat struct {
	manager   *plugin.Manager
	pluginID  string
	config    map[string]string
	modelName string
	modelID   string
}

func newPluginChat(manager *plugin.Manager, pluginID string, cc *chat.ChatConfig) *pluginChat {
	return &pluginChat{
		manager:   manager,
		pluginID:  pluginID,
		config:    pluginConfigMap(cc.APIKey, cc.BaseURL, cc.ModelName, cc.ModelID, cc.ExtraConfig, cc.CustomHeaders),
		modelName: cc.ModelName,
		modelID:   cc.ModelID,
	}
}

func (c *pluginChat) GetModelName() string { return c.modelName }
func (c *pluginChat) GetModelID() string   { return c.modelID }

func (c *pluginChat) Chat(ctx context.Context, messages []chat.Message, opts *chat.ChatOptions) (*types.ChatResponse, error) {
	stream, cleanup, err := c.openStream(ctx, messages, opts)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var content strings.Builder
	var reasoning strings.Builder
	var finishReason string
	var usage types.TokenUsage
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return nil, fmt.Errorf("chat with plugin %s: %w", c.pluginID, recvErr)
		}
		if msg := chunk.GetError(); msg != "" {
			return nil, fmt.Errorf("plugin %s chat error: %s", c.pluginID, msg)
		}
		content.WriteString(chunk.GetContent())
		reasoning.WriteString(chunk.GetReasoningContent())
		if chunk.GetFinishReason() != "" {
			finishReason = chunk.GetFinishReason()
		}
		if chunk.GetTotalTokens() > 0 {
			usage = types.TokenUsage{
				PromptTokens:     int(chunk.GetPromptTokens()),
				CompletionTokens: int(chunk.GetCompletionTokens()),
				TotalTokens:      int(chunk.GetTotalTokens()),
			}
		}
	}
	return &types.ChatResponse{
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		FinishReason:     finishReason,
		Usage:            usage,
	}, nil
}

func (c *pluginChat) ChatStream(ctx context.Context, messages []chat.Message, opts *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	stream, cleanup, err := c.openStream(ctx, messages, opts)
	if err != nil {
		return nil, err
	}
	ch := make(chan types.StreamResponse, 16)
	go func() {
		defer cleanup()
		defer close(ch)
		for {
			chunk, recvErr := stream.Recv()
			if recvErr == io.EOF {
				return
			}
			if recvErr != nil {
				ch <- types.StreamResponse{
					ResponseType: types.ResponseTypeError,
					Content:      recvErr.Error(),
					Done:         true,
				}
				return
			}
			if msg := chunk.GetError(); msg != "" {
				ch <- types.StreamResponse{
					ResponseType: types.ResponseTypeError,
					Content:      msg,
					Done:         true,
				}
				return
			}
			done := chunk.GetFinishReason() != ""
			ch <- types.StreamResponse{
				Content:      chunk.GetContent(),
				Done:         done,
				FinishReason: chunk.GetFinishReason(),
			}
		}
	}()
	return ch, nil
}

func (c *pluginChat) openStream(ctx context.Context, messages []chat.Message, opts *chat.ChatOptions) (pluginpb.ModelProviderPlugin_ChatClient, func(), error) {
	if opts == nil {
		opts = &chat.ChatOptions{}
	}
	if err := c.manager.StartOrRestart(ctx, c.pluginID, nil); err != nil {
		return nil, nil, err
	}
	client, err := c.manager.Connect(ctx, c.pluginID)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { client.Close() }
	stream, err := pluginpb.NewModelProviderPluginClient(client.Conn()).Chat(ctx, &pluginpb.ChatRequest{
		Config:      c.config,
		Model:       c.modelName,
		Messages:    toPluginMessages(messages),
		Temperature: opts.Temperature,
		TopP:        opts.TopP,
		MaxTokens:   int32(opts.MaxTokens),
		Seed:        int32(opts.Seed),
	})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("chat with plugin %s: %w", c.pluginID, err)
	}
	return stream, cleanup, nil
}

// --- Embedder ---

type pluginEmbedder struct {
	manager    *plugin.Manager
	pluginID   string
	config     map[string]string
	modelName  string
	modelID    string
	dimensions int
}

func newPluginEmbedder(manager *plugin.Manager, pluginID string, ec embedding.Config) *pluginEmbedder {
	return &pluginEmbedder{
		manager:    manager,
		pluginID:   pluginID,
		config:     pluginConfigMap(ec.APIKey, ec.BaseURL, ec.ModelName, ec.ModelID, ec.ExtraConfig, ec.CustomHeaders),
		modelName:  ec.ModelName,
		modelID:    ec.ModelID,
		dimensions: ec.Dimensions,
	}
}

func (e *pluginEmbedder) GetModelName() string { return e.modelName }
func (e *pluginEmbedder) GetModelID() string   { return e.modelID }
func (e *pluginEmbedder) GetDimensions() int   { return e.dimensions }

func (e *pluginEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.BatchEmbed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("plugin %s embed returned %d vectors for 1 input", e.pluginID, len(vectors))
	}
	return vectors[0], nil
}

func (e *pluginEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if err := e.manager.StartOrRestart(ctx, e.pluginID, nil); err != nil {
		return nil, err
	}
	client, err := e.manager.Connect(ctx, e.pluginID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	response, err := pluginpb.NewModelProviderPluginClient(client.Conn()).Embed(ctx, &pluginpb.EmbedRequest{
		Config: e.config, Model: e.modelName, Inputs: texts,
	})
	if err != nil {
		return nil, fmt.Errorf("embed with plugin %s: %w", e.pluginID, err)
	}
	// Cache dimensions if the plugin reported them and the host config didn't.
	if d := int(response.GetDimensions()); d > 0 && e.dimensions == 0 {
		e.dimensions = d
	}
	embeddings := make([][]float32, 0, len(response.GetEmbeddings()))
	for _, emb := range response.GetEmbeddings() {
		embeddings = append(embeddings, emb.GetValues())
	}
	if len(embeddings) != len(texts) {
		return nil, fmt.Errorf("plugin %s embed returned %d vectors for %d inputs", e.pluginID, len(embeddings), len(texts))
	}
	return embeddings, nil
}

// BatchEmbedWithPool satisfies EmbedderPooler. The plugin handles its own
// batching internally, so pooling is a no-op here.
func (e *pluginEmbedder) BatchEmbedWithPool(ctx context.Context, _ embedding.Embedder, texts []string) ([][]float32, error) {
	return e.BatchEmbed(ctx, texts)
}

// --- Reranker ---

type pluginReranker struct {
	manager   *plugin.Manager
	pluginID  string
	config    map[string]string
	modelName string
	modelID   string
}

func newPluginReranker(manager *plugin.Manager, pluginID string, rc *rerank.RerankerConfig) *pluginReranker {
	return &pluginReranker{
		manager:   manager,
		pluginID:  pluginID,
		config:    pluginConfigMap(rc.APIKey, rc.BaseURL, rc.ModelName, rc.ModelID, rc.ExtraConfig, rc.CustomHeaders),
		modelName: rc.ModelName,
		modelID:   rc.ModelID,
	}
}

func (r *pluginReranker) GetModelName() string { return r.modelName }
func (r *pluginReranker) GetModelID() string   { return r.modelID }

func (r *pluginReranker) Rerank(ctx context.Context, query string, documents []string) ([]rerank.RankResult, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	if err := r.manager.StartOrRestart(ctx, r.pluginID, nil); err != nil {
		return nil, err
	}
	client, err := r.manager.Connect(ctx, r.pluginID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	response, err := pluginpb.NewModelProviderPluginClient(client.Conn()).Rerank(ctx, &pluginpb.RerankRequest{
		Config: r.config, Model: r.modelName, Query: query, Documents: documents,
	})
	if err != nil {
		return nil, fmt.Errorf("rerank with plugin %s: %w", r.pluginID, err)
	}
	results := make([]rerank.RankResult, 0, len(response.GetResults()))
	for _, item := range response.GetResults() {
		results = append(results, rerank.RankResult{
			Index:          int(item.GetIndex()),
			RelevanceScore: item.GetScore(),
			Document:       rerank.DocumentInfo{Text: documents[item.GetIndex()]},
		})
	}
	return results, nil
}
