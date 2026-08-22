package plugin

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const APIVersionV1 = "weknora.plugin/v1"

// ExtensionType identifies the WeKnora extension point implemented by a plugin.
type ExtensionType string

const (
	ExtensionTypeDataSource     ExtensionType = "datasource"
	ExtensionTypeDocumentParser ExtensionType = "document_parser"
	ExtensionTypeWebSearch      ExtensionType = "web_search"
	ExtensionTypeModelProvider  ExtensionType = "model_provider"
	ExtensionTypeRetriever      ExtensionType = "retriever"
)

// Manifest is the on-disk declaration for an external WeKnora plugin.
type Manifest struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Description string `yaml:"description,omitempty"`
}

type Spec struct {
	ExtensionType  ExtensionType  `yaml:"extensionType"`
	WeKnoraVersion string         `yaml:"weknoraVersion"`
	Entrypoint     Entrypoint     `yaml:"entrypoint"`
	ConfigSchema   map[string]any `yaml:"configSchema,omitempty"`
	Permissions    Permissions    `yaml:"permissions"`
	HealthCheck    *HealthCheck   `yaml:"healthCheck,omitempty"`
}

type Entrypoint struct {
	Type                 string   `yaml:"type"`
	Command              []string `yaml:"command,omitempty"`
	Image                string   `yaml:"image,omitempty"`
	GRPCAddress          string   `yaml:"grpcAddress"`
	ContainerGRPCAddress string   `yaml:"containerGrpcAddress,omitempty"`
}

type Permissions struct {
	Network    NetworkPermission    `yaml:"network"`
	Filesystem FilesystemPermission `yaml:"filesystem,omitempty"`
}

type NetworkPermission struct {
	Enabled bool     `yaml:"enabled"`
	Hosts   []string `yaml:"hosts,omitempty"`
}

type FilesystemPermission struct {
	ReadOnly []string `yaml:"readOnly,omitempty"`
}

type HealthCheck struct {
	IntervalSeconds int `yaml:"intervalSeconds"`
	TimeoutSeconds  int `yaml:"timeoutSeconds"`
}

// ParseManifest parses and validates a plugin manifest.
func ParseManifest(data []byte) (*Manifest, error) {
	var manifest Manifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse plugin manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// Validate rejects manifests that cannot be loaded safely by the framework.
// ValidateConfig checks the simple JSON-Schema subset used by plugin manifests
// before the plugin is started. Plugins still validate their own domain-specific
// configuration through gRPC after startup.
func (m Manifest) ValidateConfig(config map[string]string) error {
	if len(m.Spec.ConfigSchema) == 0 {
		return nil
	}
	if required, ok := m.Spec.ConfigSchema["required"].([]any); ok {
		for _, value := range required {
			key, ok := value.(string)
			if !ok || strings.TrimSpace(key) == "" {
				return fmt.Errorf("plugin config schema contains an invalid required field")
			}
			if strings.TrimSpace(config[key]) == "" {
				return fmt.Errorf("plugin configuration requires %q", key)
			}
		}
	}
	properties, ok := m.Spec.ConfigSchema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	for key, rawProperty := range properties {
		value, exists := config[key]
		if !exists || strings.TrimSpace(value) == "" {
			continue
		}
		property, ok := rawProperty.(map[string]any)
		if !ok {
			continue
		}
		if propertyType, ok := property["type"].(string); ok && propertyType != "string" {
			return fmt.Errorf("plugin config schema property %q uses unsupported type %q", key, propertyType)
		}
	}
	return nil
}

func (m Manifest) Validate() error {
	if m.APIVersion != APIVersionV1 {
		return fmt.Errorf("unsupported plugin apiVersion %q", m.APIVersion)
	}
	if m.Kind != "Plugin" {
		return fmt.Errorf("plugin kind must be Plugin")
	}
	if strings.TrimSpace(m.Metadata.ID) == "" || strings.TrimSpace(m.Metadata.Name) == "" || strings.TrimSpace(m.Metadata.Version) == "" {
		return fmt.Errorf("plugin metadata id, name, and version are required")
	}
	switch m.Spec.ExtensionType {
	case ExtensionTypeDataSource, ExtensionTypeDocumentParser, ExtensionTypeWebSearch, ExtensionTypeModelProvider, ExtensionTypeRetriever:
	default:
		return fmt.Errorf("unsupported extension type %q", m.Spec.ExtensionType)
	}
	if strings.TrimSpace(m.Spec.WeKnoraVersion) == "" {
		return fmt.Errorf("plugin WeKnora version range is required")
	}
	if m.Spec.Entrypoint.Type != "process" && m.Spec.Entrypoint.Type != "container" {
		return fmt.Errorf("plugin entrypoint type must be process or container")
	}
	if strings.TrimSpace(m.Spec.Entrypoint.GRPCAddress) == "" {
		return fmt.Errorf("plugin gRPC address is required")
	}
	if !m.Spec.Permissions.Network.Enabled && len(m.Spec.Permissions.Network.Hosts) > 0 {
		return fmt.Errorf("network hosts require network permission")
	}
	if m.Spec.Entrypoint.Type == "process" && (len(m.Spec.Entrypoint.Command) == 0 || strings.TrimSpace(m.Spec.Entrypoint.Command[0]) == "") {
		return fmt.Errorf("process plugin entrypoint command is required")
	}
	if m.Spec.Entrypoint.Type == "process" && !m.Spec.Permissions.Network.Enabled {
		return fmt.Errorf("network-disabled plugins require a container runtime for enforceable isolation")
	}
	if m.Spec.Entrypoint.Type == "container" && strings.TrimSpace(m.Spec.Entrypoint.Image) == "" {
		return fmt.Errorf("container plugin entrypoint image is required")
	}
	if m.Spec.Entrypoint.Type == "container" && strings.TrimSpace(m.Spec.Entrypoint.ContainerGRPCAddress) != "" && !strings.HasPrefix(m.Spec.Entrypoint.ContainerGRPCAddress, "unix://") {
		return fmt.Errorf("container gRPC address must use unix://")
	}
	if m.Spec.Entrypoint.Type == "container" && !m.Spec.Permissions.Network.Enabled {
		if !strings.HasPrefix(m.Spec.Entrypoint.GRPCAddress, "unix://") || !strings.HasPrefix(m.Spec.Entrypoint.ContainerGRPCAddress, "unix://") {
			return fmt.Errorf("network-disabled container plugins require unix:// gRPC addresses")
		}
	}
	return nil
}
