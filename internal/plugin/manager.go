package plugin

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const ManifestFileName = "plugin.yaml"

type Status string

const (
	StatusDiscovered Status = "discovered"
	StatusDisabled   Status = "disabled"
	StatusRunning    Status = "running"
	StatusFailed     Status = "failed"
)

// Plugin is a discovered plugin together with its runtime state.
type Plugin struct {
	Manifest     Manifest
	Directory    string
	Status       Status
	LastError    string
	DiscoveredAt time.Time
}

// CheckHealth invokes the lifecycle health check of a plugin already started by
// its runtime. Process and container startup is deliberately kept separate from
// the protocol client.
func (m *Manager) CheckHealth(ctx context.Context, id, address string, timeout time.Duration) error {
	err := CheckHealth(ctx, address, timeout)
	if err != nil {
		_ = m.SetStatus(id, StatusFailed, err)
		return err
	}
	return m.SetStatus(id, StatusRunning, nil)
}

// Manager discovers manifests and maintains the plugin registry. Runtime launch
// is intentionally deferred to the next phase, after the gRPC plugin protocol is added.
type Manager struct {
	root    string
	runtime *Runtime
	mu      sync.RWMutex
	byID    map[string]*Plugin
}

func NewManager(root string) *Manager {
	return &Manager{root: root, runtime: NewRuntime(), byID: make(map[string]*Plugin)}
}

// Start launches a discovered plugin and verifies its gRPC lifecycle endpoint.
func (m *Manager) Start(ctx context.Context, id string) error {
	plugin, ok := m.Get(id)
	if !ok {
		return fs.ErrNotExist
	}
	if err := m.runtime.Start(ctx, *plugin); err != nil {
		_ = m.SetStatus(id, StatusFailed, err)
		return err
	}
	if err := m.CheckHealth(ctx, id, plugin.Manifest.Spec.Entrypoint.GRPCAddress, healthCheckTimeout(*plugin)); err != nil {
		_ = m.runtime.Stop(context.Background(), id)
		return err
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	if err := m.runtime.Stop(ctx, id); err != nil {
		return err
	}
	return m.SetStatus(id, StatusDisabled, nil)
}

func (m *Manager) StopAll(ctx context.Context) error {
	return m.runtime.StopAll(ctx)
}

// Connect dials a discovered plugin's declared gRPC endpoint. Callers should
// start the plugin first unless its endpoint is managed externally.
func (m *Manager) Connect(ctx context.Context, id string) (*Client, error) {
	plugin, ok := m.Get(id)
	if !ok {
		return nil, fs.ErrNotExist
	}
	return Dial(ctx, plugin.Manifest.Spec.Entrypoint.GRPCAddress)
}

// Discover scans direct child directories of root for plugin.yaml files.
func (m *Manager) Discover() error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read plugin directory: %w", err)
	}

	discovered := make(map[string]*Plugin)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(m.root, entry.Name())
		manifestPath := filepath.Join(directory, ManifestFileName)
		data, readErr := os.ReadFile(manifestPath)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return fmt.Errorf("read plugin manifest %s: %w", manifestPath, readErr)
		}
		manifest, parseErr := ParseManifest(data)
		if parseErr != nil {
			return fmt.Errorf("invalid plugin manifest %s: %w", manifestPath, parseErr)
		}
		if _, exists := discovered[manifest.Metadata.ID]; exists {
			return fmt.Errorf("duplicate plugin id %q", manifest.Metadata.ID)
		}
		discovered[manifest.Metadata.ID] = &Plugin{
			Manifest:     *manifest,
			Directory:    directory,
			Status:       StatusDiscovered,
			DiscoveredAt: time.Now().UTC(),
		}
	}

	m.mu.Lock()
	m.byID = discovered
	m.mu.Unlock()
	return nil
}

func (m *Manager) Get(id string) (*Plugin, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	plugin, ok := m.byID[id]
	if !ok {
		return nil, false
	}
	copy := *plugin
	return &copy, true
}

func (m *Manager) List(extensionType ExtensionType) []Plugin {
	m.mu.RLock()
	defer m.mu.RUnlock()

	plugins := make([]Plugin, 0, len(m.byID))
	for _, plugin := range m.byID {
		if extensionType != "" && plugin.Manifest.Spec.ExtensionType != extensionType {
			continue
		}
		plugins = append(plugins, *plugin)
	}
	sort.Slice(plugins, func(i, j int) bool {
		return plugins[i].Manifest.Metadata.ID < plugins[j].Manifest.Metadata.ID
	})
	return plugins
}

// SetStatus records the result of lifecycle operations that will be implemented
// by the process/container runtime in the next phase.
func (m *Manager) SetStatus(id string, status Status, lastErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	plugin, ok := m.byID[id]
	if !ok {
		return fs.ErrNotExist
	}
	plugin.Status = status
	plugin.LastError = ""
	if lastErr != nil {
		plugin.LastError = lastErr.Error()
	}
	return nil
}
