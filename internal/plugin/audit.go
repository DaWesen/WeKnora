package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// AuditEvent describes a security-relevant plugin lifecycle event. It keeps
// configuration values and document content out of the audit trail.
type AuditEvent struct {
	ID        uint64            `json:"id"`
	Timestamp time.Time         `json:"timestamp"`
	PluginID  string            `json:"pluginId"`
	Action    string            `json:"action"`
	Outcome   string            `json:"outcome"`
	Target    string            `json:"target,omitempty"`
	Message   string            `json:"message,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
}

const (
	AuditActionPluginStarted           = "plugin.started"
	AuditActionPluginStartFailed       = "plugin.start_failed"
	AuditActionPluginStopped           = "plugin.stopped"
	AuditActionPluginStopFailed        = "plugin.stop_failed"
	AuditActionPluginHealthFailed      = "plugin.health_failed"
	AuditActionPluginHealthRecovered   = "plugin.health_recovered"
	AuditActionPluginConfigFailed      = "plugin.config_failed"
	AuditActionPluginCredentialsDenied = "plugin.credentials_denied"
	AuditActionPluginIdentityFail      = "plugin.identity_failed"
	AuditActionPluginNetworkDenied     = "plugin.network_denied"
	AuditActionPluginRuntimeFailed     = "plugin.runtime_failed"
	AuditActionPluginRestarted         = "plugin.restarted"
	AuditActionPluginRestartDenied     = "plugin.restart_denied"
)

const defaultAuditEventLimit = 512

// AuditQuery narrows the in-memory event history returned by Manager. Limit 0
// uses the default cap, and events are returned newest first.
type AuditQuery struct {
	PluginID string
	Action   string
	Limit    int
}

// AuditLog keeps a bounded local history. The manager deliberately never lets
// audit recording interrupt plugin work, including when durable audit storage
// is temporarily unavailable.
type AuditLog struct {
	mu     sync.RWMutex
	limit  int
	nextID uint64
	events []AuditEvent
}

// auditSink is deliberately small so the plugin runtime depends only on the
// append operation it needs. Production injects AuditLogService; tests can use
// a focused in-memory sink.
type auditSink interface {
	Log(context.Context, *types.AuditLog) error
}

func persistAuditEvent(sink auditSink, event AuditEvent) {
	if sink == nil {
		return
	}
	details, err := json.Marshal(event.Details)
	if err != nil {
		logger.Errorf(context.Background(), "[Plugin] encode audit details id=%s error=%v", event.PluginID, err)
		return
	}
	if err := sink.Log(context.Background(), &types.AuditLog{
		Action:     types.AuditAction(event.Action),
		ScopeType:  "plugin",
		ScopeID:    event.PluginID,
		TargetType: "plugin",
		TargetID:   event.PluginID,
		Outcome:    types.AuditOutcome(event.Outcome),
		Details:    types.JSON(details),
		CreatedAt:  event.Timestamp,
	}); err != nil {
		logger.Errorf(context.Background(), "[Plugin] persist audit event id=%s action=%s error=%v", event.PluginID, event.Action, err)
	}
}

func NewAuditLog(limit int) *AuditLog {
	if limit <= 0 {
		limit = defaultAuditEventLimit
	}
	return &AuditLog{limit: limit}
}

func (l *AuditLog) Record(event AuditEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.nextID++
	event.ID = l.nextID
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	event.Details = cloneAuditDetails(event.Details)
	l.events = append(l.events, event)
	if len(l.events) > l.limit {
		l.events = append([]AuditEvent(nil), l.events[len(l.events)-l.limit:]...)
	}
}

func (l *AuditLog) List(query AuditQuery) []AuditEvent {
	l.mu.RLock()
	defer l.mu.RUnlock()

	limit := query.Limit
	if limit <= 0 || limit > l.limit {
		limit = l.limit
	}
	result := make([]AuditEvent, 0, limit)
	for index := len(l.events) - 1; index >= 0 && len(result) < limit; index-- {
		event := l.events[index]
		if query.PluginID != "" && event.PluginID != query.PluginID {
			continue
		}
		if query.Action != "" && event.Action != query.Action {
			continue
		}
		event.Details = cloneAuditDetails(event.Details)
		result = append(result, event)
	}
	return result
}

func cloneAuditDetails(details map[string]string) map[string]string {
	if len(details) == 0 {
		return nil
	}
	result := make(map[string]string, len(details))
	for key, value := range details {
		if strings.TrimSpace(key) != "" {
			result[key] = value
		}
	}
	return result
}
