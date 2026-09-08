package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MetricsSnapshot is the host-side cache of one plugin's latest metric poll.
type MetricsSnapshot struct {
	PluginID string              `json:"pluginId"`
	Samples  []*MetricSampleView `json:"samples"`
	// CollectedAt is the wall-clock time of the last successful poll. Zero
	// when the plugin reported no metrics yet.
	CollectedAt time.Time `json:"collectedAt"`
	// Unavailable is true when the plugin does not support metrics (older
	// SDK answers Unimplemented) or the last poll failed. Reason carries the
	// human-readable cause.
	Unavailable bool   `json:"unavailable"`
	Reason      string `json:"reason,omitempty"`
}

// MetricSampleView is the API-facing copy of a pluginpb.MetricSample.
type MetricSampleView struct {
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels,omitempty"`
	Timestamp int64             `json:"timestampUnixMillis"`
	Bounds    []float64         `json:"bucketBounds,omitempty"`
	Counts    []uint64          `json:"bucketCounts,omitempty"`
}

// MetricsQuery identifies the plugin whose cached snapshot is returned.
type MetricsQuery struct {
	PluginID string
}

// MetricsSnapshot returns the most recent cached metrics for a plugin. The
// snapshot is refreshed opportunistically by the periodic health monitor;
// calling this does not trigger a synchronous poll (plugins are polled at the
// health-check cadence to bound load).
func (m *Manager) MetricsSnapshot(pluginID string) (MetricsSnapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot, ok := m.metrics[pluginID]
	if !ok {
		return MetricsSnapshot{}, false
	}
	return snapshot, true
}

// CollectMetrics dials the plugin and pulls one round of metric samples into
// the cache. It returns a booleans saying whether the plugin supports metrics
// at all. A codes.Unimplemented answer marks the snapshot unavailable instead
// of failed — that is the expected state for older plugins.
func (m *Manager) CollectMetrics(ctx context.Context, pluginID string) error {
	plugin, ok := m.Get(pluginID)
	if !ok {
		return fmt.Errorf("plugin %q not found", pluginID)
	}
	if plugin.Status != StatusRunning {
		return fmt.Errorf("plugin %q is not running (status %s)", pluginID, plugin.Status)
	}
	client, err := Dial(ctx, plugin.Manifest.Spec.Entrypoint.GRPCAddress)
	if err != nil {
		m.storeMetricsSnapshot(pluginID, MetricsSnapshot{Unavailable: true, Reason: err.Error()})
		return err
	}
	defer client.Close()

	response, err := client.GetMetrics(ctx)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			// Older plugin: metrics are genuinely absent, not broken.
			m.storeMetricsSnapshot(pluginID, MetricsSnapshot{Unavailable: true, Reason: "plugin does not report metrics"})
			return nil
		}
		m.storeMetricsSnapshot(pluginID, MetricsSnapshot{Unavailable: true, Reason: err.Error()})
		return fmt.Errorf("call plugin GetMetrics: %w", err)
	}
	snapshot := MetricsSnapshot{
		PluginID:    pluginID,
		CollectedAt: time.Now().UTC(),
		Samples:     make([]*MetricSampleView, 0, len(response.GetSamples())),
	}
	for _, sample := range response.GetSamples() {
		view := &MetricSampleView{
			Name:      sample.GetName(),
			Kind:      sample.GetKind(),
			Value:     sample.GetValue(),
			Labels:    sample.GetLabels(),
			Timestamp: sample.GetTimestampUnixMillis(),
			Bounds:    sample.GetBucketBounds(),
			Counts:    sample.GetBucketCounts(),
		}
		snapshot.Samples = append(snapshot.Samples, view)
	}
	m.storeMetricsSnapshot(pluginID, snapshot)
	logger.Debugf(ctx, "[Plugin] collected metrics id=%s samples=%d", pluginID, len(snapshot.Samples))
	return nil
}

func (m *Manager) storeMetricsSnapshot(pluginID string, snapshot MetricsSnapshot) {
	m.mu.Lock()
	if m.metrics == nil {
		m.metrics = make(map[string]MetricsSnapshot)
	}
	m.metrics[pluginID] = snapshot
	m.mu.Unlock()
}

// metricsPoller is invoked from the health monitor loop after a successful
// health check. Failures never affect plugin status — metrics are advisory.
func (m *Manager) metricsPoller(id string) func(ctx context.Context) {
	return func(ctx context.Context) {
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := m.CollectMetrics(pollCtx, id); err != nil {
			// Advisory only: log at debug, do not audit-flood.
			logger.Debugf(ctx, "[Plugin] metrics poll skipped id=%s error=%v", id, err)
		}
	}
}
