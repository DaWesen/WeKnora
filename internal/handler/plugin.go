package handler

import (
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/gin-gonic/gin"
)

const maxPluginAuditLimit = 512

// PluginHandler exposes the deployment-level external plugin control plane.
type PluginHandler struct {
	manager *plugin.Manager
}

func NewPluginHandler(manager *plugin.Manager) *PluginHandler {
	return &PluginHandler{manager: manager}
}

type pluginRestartPolicyResponse struct {
	Enabled       bool `json:"enabled"`
	MaxAttempts   int  `json:"max_attempts,omitempty"`
	WindowSeconds int  `json:"window_seconds,omitempty"`
	BackoffMillis int  `json:"backoff_millis,omitempty"`
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

func pluginForResponse(value plugin.Plugin) pluginResponse {
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
	return response
}

func pluginAuditForResponse(event plugin.AuditEvent) pluginAuditEventResponse {
	return pluginAuditEventResponse{
		ID:        event.ID,
		Timestamp: event.Timestamp,
		PluginID:  event.PluginID,
		Action:    event.Action,
		Outcome:   event.Outcome,
		Details:   event.Details,
	}
}

// List returns all manifests discovered by this application instance.
func (h *PluginHandler) List(c *gin.Context) {
	plugins := h.manager.List("")
	response := make([]pluginResponse, 0, len(plugins))
	for _, value := range plugins {
		response = append(response, pluginForResponse(value))
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
	c.JSON(http.StatusOK, gin.H{"success": true, "data": pluginForResponse(*value)})
}

// ListAudit returns the bounded in-process audit history. Target and Message
// are intentionally omitted because they can contain runtime addresses or
// downstream error text.
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
	events := h.manager.AuditEvents(plugin.AuditQuery{
		PluginID: id,
		Action:   c.Query("action"),
		Limit:    limit,
	})
	response := make([]pluginAuditEventResponse, 0, len(events))
	for _, event := range events {
		response = append(response, pluginAuditForResponse(event))
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": response})
}

func parsePluginAuditLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 || limit > maxPluginAuditLimit {
		return 0, errors.New("limit must be between 1 and 512")
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
	c.JSON(http.StatusOK, gin.H{"success": true, "data": pluginForResponse(*value)})
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
