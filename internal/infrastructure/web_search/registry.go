package web_search

import (
	"fmt"
	"sort"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ProviderFactory creates a new web search provider instance from parameters.
type ProviderFactory func(params types.WebSearchProviderParameters) (interfaces.WebSearchProvider, error)

// Registry manages web search provider type registrations.
// It maps provider type IDs (e.g., "bing", "google") to their factory functions.
// Instances are created on-demand with tenant-specific parameters.
type Registry struct {
	factories map[string]ProviderFactory
	infos     map[string]types.WebSearchProviderTypeInfo
	mu        sync.RWMutex
}

// NewRegistry creates a new web search provider registry.
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]ProviderFactory),
		infos:     make(map[string]types.WebSearchProviderTypeInfo),
	}
}

// Register registers a provider type factory by ID.
func (r *Registry) Register(id string, factory ProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[id] = factory
}

// RegisterWithInfo registers a provider factory together with the metadata used
// by API clients to render its configuration form.
func (r *Registry) RegisterWithInfo(id string, info types.WebSearchProviderTypeInfo, factory ProviderFactory) error {
	if id == "" {
		return fmt.Errorf("web search provider type is empty")
	}
	if factory == nil {
		return fmt.Errorf("web search provider factory is nil")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[id]; exists {
		return fmt.Errorf("web search provider type %q already registered", id)
	}
	info.ID = id
	r.factories[id] = factory
	r.infos[id] = cloneProviderTypeInfo(info)
	return nil
}

func (r *Registry) ProviderTypes() []types.WebSearchProviderTypeInfo {
	r.mu.RLock()
	result := make([]types.WebSearchProviderTypeInfo, 0, len(r.infos))
	for _, info := range r.infos {
		result = append(result, cloneProviderTypeInfo(info))
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// AllProviderTypes returns built-in and successfully loaded external providers.
func (r *Registry) AllProviderTypes() []types.WebSearchProviderTypeInfo {
	result := types.GetWebSearchProviderTypes()
	result = append(result, r.ProviderTypes()...)
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func cloneProviderTypeInfo(info types.WebSearchProviderTypeInfo) types.WebSearchProviderTypeInfo {
	info.ConfigFields = append([]types.WebSearchProviderConfigField(nil), info.ConfigFields...)
	return info
}

// CreateProvider creates a provider instance by type with the given parameters.
func (r *Registry) CreateProvider(providerType string, params types.WebSearchProviderParameters) (interfaces.WebSearchProvider, error) {
	r.mu.RLock()
	factory, ok := r.factories[providerType]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("web search provider type %s not registered", providerType)
	}
	return factory(params)
}
