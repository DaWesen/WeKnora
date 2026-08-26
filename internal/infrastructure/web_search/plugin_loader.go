package web_search

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

// PluginLoader installs an external search plugin as a tenant-configurable provider.
type PluginLoader struct{ registry *Registry }

func NewPluginLoader(registry *Registry) *PluginLoader { return &PluginLoader{registry: registry} }
func (*PluginLoader) Type() plugin.ExtensionType       { return plugin.ExtensionTypeWebSearch }

func (l *PluginLoader) Load(ctx context.Context, manager *plugin.Manager, discovered plugin.Plugin) error {
	if l.registry == nil {
		return fmt.Errorf("web search registry is nil")
	}
	if discovered.Manifest.Spec.ExtensionType != l.Type() {
		return fmt.Errorf("expected %q plugin, got %q", l.Type(), discovered.Manifest.Spec.ExtensionType)
	}
	pluginID := discovered.Manifest.Metadata.ID
	if err := manager.StartOrRestart(ctx, pluginID, nil); err != nil {
		return fmt.Errorf("start web search plugin %q: %w", pluginID, err)
	}
	client, err := manager.Connect(ctx, pluginID)
	if err != nil {
		return fmt.Errorf("connect web search plugin %q: %w", pluginID, err)
	}
	defer client.Close()

	description, err := pluginpb.NewWebSearchPluginClient(client.Conn()).Describe(ctx, &pluginpb.WebSearchDescribeRequest{})
	if err != nil {
		return fmt.Errorf("describe web search plugin %q: %w", pluginID, err)
	}
	providerType := strings.TrimSpace(description.GetProviderType())
	if providerType == "" {
		return fmt.Errorf("web search plugin %q returned an empty provider type", pluginID)
	}
	if err := plugin.ValidateDescribeCapabilities(discovered.Manifest.Spec.Capabilities, description.GetCapabilities()); err != nil {
		return fmt.Errorf("web search plugin %q: %w", pluginID, err)
	}
	return l.registry.RegisterWithInfo(providerType, types.WebSearchProviderTypeInfo{
		Name:           description.GetDisplayName(),
		Description:    description.GetDescription(),
		RequiresAPIKey: description.GetRequiresApiKey(),
		SupportsProxy:  description.GetSupportsProxy(),
		ConfigFields:   webSearchConfigFields(description.GetConfigFields()),
	}, func(params types.WebSearchProviderParameters) (interfaces.WebSearchProvider, error) {
		return &pluginProvider{manager: manager, pluginID: pluginID, providerType: providerType, params: params}, nil
	})
}

type pluginProvider struct {
	manager      *plugin.Manager
	pluginID     string
	providerType string
	params       types.WebSearchProviderParameters
}

func (p *pluginProvider) Name() string { return p.providerType }

func (p *pluginProvider) Search(ctx context.Context, query string, maxResults int, includeDate bool) ([]*types.WebSearchResult, error) {
	config := webSearchPluginConfig(p.params)
	if err := p.manager.StartOrRestart(ctx, p.pluginID, config); err != nil {
		return nil, err
	}
	client, err := p.manager.Connect(ctx, p.pluginID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	response, err := pluginpb.NewWebSearchPluginClient(client.Conn()).Search(ctx, &pluginpb.WebSearchRequest{
		Config: config, Query: query, MaxResults: int32(maxResults), IncludeDate: includeDate,
	})
	if err != nil {
		return nil, fmt.Errorf("search with plugin %s: %w", p.pluginID, err)
	}
	results := make([]*types.WebSearchResult, 0, len(response.Results))
	for _, result := range response.Results {
		item := &types.WebSearchResult{
			Title: result.Title, URL: result.Url, Snippet: result.Snippet,
			Content: result.Content, Source: result.Source,
		}
		if result.PublishedAt != "" {
			publishedAt, err := time.Parse(time.RFC3339, result.PublishedAt)
			if err != nil {
				return nil, fmt.Errorf("plugin %s returned invalid published_at %q: %w", p.pluginID, result.PublishedAt, err)
			}
			item.PublishedAt = &publishedAt
		}
		results = append(results, item)
	}
	return results, nil
}

func webSearchPluginConfig(params types.WebSearchProviderParameters) map[string]string {
	config := make(map[string]string, len(params.ExtraConfig)+4)
	if params.APIKey != "" {
		config["api_key"] = params.APIKey
	}
	if params.EngineID != "" {
		config["engine_id"] = params.EngineID
	}
	if params.BaseURL != "" {
		config["base_url"] = params.BaseURL
	}
	if params.ProxyURL != "" {
		config["proxy_url"] = params.ProxyURL
	}
	for key, value := range params.ExtraConfig {
		config[key] = value
	}
	return config
}

func webSearchConfigFields(fields []*pluginpb.ExtensionConfigField) []types.WebSearchProviderConfigField {
	result := make([]types.WebSearchProviderConfigField, 0, len(fields))
	for _, field := range fields {
		if field == nil || strings.TrimSpace(field.GetKey()) == "" {
			continue
		}
		options := make([]types.WebSearchProviderConfigFieldOption, 0, len(field.GetOptions()))
		for _, option := range field.GetOptions() {
			options = append(options, types.WebSearchProviderConfigFieldOption{Label: option, Value: option})
		}
		result = append(result, types.WebSearchProviderConfigField{
			Key: field.GetKey(), Label: field.GetLabel(), Type: field.GetType(),
			Required: field.GetRequired(), Default: field.GetDefaultValue(), Options: options,
		})
	}
	return result
}
