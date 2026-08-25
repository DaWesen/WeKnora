package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

type automaticRecovery struct {
	cancel   context.CancelFunc
	done     chan struct{}
	previous *automaticRecovery
}

type healthMonitor struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type healthState struct {
	consecutiveFailures int
	lastCheckedAt       time.Time
	lastFailureAt       time.Time
}

type healthCheckResult struct {
	failureThresholdReached bool
	recoveredFailures       int
}

// HealthStatus describes periodic health monitoring without exposing runtime endpoints.
type HealthStatus struct {
	Enabled             bool
	IntervalSeconds     int
	TimeoutSeconds      int
	FailureThreshold    int
	ConsecutiveFailures int
	Monitoring          bool
	LastCheckedAt       time.Time
	LastFailureAt       time.Time
}

// RestartStatus is a safe snapshot of the manifest restart policy and its
// current in-memory budget consumption. It intentionally excludes timestamps
// of individual attempts and all runtime endpoint details.
type RestartStatus struct {
	Enabled       bool
	MaxAttempts   int
	WindowSeconds int
	BackoffMillis int
	Attempts      int
	Remaining     int
	Restarting    bool
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
	restartConfigs  map[string]map[string]string
	recoveries      map[string]*automaticRecovery
	healthMonitors  map[string]*healthMonitor
	healthStates    map[string]*healthState
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
		restartConfigs:  make(map[string]map[string]string),
		recoveries:      make(map[string]*automaticRecovery),
		healthMonitors:  make(map[string]*healthMonitor),
		healthStates:    make(map[string]*healthState),
	}
	manager.runtime.SetProcessExitHandler(manager.handleRuntimeFailure)
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

// RecordCredentialsDenied records only the rejection outcome. Credential values
// and plugin diagnostic text must never enter the audit trail.
func (m *Manager) RecordCredentialsDenied(pluginID string) {
	m.recordAudit(pluginID, AuditActionPluginCredentialsDenied, "denied", "", "plugin credentials rejected", nil)
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
		safeErr := errors.New("plugin health check failed")
		_ = m.SetStatus(id, StatusFailed, safeErr)
		m.recordAudit(id, AuditActionPluginHealthFailed, "failed", "", safeErr.Error(), nil)
		logger.Errorf(ctx, "[Plugin] health check failed id=%s error=%v", id, err)
		return safeErr
	}
	if err := m.validateRuntimeConfig(ctx, *plugin, config); err != nil {
		_ = m.runtime.Stop(context.Background(), id)
		_ = m.SetStatus(id, StatusFailed, err)
		m.recordAudit(id, AuditActionPluginConfigFailed, "denied", "", "plugin rejected runtime configuration", map[string]string{"stage": "runtime_config"})
		logger.Errorf(ctx, "[Plugin] runtime config validation failed id=%s error=%v", id, err)
		return err
	}
	if err := m.verifyIdentity(ctx, *plugin); err != nil {
		_ = m.runtime.Stop(context.Background(), id)
		_ = m.SetStatus(id, StatusFailed, err)
		m.recordAudit(id, AuditActionPluginIdentityFail, "failed", "", err.Error(), nil)
		logger.Errorf(ctx, "[Plugin] identity verification failed id=%s error=%v", id, err)
		return err
	}
	m.rememberRestartConfig(id, config)
	m.resetHealthState(id)
	m.recordAudit(id, AuditActionPluginStarted, "success", "", "plugin started", map[string]string{
		"extension_type":  string(plugin.Manifest.Spec.ExtensionType),
		"network_enabled": fmt.Sprintf("%t", plugin.Manifest.Spec.Permissions.Network.Enabled),
	})
	m.startHealthMonitor(id)
	logger.Infof(ctx, "[Plugin] started id=%s type=%s network=%t", id, plugin.Manifest.Spec.ExtensionType, plugin.Manifest.Spec.Permissions.Network.Enabled)
	return nil
}

// validateRuntimeConfig lets a started plugin apply business-level validation
// after the manifest schema has accepted the configuration. Audit events remain
// deliberately generic so plugin-provided diagnostics never enter durable logs.
func (m *Manager) validateRuntimeConfig(ctx context.Context, plugin Plugin, config map[string]string) error {
	client, err := Dial(ctx, plugin.Manifest.Spec.Entrypoint.GRPCAddress)
	if err != nil {
		return fmt.Errorf("dial plugin for runtime config validation: %w", err)
	}
	defer client.Close()

	response, err := client.ValidateConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("validate plugin runtime configuration: %w", err)
	}
	if response.GetValid() {
		return nil
	}

	fields := make([]string, 0, len(response.GetErrors()))
	for _, fieldError := range response.GetErrors() {
		field := fieldError.GetField()
		if field != "" {
			fields = append(fields, field)
		}
	}
	sort.Strings(fields)
	if len(fields) == 0 {
		return fmt.Errorf("plugin rejected runtime configuration")
	}
	return fmt.Errorf("plugin rejected runtime configuration for fields: %s", strings.Join(fields, ", "))
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
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if err := ctx.Err(); err != nil {
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
	if err := ctx.Err(); err != nil {
		return err
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
	// Transport errors can contain a plugin endpoint. Keep the detailed cause in
	// logs at its origin, but never retain it in control-plane state or audits.
	safeCause := errors.New("plugin runtime failed")
	if err := m.SetStatus(id, StatusFailed, safeCause); err != nil {
		return err
	}
	m.recordAudit(id, AuditActionPluginRuntimeFailed, "failed", "", safeCause.Error(), nil)
	return nil
}

// handleRuntimeFailure 在宿主进程或容器异常退出后异步恢复插件。配置仅保留
// 在内存中，避免将凭据写入审计记录或磁盘。Restart 仍是预算的唯一执行点。
func (m *Manager) handleRuntimeFailure(id string, cause error) {
	m.stopHealthMonitor(id)
	if err := m.MarkRuntimeFailed(id, cause); err != nil {
		logger.Errorf(context.Background(), "[Plugin] record runtime failure id=%s error=%v", id, err)
		return
	}
	m.scheduleAutomaticRecovery(id)
}

func (m *Manager) startHealthMonitor(id string) {
	plugin, ok := m.Get(id)
	if !ok || HealthCheckInterval(*plugin) == 0 {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	monitor := &healthMonitor{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	previous := m.healthMonitors[id]
	if previous != nil {
		previous.cancel()
	}
	m.healthMonitors[id] = monitor
	m.mu.Unlock()
	if previous != nil {
		<-previous.done
	}

	go func() {
		ticker := time.NewTicker(HealthCheckInterval(*plugin))
		defer ticker.Stop()
		defer m.endHealthMonitor(id, monitor)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, ok := m.Get(id)
				if !ok || current.Status != StatusRunning {
					return
				}
				err := CheckHealth(ctx, current.Manifest.Spec.Entrypoint.GRPCAddress, healthCheckTimeout(*current))
				if ctx.Err() != nil {
					return
				}
				result := m.recordHealthCheck(id, monitor, err, healthFailureThreshold(*current))
				if result.recoveredFailures > 0 {
					m.recordAudit(id, AuditActionPluginHealthRecovered, "success", "", "plugin health check recovered", map[string]string{
						"previous_consecutive_failures": strconv.Itoa(result.recoveredFailures),
					})
				}
				if result.failureThresholdReached && err != nil {
					m.handleHealthCheckFailure(id, monitor, err)
					return
				}
			}
		}
	}()
}

func (m *Manager) recordHealthCheck(id string, monitor *healthMonitor, checkErr error, threshold int) healthCheckResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.healthMonitors[id] != monitor {
		return healthCheckResult{}
	}
	state := m.healthStates[id]
	if state == nil {
		state = &healthState{}
		m.healthStates[id] = state
	}
	state.lastCheckedAt = time.Now().UTC()
	if checkErr == nil {
		result := healthCheckResult{recoveredFailures: state.consecutiveFailures}
		state.consecutiveFailures = 0
		state.lastFailureAt = time.Time{}
		return result
	}
	state.consecutiveFailures++
	state.lastFailureAt = state.lastCheckedAt
	return healthCheckResult{failureThresholdReached: state.consecutiveFailures >= threshold}
}

func (m *Manager) resetHealthState(id string) {
	m.mu.Lock()
	m.healthStates[id] = &healthState{}
	m.mu.Unlock()
}

func (m *Manager) clearHealthState(id string) {
	m.mu.Lock()
	delete(m.healthStates, id)
	m.mu.Unlock()
}

func (m *Manager) endHealthMonitor(id string, monitor *healthMonitor) {
	m.mu.Lock()
	if m.healthMonitors[id] == monitor {
		delete(m.healthMonitors, id)
	}
	m.mu.Unlock()
	close(monitor.done)
}

func (m *Manager) ownsHealthMonitor(id string, monitor *healthMonitor) bool {
	if monitor == nil {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.healthMonitors[id] == monitor
}

func (m *Manager) stopHealthMonitor(id string) {
	m.mu.Lock()
	monitor := m.healthMonitors[id]
	delete(m.healthMonitors, id)
	m.mu.Unlock()
	if monitor != nil {
		monitor.cancel()
	}
}

func (m *Manager) stopAllHealthMonitors() {
	m.mu.Lock()
	monitors := make([]*healthMonitor, 0, len(m.healthMonitors))
	for _, monitor := range m.healthMonitors {
		monitors = append(monitors, monitor)
	}
	m.healthMonitors = make(map[string]*healthMonitor)
	m.mu.Unlock()
	for _, monitor := range monitors {
		monitor.cancel()
	}
}

func (m *Manager) handleHealthCheckFailure(id string, monitor *healthMonitor, cause error) {
	if !m.ownsHealthMonitor(id, monitor) {
		return
	}
	m.stopHealthMonitor(id)
	if err := m.runtime.Stop(context.Background(), id); err != nil {
		logger.Errorf(context.Background(), "[Plugin] stop unhealthy runtime id=%s error=%v", id, err)
	}
	if err := m.MarkRuntimeFailed(id, cause); err != nil {
		logger.Errorf(context.Background(), "[Plugin] record health failure id=%s error=%v", id, err)
		return
	}
	m.recordAudit(id, AuditActionPluginHealthFailed, "failed", "", "plugin health check failed", nil)
	m.scheduleAutomaticRecovery(id)
}

func (m *Manager) scheduleAutomaticRecovery(id string) {
	config, ok := m.restartConfig(id)
	if !ok {
		m.recordAudit(id, AuditActionPluginRestartDenied, "denied", "", "automatic recovery requires retained runtime configuration", nil)
		return
	}
	plugin, ok := m.Get(id)
	if !ok || plugin.Manifest.Spec.RestartPolicy == nil || !plugin.Manifest.Spec.RestartPolicy.Enabled {
		return
	}

	ctx, recovery := m.beginAutomaticRecovery(id)
	go func() {
		defer m.endAutomaticRecovery(id, recovery)
		if !waitForPreviousRecovery(ctx, recovery) {
			return
		}
		for {
			canRetry, budgetExhausted := m.canRetryAutomaticRecovery(ctx, id)
			if !canRetry {
				if budgetExhausted {
					m.recordAudit(id, AuditActionPluginRestartDenied, "denied", "", "restart budget exhausted", nil)
				}
				return
			}
			err := m.Restart(ctx, id, config)
			if err == nil || ctx.Err() != nil {
				return
			}
			logger.Errorf(context.Background(), "[Plugin] automatic recovery attempt failed id=%s error=%v", id, err)
		}
	}()
}

// canRetryAutomaticRecovery keeps retry scheduling separate from Restart. Each
// actual attempt still goes through Restart so the policy budget, backoff and
// concurrent-restart guard have exactly one enforcement path. The second result
// tells the caller that the recovery chain ended because its budget was spent.
func (m *Manager) canRetryAutomaticRecovery(ctx context.Context, id string) (bool, bool) {
	if ctx.Err() != nil {
		return false, false
	}
	plugin, ok := m.Get(id)
	if !ok || plugin.Status != StatusFailed {
		return false, false
	}
	policy := plugin.Manifest.Spec.RestartPolicy
	if policy == nil || !policy.Enabled {
		return false, false
	}
	status, ok := m.RestartStatus(id)
	if !ok || status.Restarting {
		return false, false
	}
	return status.Remaining > 0, status.Remaining == 0
}

func (m *Manager) beginAutomaticRecovery(id string) (context.Context, *automaticRecovery) {
	ctx, cancel := context.WithCancel(context.Background())
	recovery := &automaticRecovery{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	if previous := m.recoveries[id]; previous != nil {
		previous.cancel()
		recovery.previous = previous
	}
	m.recoveries[id] = recovery
	m.mu.Unlock()
	return ctx, recovery
}

func waitForPreviousRecovery(ctx context.Context, recovery *automaticRecovery) bool {
	if recovery.previous == nil {
		return true
	}
	select {
	case <-recovery.previous.done:
		return true
	case <-ctx.Done():
		return false
	}
}

func cancelRecoveryChain(recovery *automaticRecovery) {
	for recovery != nil {
		recovery.cancel()
		recovery = recovery.previous
	}
}

func waitForRecoveryChain(ctx context.Context, recovery *automaticRecovery) error {
	for recovery != nil {
		select {
		case <-recovery.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		recovery = recovery.previous
	}
	return nil
}

func (m *Manager) endAutomaticRecovery(id string, recovery *automaticRecovery) {
	m.mu.Lock()
	if m.recoveries[id] == recovery {
		delete(m.recoveries, id)
	}
	m.mu.Unlock()
	close(recovery.done)
}

func (m *Manager) cancelAutomaticRecovery(ctx context.Context, id string) error {
	m.mu.RLock()
	recovery := m.recoveries[id]
	m.mu.RUnlock()
	if recovery == nil {
		return nil
	}
	cancelRecoveryChain(recovery)
	return waitForRecoveryChain(ctx, recovery)
}

func (m *Manager) rememberRestartConfig(id string, config map[string]string) {
	m.mu.Lock()
	m.restartConfigs[id] = clonePluginConfig(config)
	m.mu.Unlock()
}

func (m *Manager) restartConfig(id string) (map[string]string, bool) {
	m.mu.RLock()
	config, ok := m.restartConfigs[id]
	m.mu.RUnlock()
	return clonePluginConfig(config), ok
}

func clonePluginConfig(config map[string]string) map[string]string {
	if config == nil {
		return nil
	}
	clone := make(map[string]string, len(config))
	for key, value := range config {
		clone[key] = value
	}
	return clone
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

// RestartStatus returns the current restart budget for a discovered plugin.
// It is intended for operators to understand why a failed plugin can or cannot
// be retried; enforcement remains exclusively in Restart.
func (m *Manager) RestartStatus(id string) (RestartStatus, bool) {
	plugin, ok := m.Get(id)
	if !ok {
		return RestartStatus{}, false
	}
	policy := plugin.Manifest.Spec.RestartPolicy
	if policy == nil {
		return RestartStatus{}, true
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.restarts[id]
	attempts := 0
	if policy.Enabled {
		attempts = len(m.pruneRestartAttemptsLocked(id, *policy, time.Now().UTC(), state))
	}
	remaining := policy.MaxAttempts - attempts
	if remaining < 0 {
		remaining = 0
	}
	return RestartStatus{
		Enabled:       policy.Enabled,
		MaxAttempts:   policy.MaxAttempts,
		WindowSeconds: policy.WindowSeconds,
		BackoffMillis: policy.BackoffMillis,
		Attempts:      attempts,
		Remaining:     remaining,
		Restarting:    m.restarting[id],
	}, true
}

// HealthStatus returns the safe monitoring configuration and whether its monitor
// is currently active. It intentionally omits the health endpoint address.
func (m *Manager) HealthStatus(id string) (HealthStatus, bool) {
	plugin, ok := m.Get(id)
	if !ok {
		return HealthStatus{}, false
	}
	check := plugin.Manifest.Spec.HealthCheck
	if check == nil {
		return HealthStatus{}, true
	}
	m.mu.RLock()
	_, monitoring := m.healthMonitors[id]
	state := m.healthStates[id]
	status := HealthStatus{
		Enabled:          true,
		IntervalSeconds:  check.IntervalSeconds,
		TimeoutSeconds:   check.TimeoutSeconds,
		FailureThreshold: healthFailureThreshold(*plugin),
		Monitoring:       monitoring,
	}
	if state != nil {
		status.ConsecutiveFailures = state.consecutiveFailures
		status.LastCheckedAt = state.lastCheckedAt
		status.LastFailureAt = state.lastFailureAt
	}
	m.mu.RUnlock()
	return status, true
}

func (m *Manager) reserveRestartAttempt(id string, policy RestartPolicy, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.restarts[id]
	if state == nil {
		state = &restartState{}
		m.restarts[id] = state
	}
	active := m.pruneRestartAttemptsLocked(id, policy, now, state)
	if len(active) >= policy.MaxAttempts {
		return 0, fmt.Errorf("restart budget exhausted: %d attempts within %ds", policy.MaxAttempts, policy.WindowSeconds)
	}
	state.attempts = append(active, now)
	return len(state.attempts), nil
}

func (m *Manager) pruneRestartAttemptsLocked(id string, policy RestartPolicy, now time.Time, state *restartState) []time.Time {
	if state == nil {
		return nil
	}
	windowStart := now.Add(-time.Duration(policy.WindowSeconds) * time.Second)
	active := state.attempts[:0]
	for _, attemptedAt := range state.attempts {
		if !attemptedAt.Before(windowStart) {
			active = append(active, attemptedAt)
		}
	}
	state.attempts = active
	return active
}

func restartBackoff(policy RestartPolicy) time.Duration {
	if policy.BackoffMillis > 0 {
		return time.Duration(policy.BackoffMillis) * time.Millisecond
	}
	return defaultRestartBackoff
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	m.stopHealthMonitor(id)
	plugin, ok := m.Get(id)
	if !ok {
		return fs.ErrNotExist
	}
	if err := m.cancelAutomaticRecovery(ctx, id); err != nil {
		return err
	}
	if err := m.stopRuntime(ctx, *plugin); err != nil {
		m.recordAudit(id, AuditActionPluginStopFailed, "failed", "", err.Error(), nil)
		logger.Errorf(ctx, "[Plugin] stop failed id=%s error=%v", id, err)
		return err
	}
	if err := m.SetStatus(id, StatusDisabled, nil); err != nil {
		return err
	}
	m.forgetRestartConfig(id)
	m.clearHealthState(id)
	m.recordAudit(id, AuditActionPluginStopped, "success", "", "plugin stopped", nil)
	logger.Infof(ctx, "[Plugin] stopped id=%s", id)
	return nil
}

func (m *Manager) StopAll(ctx context.Context) error {
	m.stopAllHealthMonitors()
	if err := m.cancelAllAutomaticRecoveries(ctx); err != nil {
		return err
	}
	for _, plugin := range m.List("") {
		if plugin.Status != StatusRunning && plugin.Status != StatusFailed {
			continue
		}
		if plugin.Manifest.Metadata.ID == "" {
			continue
		}
		if err := m.stopRuntime(ctx, plugin); err != nil {
			logger.Errorf(ctx, "[Plugin] stop all failed id=%s error=%v", plugin.Manifest.Metadata.ID, err)
			return err
		}
		if err := m.SetStatus(plugin.Manifest.Metadata.ID, StatusDisabled, nil); err != nil {
			return err
		}
		m.recordAudit(plugin.Manifest.Metadata.ID, AuditActionPluginStopped, "success", "", "plugin stopped", map[string]string{"reason": "application_shutdown"})
	}
	m.mu.Lock()
	for _, plugin := range m.byID {
		if plugin.Status == StatusRunning || plugin.Status == StatusFailed {
			plugin.Status = StatusDisabled
			plugin.LastError = ""
		}
	}
	m.restartConfigs = make(map[string]map[string]string)
	m.healthStates = make(map[string]*healthState)
	m.mu.Unlock()
	return nil
}

func (m *Manager) cancelAllAutomaticRecoveries(ctx context.Context) error {
	m.mu.RLock()
	recoveries := make([]*automaticRecovery, 0, len(m.recoveries))
	for _, recovery := range m.recoveries {
		recoveries = append(recoveries, recovery)
	}
	m.mu.RUnlock()
	for _, recovery := range recoveries {
		cancelRecoveryChain(recovery)
	}
	for _, recovery := range recoveries {
		if err := waitForRecoveryChain(ctx, recovery); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) forgetRestartConfig(id string) {
	m.mu.Lock()
	delete(m.restartConfigs, id)
	m.mu.Unlock()
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

// HealthCheckInterval returns the configured monitoring interval for a plugin.
// A zero duration means the plugin has no periodic health monitoring configured.
func HealthCheckInterval(plugin Plugin) time.Duration {
	if check := plugin.Manifest.Spec.HealthCheck; check != nil {
		return time.Duration(check.IntervalSeconds) * time.Second
	}
	return 0
}

func healthFailureThreshold(plugin Plugin) int {
	if check := plugin.Manifest.Spec.HealthCheck; check != nil && check.FailureThreshold > 0 {
		return check.FailureThreshold
	}
	return 1
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
