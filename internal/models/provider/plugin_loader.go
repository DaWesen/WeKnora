package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/Tencent/WeKnora/internal/types"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

// PluginLoader registers metadata for an external model provider. The current
// plugin protocol supports provider/model discovery only; inference continues
// to use the host's existing model adapters.
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

	description, err := pluginpb.NewModelProviderPluginClient(client.Conn()).Describe(ctx, &pluginpb.ModelProviderDescribeRequest{})
	if err != nil {
		return fmt.Errorf("describe model provider plugin %q: %w", pluginID, err)
	}
	name := ProviderName(strings.TrimSpace(description.GetProviderType()))
	if name == "" {
		return fmt.Errorf("model provider plugin %q returned an empty provider type", pluginID)
	}
	modelTypes := pluginModelTypes(description.GetModelTypes())
	if len(modelTypes) == 0 {
		return fmt.Errorf("model provider plugin %q returned no supported model types", pluginID)
	}
	return RegisterExternal(&pluginProvider{info: ProviderInfo{
		Name:         name,
		DisplayName:  description.GetDisplayName(),
		Description:  description.GetDescription(),
		ModelTypes:   modelTypes,
		DefaultURLs:  pluginDefaultURLs(description.GetDefaultUrls()),
		RequiresAuth: description.GetRequiresAuth(),
		ExtraFields:  pluginExtraFields(description.GetConfigFields()),
	}})
}

type pluginProvider struct{ info ProviderInfo }

func (p *pluginProvider) Info() ProviderInfo { return p.info }
func (p *pluginProvider) ValidateConfig(config *Config) error {
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

func pluginExtraFields(fields []*pluginpb.ExtensionConfigField) []ExtraFieldConfig {
	result := make([]ExtraFieldConfig, 0, len(fields))
	for _, field := range fields {
		if field == nil || strings.TrimSpace(field.GetKey()) == "" {
			continue
		}
		result = append(result, ExtraFieldConfig{
			Key: field.GetKey(), Label: field.GetLabel(), Type: field.GetType(),
			Required: field.GetRequired(), Default: field.GetDefaultValue(),
		})
	}
	return result
}
