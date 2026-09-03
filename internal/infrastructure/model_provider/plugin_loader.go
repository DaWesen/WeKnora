// Package model_provider hosts the external model provider plugin loader and
// its inference adapters. It lives in infrastructure (not internal/models/provider)
// because the adapters must import chat/embedding/rerank, which themselves import
// the provider package — putting the loader in provider would create a cycle.
package model_provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/models/provider"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/Tencent/WeKnora/internal/types"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

// PluginLoader discovers external model provider plugins, registers their
// metadata into the provider registry, and — when the plugin declares inference
// capabilities (chat/embedding/rerank) — registers a CapabilityFactory that
// delegates inference to the plugin's gRPC RPCs.
type PluginLoader struct{}

func NewPluginLoader() *PluginLoader             { return &PluginLoader{} }
func (*PluginLoader) Type() plugin.ExtensionType { return plugin.ExtensionTypeModelProvider }

func (l *PluginLoader) Load(ctx context.Context, manager *plugin.Manager, discovered plugin.Plugin) error {
	if discovered.Manifest.Spec.ExtensionType != l.Type() {
		return fmt.Errorf("expected %q plugin, got %q", l.Type(), discovered.Manifest.Spec.ExtensionType)
	}
	pluginID := discovered.Manifest.Metadata.ID
	if err := manager.StartOrRestart(ctx, pluginID, nil); err != nil {
		return fmt.Errorf("start model provider plugin %q: %w", pluginID, err)
	}
	client, err := manager.Connect(ctx, pluginID)
	if err != nil {
		return fmt.Errorf("connect model provider plugin %q: %w", pluginID, err)
	}
	defer client.Close()

	pb := pluginpb.NewModelProviderPluginClient(client.Conn())
	description, err := pb.Describe(ctx, &pluginpb.ModelProviderDescribeRequest{})
	if err != nil {
		return fmt.Errorf("describe model provider plugin %q: %w", pluginID, err)
	}
	name := provider.ProviderName(strings.TrimSpace(description.GetProviderType()))
	if name == "" {
		return fmt.Errorf("model provider plugin %q returned an empty provider type", pluginID)
	}
	modelTypes := pluginModelTypes(description.GetModelTypes())
	if len(modelTypes) == 0 {
		return fmt.Errorf("model provider plugin %q returned no supported model types", pluginID)
	}
	if err := plugin.ValidateDescribeCapabilities(discovered.Manifest.Spec.Capabilities, description.GetCapabilities()); err != nil {
		return fmt.Errorf("model provider plugin %q: %w", pluginID, err)
	}
	if err := provider.RegisterExternal(&pluginProvider{info: provider.ProviderInfo{
		Name:         name,
		DisplayName:  description.GetDisplayName(),
		Description:  description.GetDescription(),
		ModelTypes:   modelTypes,
		DefaultURLs:  pluginDefaultURLs(description.GetDefaultUrls()),
		RequiresAuth: description.GetRequiresAuth(),
		ExtraFields:  pluginExtraFields(description.GetConfigFields()),
	}}); err != nil {
		return fmt.Errorf("register model provider plugin %q: %w", pluginID, err)
	}

	// Register inference capability factories. Each factory captures the plugin
	// manager and plugin ID so the returned adapter can open a connection per
	// call. Only chat/embedding/rerank have inference RPCs today; vlm reuses
	// the chat RPC (images are carried in ChatMessage.images), asr has none.
	for _, modelType := range modelTypes {
		factory, err := newCapabilityFactory(modelType, manager, pluginID, name)
		if err != nil {
			return fmt.Errorf("model provider plugin %q: %w", pluginID, err)
		}
		if factory == nil {
			continue // model type has no inference RPC (e.g. ASR)
		}
		if err := provider.RegisterCapabilityFactory(name, modelType, factory); err != nil {
			return fmt.Errorf("register capability %s for plugin %q: %w", modelType, pluginID, err)
		}
	}
	return nil
}

// newCapabilityFactory returns a CapabilityFactory for the given model type, or
// nil if the model type has no inference RPC (ASR). VLM maps to the chat factory.
func newCapabilityFactory(modelType types.ModelType, manager *plugin.Manager, pluginID string, name provider.ProviderName) (provider.CapabilityFactory, error) {
	switch modelType {
	case types.ModelTypeKnowledgeQA, types.ModelTypeVLLM:
		return func(config any) (any, error) {
			cc, ok := config.(*chat.ChatConfig)
			if !ok {
				return nil, fmt.Errorf("chat factory for %s received %T", name, config)
			}
			return newPluginChat(manager, pluginID, cc), nil
		}, nil
	case types.ModelTypeEmbedding:
		return func(config any) (any, error) {
			ec, ok := config.(embedding.Config)
			if !ok {
				return nil, fmt.Errorf("embedding factory for %s received %T", name, config)
			}
			return newPluginEmbedder(manager, pluginID, ec), nil
		}, nil
	case types.ModelTypeRerank:
		return func(config any) (any, error) {
			rc, ok := config.(*rerank.RerankerConfig)
			if !ok {
				return nil, fmt.Errorf("rerank factory for %s received %T", name, config)
			}
			return newPluginReranker(manager, pluginID, rc), nil
		}, nil
	case types.ModelTypeASR:
		// No inference RPC for ASR in the current protocol.
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported model type %q for plugin inference", modelType)
	}
}

// --- metadata helpers (moved from internal/models/provider/plugin_loader.go) ---

type pluginProvider struct{ info provider.ProviderInfo }

func (p *pluginProvider) Info() provider.ProviderInfo { return p.info }
func (p *pluginProvider) ValidateConfig(config *provider.Config) error {
	if config == nil {
		return fmt.Errorf("model provider config is nil")
	}
	if strings.TrimSpace(config.ModelName) == "" {
		return fmt.Errorf("model name is required")
	}
	if p.info.RequiresAuth && strings.TrimSpace(config.APIKey) == "" {
		return fmt.Errorf("API key is required")
	}
	return nil
}

func pluginModelTypes(modelTypes []string) []types.ModelType {
	result := make([]types.ModelType, 0, len(modelTypes))
	seen := make(map[types.ModelType]struct{}, len(modelTypes))
	for _, modelType := range modelTypes {
		var mapped types.ModelType
		switch strings.ToLower(strings.TrimSpace(modelType)) {
		case "chat":
			mapped = types.ModelTypeKnowledgeQA
		case "embedding":
			mapped = types.ModelTypeEmbedding
		case "rerank":
			mapped = types.ModelTypeRerank
		case "vlm":
			mapped = types.ModelTypeVLLM
		case "asr":
			mapped = types.ModelTypeASR
		default:
			continue
		}
		if _, exists := seen[mapped]; !exists {
			seen[mapped] = struct{}{}
			result = append(result, mapped)
		}
	}
	return result
}

func pluginDefaultURLs(urls map[string]string) map[types.ModelType]string {
	result := make(map[types.ModelType]string, len(urls))
	for modelType, url := range urls {
		mapped := pluginModelTypes([]string{modelType})
		if len(mapped) == 1 && strings.TrimSpace(url) != "" {
			result[mapped[0]] = url
		}
	}
	return result
}

func pluginExtraFields(fields []*pluginpb.ExtensionConfigField) []provider.ExtraFieldConfig {
	result := make([]provider.ExtraFieldConfig, 0, len(fields))
	for _, field := range fields {
		if field == nil || strings.TrimSpace(field.GetKey()) == "" {
			continue
		}
		result = append(result, provider.ExtraFieldConfig{
			Key: field.GetKey(), Label: field.GetLabel(), Type: field.GetType(),
			Required: field.GetRequired(), Default: field.GetDefaultValue(),
		})
	}
	return result
}
