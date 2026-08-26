package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ExtensionDescriptor is the host-visible description of one discovered
// extension. It is derived from the manifest and never contains runtime
// endpoints, credentials, or other sensitive configuration.
type ExtensionDescriptor struct {
	ID           string         `json:"id"`
	Type         ExtensionType  `json:"type"`
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	ConfigSchema map[string]any `json:"config_schema,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
}

func DescriptorFromPlugin(discovered Plugin) ExtensionDescriptor {
	return ExtensionDescriptor{
		ID:           discovered.Manifest.Metadata.ID,
		Type:         discovered.Manifest.Spec.ExtensionType,
		Name:         discovered.Manifest.Metadata.Name,
		Description:  discovered.Manifest.Metadata.Description,
		ConfigSchema: cloneConfigSchema(discovered.Manifest.Spec.ConfigSchema),
		Capabilities: append([]string(nil), discovered.Manifest.Spec.Capabilities...),
	}
}

func cloneConfigSchema(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(schema))
	for key, value := range schema {
		cloned[key] = cloneConfigValue(value)
	}
	return cloned
}

func cloneConfigValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneConfigSchema(value)
	case map[string]string:
		cloned := make(map[string]string, len(value))
		for key, item := range value {
			cloned[key] = item
		}
		return cloned
	case []any:
		cloned := make([]any, len(value))
		for i, item := range value {
			cloned[i] = cloneConfigValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), value...)
	default:
		return value
	}
}

// ExtensionLoader registers one external plugin with the host capability
// registry for its extension type. Runtime lifecycle remains owned by Manager.
type ExtensionLoader interface {
	Type() ExtensionType
	Load(context.Context, *Manager, Plugin) error
}

// ExtensionLoaderRegistry dispatches discovered plugins to their business
// adapters without coupling the runtime manager to any extension protocol.
type ExtensionLoaderRegistry struct {
	loaders     map[ExtensionType]ExtensionLoader
	descriptors map[string]ExtensionDescriptor
	mu          sync.RWMutex
}

func NewExtensionLoaderRegistry() *ExtensionLoaderRegistry {
	return &ExtensionLoaderRegistry{
		loaders:     make(map[ExtensionType]ExtensionLoader),
		descriptors: make(map[string]ExtensionDescriptor),
	}
}

func (r *ExtensionLoaderRegistry) Register(loader ExtensionLoader) error {
	if loader == nil {
		return fmt.Errorf("extension loader is nil")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	extensionType := loader.Type()
	if extensionType == "" {
		return fmt.Errorf("extension loader type is empty")
	}
	if _, exists := r.loaders[extensionType]; exists {
		return fmt.Errorf("extension loader already registered for %q", extensionType)
	}
	r.loaders[extensionType] = loader
	return nil
}

// Load dispatches one discovered plugin to the loader selected by its manifest.
func (r *ExtensionLoaderRegistry) Load(ctx context.Context, manager *Manager, discovered Plugin) error {
	r.mu.RLock()
	loader, exists := r.loaders[discovered.Manifest.Spec.ExtensionType]
	r.mu.RUnlock()
	if !exists {
		return fmt.Errorf("no extension loader registered for %q", discovered.Manifest.Spec.ExtensionType)
	}
	if err := loader.Load(ctx, manager, discovered); err != nil {
		return fmt.Errorf("load plugin %s: %w", discovered.Manifest.Metadata.ID, err)
	}

	r.mu.Lock()
	r.descriptors[discovered.Manifest.Metadata.ID] = DescriptorFromPlugin(discovered)
	r.mu.Unlock()
	return nil
}

// Descriptor returns a copy of the descriptor registered for a loaded plugin.
func (r *ExtensionLoaderRegistry) Descriptor(id string) (ExtensionDescriptor, bool) {
	r.mu.RLock()
	descriptor, ok := r.descriptors[id]
	r.mu.RUnlock()
	if !ok {
		return ExtensionDescriptor{}, false
	}
	descriptor.ConfigSchema = cloneConfigSchema(descriptor.ConfigSchema)
	descriptor.Capabilities = append([]string(nil), descriptor.Capabilities...)
	return descriptor, true
}

// Descriptors returns host-safe descriptors for all successfully loaded plugins.
func (r *ExtensionLoaderRegistry) Descriptors() []ExtensionDescriptor {
	r.mu.RLock()
	result := make([]ExtensionDescriptor, 0, len(r.descriptors))
	for _, descriptor := range r.descriptors {
		descriptor.ConfigSchema = cloneConfigSchema(descriptor.ConfigSchema)
		descriptor.Capabilities = append([]string(nil), descriptor.Capabilities...)
		result = append(result, descriptor)
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// LoadAll loads every discovered plugin. Each loader only installs a host-side
// adapter; it must not start runtimes or duplicate health and recovery logic.
func (r *ExtensionLoaderRegistry) LoadAll(ctx context.Context, manager *Manager) error {
	var errs error
	for _, discovered := range manager.List("") {
		if err := r.Load(ctx, manager, discovered); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}
