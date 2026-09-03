package plugin

import (
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
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

// validCapabilities returns the capability whitelist for one extension type.
// An empty slice means the type has no predefined capabilities (any non-empty
// string is accepted). Capabilities that are invalid for a type are rejected
// at validation time so a misconfigured manifest never reaches a loader.
func validCapabilities(t ExtensionType) []string {
	switch t {
	case ExtensionTypeDataSource:
		return []string{"sync", "stream", "batch"}
	case ExtensionTypeDocumentParser:
		return []string{"url", "stream", "ocr", "table", "audio"}
	case ExtensionTypeWebSearch:
		return []string{"proxy", "stream", "date_filter"}
	case ExtensionTypeModelProvider:
		return []string{"chat", "embedding", "rerank", "vlm", "asr"}
	case ExtensionTypeRetriever:
		return []string{"vector", "keywords", "hybrid", "index", "embedding"}
	default:
		return nil
	}
}

// HostVersion is the current WeKnora version used to evaluate manifest ranges.
// It is set at link time via -ldflags or defaults to a development sentinel.
var HostVersion = "0.1.0"

// validateVersionRange checks that the manifest's weknoraVersion range is
// syntactically valid and that the running host satisfies it. Supported
// forms: ">=X.Y.Z", ">=X.Y.Z <X.Y.Z", or an exact "X.Y.Z".
func validateVersionRange(rangeSpec string) error {
	rangeSpec = strings.TrimSpace(rangeSpec)
	if rangeSpec == "" {
		return fmt.Errorf("plugin WeKnora version range is required")
	}
	host := "v" + strings.TrimPrefix(strings.TrimSpace(HostVersion), "v")
	if !semver.IsValid(host) {
		return fmt.Errorf("host version %q is not a valid semver", HostVersion)
	}
	for _, part := range strings.Fields(rangeSpec) {
		switch {
		case strings.HasPrefix(part, ">="):
			min := "v" + strings.TrimPrefix(part[2:], "v")
			if !semver.IsValid(min) {
				return fmt.Errorf("invalid version constraint %q", part)
			}
			if semver.Compare(host, min) < 0 {
				return fmt.Errorf("host version %s does not satisfy %s", HostVersion, part)
			}
		case strings.HasPrefix(part, "<"):
			max := "v" + strings.TrimPrefix(part[1:], "v")
			if !semver.IsValid(max) {
				return fmt.Errorf("invalid version constraint %q", part)
			}
			if semver.Compare(host, max) >= 0 {
				return fmt.Errorf("host version %s does not satisfy %s", HostVersion, part)
			}
		case strings.HasPrefix(part, ">"):
			min := "v" + strings.TrimPrefix(part[1:], "v")
			if !semver.IsValid(min) {
				return fmt.Errorf("invalid version constraint %q", part)
			}
			if semver.Compare(host, min) <= 0 {
				return fmt.Errorf("host version %s does not satisfy %s", HostVersion, part)
			}
		case semver.IsValid("v" + strings.TrimPrefix(part, "v")):
			exact := "v" + strings.TrimPrefix(part, "v")
			if semver.Compare(host, exact) != 0 {
				return fmt.Errorf("host version %s does not satisfy exact %s", HostVersion, part)
			}
		default:
			return fmt.Errorf("unsupported version constraint %q", part)
		}
	}
	return nil
}

// validateCapabilities trims, deduplicates, and validates capabilities against
// the extension-type whitelist. It mutates m.Spec.Capabilities in place so the
// trimmed values are visible to downstream code.
func (m *Manifest) validateCapabilities() error {
	allowed := validCapabilities(m.Spec.ExtensionType)
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, cap := range allowed {
		allowedSet[cap] = struct{}{}
	}
	seen := make(map[string]struct{}, len(m.Spec.Capabilities))
	trimmed := make([]string, 0, len(m.Spec.Capabilities))
	for _, capability := range m.Spec.Capabilities {
		capability = strings.TrimSpace(capability)
		if capability == "" {
			return fmt.Errorf("plugin capability must not be empty")
		}
		if _, exists := seen[capability]; exists {
			return fmt.Errorf("plugin capability %q is duplicated", capability)
		}
		if len(allowedSet) > 0 {
			if _, ok := allowedSet[capability]; !ok {
				return fmt.Errorf("plugin capability %q is not valid for extension type %q", capability, m.Spec.ExtensionType)
			}
		}
		seen[capability] = struct{}{}
		trimmed = append(trimmed, capability)
	}
	m.Spec.Capabilities = trimmed
	return nil
}

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
	Capabilities   []string       `yaml:"capabilities,omitempty"`
	Entrypoint     Entrypoint     `yaml:"entrypoint"`
	ConfigSchema   map[string]any `yaml:"configSchema,omitempty"`
	Permissions    Permissions    `yaml:"permissions"`
	HealthCheck    *HealthCheck   `yaml:"healthCheck,omitempty"`
	RestartPolicy  *RestartPolicy `yaml:"restartPolicy,omitempty"`
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
	IntervalSeconds  int `yaml:"intervalSeconds"`
	TimeoutSeconds   int `yaml:"timeoutSeconds"`
	FailureThreshold int `yaml:"failureThreshold,omitempty"`
}

// RestartPolicy controls automatic recovery after an unexpected plugin failure.
// A plugin is restarted at most MaxAttempts times inside WindowSeconds.
type RestartPolicy struct {
	Enabled       bool `yaml:"enabled"`
	MaxAttempts   int  `yaml:"maxAttempts,omitempty"`
	WindowSeconds int  `yaml:"windowSeconds,omitempty"`
	BackoffMillis int  `yaml:"backoffMillis,omitempty"`
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

func (m *Manifest) Validate() error {
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
	if err := m.validateCapabilities(); err != nil {
		return err
	}
	if err := validateVersionRange(m.Spec.WeKnoraVersion); err != nil {
		return err
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
	if check := m.Spec.HealthCheck; check != nil {
		if check.IntervalSeconds <= 0 || check.IntervalSeconds > 3600 {
			return fmt.Errorf("health check intervalSeconds must be between 1 and 3600")
		}
		if check.TimeoutSeconds <= 0 || check.TimeoutSeconds > 60 {
			return fmt.Errorf("health check timeoutSeconds must be between 1 and 60")
		}
		if check.TimeoutSeconds > check.IntervalSeconds {
			return fmt.Errorf("health check timeoutSeconds must not exceed intervalSeconds")
		}
		if check.FailureThreshold < 0 || check.FailureThreshold > 10 {
			return fmt.Errorf("health check failureThreshold must be between 0 and 10")
		}
	}
	if policy := m.Spec.RestartPolicy; policy != nil && policy.Enabled {
		if policy.MaxAttempts <= 0 || policy.MaxAttempts > 10 {
			return fmt.Errorf("restart policy maxAttempts must be between 1 and 10")
		}
		if policy.WindowSeconds <= 0 || policy.WindowSeconds > 3600 {
			return fmt.Errorf("restart policy windowSeconds must be between 1 and 3600")
		}
		if policy.BackoffMillis < 0 || policy.BackoffMillis > 60000 {
			return fmt.Errorf("restart policy backoffMillis must be between 0 and 60000")
		}
	}
	return nil
}
