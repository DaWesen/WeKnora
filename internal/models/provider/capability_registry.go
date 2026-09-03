package provider

import (
	"fmt"
	"sort"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
)

// CapabilityFactory creates a model client for one capability of one provider.
// Built-in providers register concrete factories; external providers register
// a factory that delegates inference to the plugin gRPC protocol. The config
// argument is the domain-specific config (*chat.ChatConfig, embedding.Config,
// *rerank.RerankerConfig); the caller casts the returned value to the matching
// interface (chat.Chat, embedding.Embedder, rerank.Reranker).
type CapabilityFactory func(config any) (any, error)

// capabilityKey uniquely identifies a (provider, model-type) pair.
type capabilityKey struct {
	provider   ProviderName
	capability types.ModelType
}

var (
	capabilityMu       sync.RWMutex
	capabilityRegistry = make(map[capabilityKey]CapabilityFactory)
)

// RegisterCapabilityFactory registers a factory for one provider capability.
// It is called by built-in provider init() functions and by the plugin loader
// for external providers. Duplicate registrations are rejected.
func RegisterCapabilityFactory(provider ProviderName, capability types.ModelType, factory CapabilityFactory) error {
	if factory == nil {
		return fmt.Errorf("capability factory is nil")
	}
	key := capabilityKey{provider: provider, capability: capability}
	capabilityMu.Lock()
	defer capabilityMu.Unlock()
	if _, exists := capabilityRegistry[key]; exists {
		return fmt.Errorf("capability factory for %s/%s already registered", provider, capability)
	}
	capabilityRegistry[key] = factory
	return nil
}

// GetCapabilityFactory looks up a factory by provider and model type.
func GetCapabilityFactory(provider ProviderName, capability types.ModelType) (CapabilityFactory, bool) {
	capabilityMu.RLock()
	defer capabilityMu.RUnlock()
	factory, ok := capabilityRegistry[capabilityKey{provider: provider, capability: capability}]
	return factory, ok
}

// CapabilityFactories returns all registered (provider, capability) pairs
// sorted by provider then capability. This is used for discovery and
// debugging.
func CapabilityFactories() []struct {
	Provider   ProviderName
	Capability types.ModelType
} {
	capabilityMu.RLock()
	result := make([]struct {
		Provider   ProviderName
		Capability types.ModelType
	}, 0, len(capabilityRegistry))
	for key := range capabilityRegistry {
		result = append(result, struct {
			Provider   ProviderName
			Capability types.ModelType
		}{Provider: key.provider, Capability: key.capability})
	}
	capabilityMu.RUnlock()
	sort.Slice(result, func(i, j int) bool {
		if result[i].Provider != result[j].Provider {
			return result[i].Provider < result[j].Provider
		}
		return result[i].Capability < result[j].Capability
	})
	return result
}
