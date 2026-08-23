package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type recordingAuditSink struct {
	entries []*types.AuditLog
}

func (s *recordingAuditSink) Log(_ context.Context, entry *types.AuditLog) error {
	copy := *entry
	s.entries = append(s.entries, &copy)
	return nil
}

type failingAuditSink struct{}

func (failingAuditSink) Log(context.Context, *types.AuditLog) error {
	return errors.New("audit store unavailable")
}

func TestAuditLogReturnsNewestMatchingEvents(t *testing.T) {
	log := NewAuditLog(3)
	log.Record(AuditEvent{PluginID: "first", Action: AuditActionPluginStarted, Outcome: "success"})
	log.Record(AuditEvent{PluginID: "target", Action: AuditActionPluginNetworkDenied, Outcome: "denied", Target: "example.com:443"})
	log.Record(AuditEvent{PluginID: "target", Action: AuditActionPluginStopped, Outcome: "success"})
	log.Record(AuditEvent{PluginID: "target", Action: AuditActionPluginNetworkDenied, Outcome: "denied", Target: "api.example.com:443"})

	events := log.List(AuditQuery{PluginID: "target"})
	require.Len(t, events, 3)
	require.Equal(t, AuditActionPluginNetworkDenied, events[0].Action)
	require.Equal(t, "api.example.com:443", events[0].Target)
	require.Equal(t, AuditActionPluginStopped, events[1].Action)
	require.Equal(t, uint64(4), events[0].ID)
	require.False(t, events[0].Timestamp.IsZero())

	denied := log.List(AuditQuery{Action: AuditActionPluginNetworkDenied, Limit: 1})
	require.Len(t, denied, 1)
	require.Equal(t, "api.example.com:443", denied[0].Target)
}

func TestAuditLogDoesNotExposeMutableDetails(t *testing.T) {
	log := NewAuditLog(1)
	details := map[string]string{"stage": "runtime_start"}
	log.Record(AuditEvent{PluginID: "plugin", Action: AuditActionPluginStartFailed, Details: details})
	details["stage"] = "changed"

	events := log.List(AuditQuery{})
	require.Equal(t, "runtime_start", events[0].Details["stage"])
	events[0].Details["stage"] = "changed again"

	events = log.List(AuditQuery{})
	require.Equal(t, "runtime_start", events[0].Details["stage"])
}

func TestManagerRecordsNetworkDenial(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.RecordNetworkDenied("com.example.local-files", "api.example.com:443", "outbound network is disabled")

	events := manager.AuditEvents(AuditQuery{PluginID: "com.example.local-files"})
	require.Len(t, events, 1)
	require.Equal(t, AuditActionPluginNetworkDenied, events[0].Action)
	require.Equal(t, "denied", events[0].Outcome)
	require.Equal(t, "api.example.com:443", events[0].Target)
	require.Equal(t, "outbound network is disabled", events[0].Message)
}

func TestManagerPersistsPluginAuditWithoutMessageOrTargetLeakage(t *testing.T) {
	sink := &recordingAuditSink{}
	manager := NewManagerWithAudit(t.TempDir(), sink)
	manager.recordAudit(
		"com.example.local-files",
		AuditActionPluginNetworkDenied,
		"denied",
		"api.example.com:443",
		"credential=must-not-be-persisted",
		map[string]string{"stage": "network_policy"},
	)

	require.Len(t, sink.entries, 1)
	entry := sink.entries[0]
	require.Equal(t, types.AuditActionPluginNetworkDenied, entry.Action)
	require.Equal(t, "plugin", entry.ScopeType)
	require.Equal(t, "com.example.local-files", entry.ScopeID)
	require.Equal(t, "plugin", entry.TargetType)
	require.Equal(t, "com.example.local-files", entry.TargetID)
	require.Equal(t, types.AuditOutcomeDenied, entry.Outcome)
	require.False(t, entry.CreatedAt.IsZero())
	memoryEvents := manager.AuditEvents(AuditQuery{PluginID: "com.example.local-files"})
	require.Len(t, memoryEvents, 1)
	require.Equal(t, memoryEvents[0].Timestamp, entry.CreatedAt)
	var details map[string]string
	require.NoError(t, json.Unmarshal(entry.Details, &details))
	require.Equal(t, map[string]string{"stage": "network_policy"}, details)
	require.NotContains(t, string(entry.Details), "must-not-be-persisted")
	require.NotContains(t, string(entry.Details), "api.example.com")
}

func TestManagerKeepsInMemoryAuditWhenPersistentWriteFails(t *testing.T) {
	manager := NewManagerWithAudit(t.TempDir(), failingAuditSink{})

	require.NotPanics(t, func() {
		manager.RecordNetworkDenied("com.example.local-files", "api.example.com:443", "outbound network is disabled")
	})

	events := manager.AuditEvents(AuditQuery{PluginID: "com.example.local-files"})
	require.Len(t, events, 1)
	require.Equal(t, AuditActionPluginNetworkDenied, events[0].Action)
	require.Equal(t, "denied", events[0].Outcome)
}
