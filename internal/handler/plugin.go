package handler

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

const maxPluginAuditLimit = 100

// PluginHandler exposes the deployment-level external plugin control plane.
type PluginHandler struct {
	manager      *plugin.Manager
	auditService interfaces.AuditLogService
}

func NewPluginHandler(manager *plugin.Manager, auditService interfaces.AuditLogService) *PluginHandler {
	return &PluginHandler{manager: manager, auditService: auditService}
}

type pluginRestartPolicyResponse struct {
	Enabled       bool `json:"enabled"`
	MaxAttempts   int  `json:"max_attempts,omitempty"`
	WindowSeconds int  `json:"window_seconds,omitempty"`
	BackoffMillis int  `json:"backoff_millis,omitempty"`
}

type pluginRestartStateResponse struct {
	Enabled       bool `json:"enabled"`
	MaxAttempts   int  `json:"max_attempts,omitempty"`
	WindowSeconds int  `json:"window_seconds,omitempty"`
	BackoffMillis int  `json:"backoff_millis,omitempty"`
	Attempts      int  `json:"attempts"`
	Remaining     int  `json:"remaining"`
	Restarting    bool `json:"restarting"`
}

type pluginHealthStateResponse struct {
	Enabled             bool      `json:"enabled"`
	IntervalSeconds     int       `json:"interval_seconds,omitempty"`
	TimeoutSeconds      int       `json:"timeout_seconds,omitempty"`
	FailureThreshold    int       `json:"failure_threshold,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Monitoring          bool      `json:"monitoring"`
	LastCheckedAt       time.Time `json:"last_checked_at,omitempty"`
	LastFailureAt       time.Time `json:"last_failure_at,omitempty"`
}

// pluginResponse deliberately excludes runtime entrypoints, filesystem grants,
// and discovery directories because they disclose deployment topology.
type pluginResponse struct {
	ID            string                       `json:"id"`
	Name          string                       `json:"name"`
	Version       string                       `json:"version"`
	Description   string                       `json:"description,omitempty"`
	ExtensionType string                       `json:"extension_type"`
	Status        plugin.Status                `json:"status"`
	LastError     string                       `json:"last_error,omitempty"`
	DiscoveredAt  time.Time                    `json:"discovered_at"`
	RestartPolicy *pluginRestartPolicyResponse `json:"restart_policy,omitempty"`
	RestartState  *pluginRestartStateResponse  `json:"restart_state,omitempty"`
	HealthState   *pluginHealthStateResponse   `json:"health_state,omitempty"`
}

type pluginAuditEventResponse struct {
	ID        uint64            `json:"id"`
	Timestamp time.Time         `json:"timestamp"`
	PluginID  string            `json:"plugin_id"`
	Action    string            `json:"action"`
	Outcome   string            `json:"outcome"`
	Details   map[string]string `json:"details,omitempty"`
}

type restartPluginRequest struct {
	Config map[string]string `json:"config"`
}

func pluginForResponse(manager *plugin.Manager, value plugin.Plugin) pluginResponse {
	response := pluginResponse{
		ID:            value.Manifest.Metadata.ID,
		Name:          value.Manifest.Metadata.Name,
		Version:       value.Manifest.Metadata.Version,
		Description:   value.Manifest.Metadata.Description,
		ExtensionType: string(value.Manifest.Spec.ExtensionType),
		Status:        value.Status,
		LastError:     value.LastError,
		DiscoveredAt:  value.DiscoveredAt,
	}
	if policy := value.Manifest.Spec.RestartPolicy; policy != nil {
		response.RestartPolicy = &pluginRestartPolicyResponse{
			Enabled:       policy.Enabled,
			MaxAttempts:   policy.MaxAttempts,
			WindowSeconds: policy.WindowSeconds,
			BackoffMillis: policy.BackoffMillis,
		}
	}
	if state, ok := manager.RestartStatus(value.Manifest.Metadata.ID); ok && response.RestartPolicy != nil {
		response.RestartState = &pluginRestartStateResponse{
			Enabled:       state.Enabled,
			MaxAttempts:   state.MaxAttempts,
			WindowSeconds: state.WindowSeconds,
			BackoffMillis: state.BackoffMillis,
			Attempts:      state.Attempts,
			Remaining:     state.Remaining,
			Restarting:    state.Restarting,
		}
	}
	if state, ok := manager.HealthStatus(value.Manifest.Metadata.ID); ok && state.Enabled {
		response.HealthState = &pluginHealthStateResponse{
			Enabled:             state.Enabled,
			IntervalSeconds:     state.IntervalSeconds,
			TimeoutSeconds:      state.TimeoutSeconds,
			FailureThreshold:    state.FailureThreshold,
			ConsecutiveFailures: state.ConsecutiveFailures,
			Monitoring:          state.Monitoring,
			LastCheckedAt:       state.LastCheckedAt,
			LastFailureAt:       state.LastFailureAt,
		}
	}
	return response
}

func pluginAuditForResponse(entry *types.AuditLog) pluginAuditEventResponse {
	response := pluginAuditEventResponse{
		ID:        entry.ID,
		Timestamp: entry.CreatedAt,
		PluginID:  entry.ScopeID,
		Action:    string(entry.Action),
		Outcome:   string(entry.Outcome),
	}
	// Details are written by the plugin runtime from a bounded, redacted map.
	// Invalid historical payloads remain readable without failing the whole feed.
	_ = json.Unmarshal(entry.Details, &response.Details)
	return response
}

// List returns all manifests discovered by this application instance.
func (h *PluginHandler) List(c *gin.Context) {
	plugins := h.manager.List("")
	response := make([]pluginResponse, 0, len(plugins))
	for _, value := range plugins {
		response = append(response, pluginForResponse(h.manager, value))
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": response})
}

// Get returns safe deployment status for one external plugin.
func (h *PluginHandler) Get(c *gin.Context) {
	value, ok := h.manager.Get(c.Param("id"))
	if !ok {
		c.Error(apperrors.NewNotFoundError("plugin not found"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": pluginForResponse(h.manager, *value)})
}

// ListAudit returns durable system-scope plugin events. Target and Message are
// never persisted, so runtime addresses and downstream error text stay out of
// this control-plane response even after an application restart.
func (h *PluginHandler) ListAudit(c *gin.Context) {
	id := c.Param("id")
	if _, ok := h.manager.Get(id); !ok {
		c.Error(apperrors.NewNotFoundError("plugin not found"))
		return
	}

	limit, err := parsePluginAuditLimit(c.Query("limit"))
	if err != nil {
		c.Error(apperrors.NewValidationError(err.Error()))
		return
	}
	entries, err := h.auditService.List(c.Request.Context(), 0, &interfaces.AuditLogQuery{
		Limit:     limit,
		Action:    types.AuditAction(c.Query("action")),
		ScopeType: "plugin",
		ScopeID:   id,
	})
	if err != nil {
		c.Error(apperrors.NewInternalServerError("list plugin audit history"))
		return
	}
	response := make([]pluginAuditEventResponse, 0, len(entries))
	for _, entry := range entries {
		response = append(response, pluginAuditForResponse(entry))
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": response})
}

func parsePluginAuditLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 || limit > maxPluginAuditLimit {
		return 0, errors.New("limit must be between 1 and 100")
	}
	return limit, nil
}

// Restart restarts only failed plugins and leaves restart-budget enforcement to
// Manager.Restart. Config is supplied again because the runtime never stores it.
func (h *PluginHandler) Restart(c *gin.Context) {
	var request restartPluginRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewValidationError("invalid restart request").WithDetails(err.Error()))
		return
	}
	if err := h.manager.Restart(c.Request.Context(), c.Param("id"), request.Config); err != nil {
		handlePluginRestartError(c, err)
		return
	}
	value, ok := h.manager.Get(c.Param("id"))
	if !ok {
		c.Error(apperrors.NewNotFoundError("plugin not found"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": pluginForResponse(h.manager, *value)})
}

func handlePluginRestartError(c *gin.Context, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		c.Error(apperrors.NewNotFoundError("plugin not found"))
		return
	}
	message := err.Error()
	if strings.Contains(message, "plugin configuration requires") ||
		strings.Contains(message, "plugin config schema") {
		c.Error(apperrors.NewValidationError(message))
		return
	}
	c.Error(apperrors.NewConflictError(message))
}
