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

	"github.com/Tencent/WeKnora/internal/logger"
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

type restartState struct {
	attempts []time.Time
}

const (
	defaultRestartBackoff  = 250 * time.Millisecond
	defaultShutdownTimeout = 5 * time.Second
)

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
	root            string
	runtime         *Runtime
	audit           *AuditLog
	persistentAudit auditSink
	mu              sync.RWMutex
	byID            map[string]*Plugin
	restarts        map[string]*restartState
	restarting      map[string]bool
}

func NewManager(root string) *Manager {
	return NewManagerWithAudit(root, nil)
}

// NewManagerWithAudit also writes lifecycle events to the application audit
// store. Durable audit failures are intentionally non-fatal to plugin work.
func NewManagerWithAudit(root string, persistentAudit auditSink) *Manager {
	manager := &Manager{
		root:            root,
		runtime:         NewRuntime(),
		audit:           NewAuditLog(0),
		persistentAudit: persistentAudit,
		byID:            make(map[string]*Plugin),
		restarts:        make(map[string]*restartState),
		restarting:      make(map[string]bool),
	}
	manager.runtime.SetProcessExitHandler(func(id string, cause error) {
		_ = manager.MarkRuntimeFailed(id, cause)
	})
	return manager
}

// AuditEvents returns bounded, structured lifecycle and security events.
func (m *Manager) AuditEvents(query AuditQuery) []AuditEvent {
	return m.audit.List(query)
}

func (m *Manager) recordAudit(pluginID, action, outcome, target, message string, details map[string]string) {
	event := AuditEvent{
		Timestamp: time.Now().UTC(),
		PluginID:  pluginID,
		Action:    action,
		Outcome:   outcome,
		Target:    target,
		Message:   message,
		Details:   details,
	}
	m.audit.Record(event)
	persistAuditEvent(m.persistentAudit, event)
}

// RecordNetworkDenied persists a security-policy rejection reported by a plugin.
// The target is metadata only; callers must not include credentials or payloads.
func (m *Manager) RecordNetworkDenied(pluginID, target, message string) {
	m.recordAudit(pluginID, AuditActionPluginNetworkDenied, "denied", target, message, nil)
}

// Start launches a discovered plugin without runtime configuration.
func (m *Manager) Start(ctx context.Context, id string) error {
	return m.StartWithConfig(ctx, id, nil)
}

// StartOrRestart starts a discovered plugin or, after a runtime failure,
// recovers it through the manifest restart policy. Callers that invoke a
// plugin repeatedly must use this entry point so a failed runtime cannot
// silently bypass its restart budget on the next request.
func (m *Manager) StartOrRestart(ctx context.Context, id string, config map[string]string) error {
	plugin, ok := m.Get(id)
	if !ok {
		return fs.ErrNotExist
	}
	if plugin.Status == StatusFailed {
		return m.Restart(ctx, id, config)
	}
	return m.StartWithConfig(ctx, id, config)
}

// StartWithConfig launches a discovered plugin and resolves configuration-backed
// filesystem permissions before verifying its gRPC lifecycle endpoint.
func (m *Manager) StartWithConfig(ctx context.Context, id string, config map[string]string) error {
	plugin, ok := m.Get(id)
	if !ok {
		return fs.ErrNotExist
	}
	if err := plugin.Manifest.ValidateConfig(config); err != nil {
		m.recordAudit(id, AuditActionPluginStartFailed, "denied", "", err.Error(), map[string]string{"stage": "manifest_config"})
		return err
	}
	if err := m.runtime.Start(ctx, *plugin, config); err != nil {
		_ = m.SetStatus(id, StatusFailed, err)
		m.recordAudit(id, AuditActionPluginStartFailed, "failed", "", err.Error(), map[string]string{"stage": "runtime_start"})
		logger.Errorf(ctx, "[Plugin] start failed id=%s error=%v", id, err)
		return err
	}
	if err := m.CheckHealth(ctx, id, plugin.Manifest.Spec.Entrypoint.GRPCAddress, healthCheckTimeout(*plugin)); err != nil {
		_ = m.runtime.Stop(context.Background(), id)
		m.recordAudit(id, AuditActionPluginHealthFailed, "failed", "", err.Error(), nil)
		logger.Errorf(ctx, "[Plugin] health check failed id=%s error=%v", id, err)
		return err
	}
	if err := m.verifyIdentity(ctx, *plugin); err != nil {
		_ = m.runtime.Stop(context.Background(), id)
		_ = m.SetStatus(id, StatusFailed, err)
		m.recordAudit(id, AuditActionPluginIdentityFail, "failed", "", err.Error(), nil)
		logger.Errorf(ctx, "[Plugin] identity verification failed id=%s error=%v", id, err)
		return err
	}
	m.recordAudit(id, AuditActionPluginStarted, "success", "", "plugin started", map[string]string{
		"extension_type":  string(plugin.Manifest.Spec.ExtensionType),
		"network_enabled": fmt.Sprintf("%t", plugin.Manifest.Spec.Permissions.Network.Enabled),
	})
	logger.Infof(ctx, "[Plugin] started id=%s type=%s network=%t", id, plugin.Manifest.Spec.ExtensionType, plugin.Manifest.Spec.Permissions.Network.Enabled)
	return nil
}

func (m *Manager) verifyIdentity(ctx context.Context, plugin Plugin) error {
	client, err := Dial(ctx, plugin.Manifest.Spec.Entrypoint.GRPCAddress)
	if err != nil {
		return fmt.Errorf("dial plugin for identity verification: %w", err)
	}
	defer client.Close()

	info, err := client.GetInfo(ctx)
	if err != nil {
		return fmt.Errorf("get plugin identity: %w", err)
	}
	return validatePluginInfo(plugin.Manifest, info.Id, info.Version, info.ExtensionTypes)
}

func validatePluginInfo(manifest Manifest, id, version string, extensionTypes []string) error {
	if id != manifest.Metadata.ID {
		return fmt.Errorf("plugin id mismatch: manifest=%q runtime=%q", manifest.Metadata.ID, id)
	}
	if version != manifest.Metadata.Version {
		return fmt.Errorf("plugin version mismatch: manifest=%q runtime=%q", manifest.Metadata.Version, version)
	}
	for _, extensionType := range extensionTypes {
		if extensionType == string(manifest.Spec.ExtensionType) {
			return nil
		}
	}
	return fmt.Errorf("plugin does not provide declared extension type %q", manifest.Spec.ExtensionType)
}

// Restart restarts a failed plugin while enforcing its manifest recovery budget.
// The caller supplies the same configuration used to launch the failing runtime.
func (m *Manager) Restart(ctx context.Context, id string, config map[string]string) error {
	plugin, ok := m.Get(id)
	if !ok {
		return fs.ErrNotExist
	}
	if plugin.Status != StatusFailed {
		err := fmt.Errorf("plugin %q is not in failed state", id)
		m.recordAudit(id, AuditActionPluginRestartDenied, "denied", "", err.Error(), nil)
		return err
	}
	policy := plugin.Manifest.Spec.RestartPolicy
	if policy == nil || !policy.Enabled {
		err := fmt.Errorf("automatic restart is disabled for plugin %q", id)
		m.recordAudit(id, AuditActionPluginRestartDenied, "denied", "", err.Error(), nil)
		return err
	}
	if !m.beginRestart(id) {
		err := fmt.Errorf("plugin %q restart is already in progress", id)
		m.recordAudit(id, AuditActionPluginRestartDenied, "denied", "", err.Error(), nil)
		return err
	}
	defer m.endRestart(id)

	attempt, err := m.reserveRestartAttempt(id, *policy, time.Now().UTC())
	if err != nil {
		m.recordAudit(id, AuditActionPluginRestartDenied, "denied", "", err.Error(), nil)
		return err
	}

	if err := m.stopRuntime(ctx, *plugin); err != nil {
		m.recordAudit(id, AuditActionPluginStopFailed, "failed", "", err.Error(), map[string]string{"stage": "restart"})
		return err
	}
	backoff := restartBackoff(*policy)
	if backoff > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	if err := m.StartWithConfig(ctx, id, config); err != nil {
		return err
	}
	m.recordAudit(id, AuditActionPluginRestarted, "success", "", "plugin restarted", map[string]string{"attempt": fmt.Sprintf("%d", attempt)})
	return nil
}

// MarkRuntimeFailed transitions a running plugin to failed after a transport or
// process failure and preserves the reason for restart policy decisions.
func (m *Manager) MarkRuntimeFailed(id string, cause error) error {
	if cause == nil {
		return fmt.Errorf("plugin runtime failure cause is required")
	}
	if err := m.SetStatus(id, StatusFailed, cause); err != nil {
		return err
	}
	m.recordAudit(id, AuditActionPluginRuntimeFailed, "failed", "", cause.Error(), nil)
	return nil
}

func (m *Manager) beginRestart(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.restarting[id] {
		return false
	}
	m.restarting[id] = true
	return true
}

func (m *Manager) endRestart(id string) {
	m.mu.Lock()
	delete(m.restarting, id)
	m.mu.Unlock()
}

func (m *Manager) reserveRestartAttempt(id string, policy RestartPolicy, now time.Time) (int, error) {
	windowStart := now.Add(-time.Duration(policy.WindowSeconds) * time.Second)
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.restarts[id]
	if state == nil {
		state = &restartState{}
		m.restarts[id] = state
	}
	active := state.attempts[:0]
	for _, attemptedAt := range state.attempts {
		if !attemptedAt.Before(windowStart) {
			active = append(active, attemptedAt)
		}
	}
	if len(active) >= policy.MaxAttempts {
		state.attempts = active
		return 0, fmt.Errorf("restart budget exhausted: %d attempts within %ds", policy.MaxAttempts, policy.WindowSeconds)
	}
	state.attempts = append(active, now)
	return len(state.attempts), nil
}

func restartBackoff(policy RestartPolicy) time.Duration {
	if policy.BackoffMillis > 0 {
		return time.Duration(policy.BackoffMillis) * time.Millisecond
	}
	return defaultRestartBackoff
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	plugin, ok := m.Get(id)
	if !ok {
		return fs.ErrNotExist
	}
	if err := m.stopRuntime(ctx, *plugin); err != nil {
		m.recordAudit(id, AuditActionPluginStopFailed, "failed", "", err.Error(), nil)
		logger.Errorf(ctx, "[Plugin] stop failed id=%s error=%v", id, err)
		return err
	}
	if err := m.SetStatus(id, StatusDisabled, nil); err != nil {
		return err
	}
	m.recordAudit(id, AuditActionPluginStopped, "success", "", "plugin stopped", nil)
	logger.Infof(ctx, "[Plugin] stopped id=%s", id)
	return nil
}

func (m *Manager) StopAll(ctx context.Context) error {
	for _, plugin := range m.List("") {
		if plugin.Status != StatusRunning && plugin.Status != StatusFailed {
			continue
		}
		if err := m.stopRuntime(ctx, plugin); err != nil {
			logger.Errorf(ctx, "[Plugin] stop all failed id=%s error=%v", plugin.Manifest.Metadata.ID, err)
			return err
		}
	}
	if err := m.runtime.StopAll(ctx); err != nil {
		logger.Errorf(ctx, "[Plugin] stop all failed error=%v", err)
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, plugin := range m.byID {
		if plugin.Status == StatusRunning || plugin.Status == StatusFailed {
			plugin.Status = StatusDisabled
			plugin.LastError = ""
		}
	}
	return nil
}

func (m *Manager) stopRuntime(ctx context.Context, plugin Plugin) error {
	if m.runtime.IsStarted(plugin.Manifest.Metadata.ID) {
		shutdownCtx, cancel := context.WithTimeout(ctx, defaultShutdownTimeout)
		client, err := Dial(shutdownCtx, plugin.Manifest.Spec.Entrypoint.GRPCAddress)
		if err == nil {
			err = client.Shutdown(shutdownCtx)
			closeErr := client.Close()
			if err == nil {
				err = closeErr
			}
		}
		cancel()
		if err != nil {
			logger.Warnf(ctx, "[Plugin] graceful shutdown failed id=%s error=%v; forcing runtime stop", plugin.Manifest.Metadata.ID, err)
		}
	}
	return m.runtime.Stop(ctx, plugin.Manifest.Metadata.ID)
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
