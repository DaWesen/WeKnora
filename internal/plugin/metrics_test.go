package plugin

import (
	"context"
	"net"
	"testing"
	"time"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/codes"
)

// metricsFake is a lifecycle-only plugin server used to exercise the host's
// metrics collection without a real plugin binary.
type metricsFake struct {
	pluginsdk.Lifecycle
	serveMetrics bool
	registry     *pluginsdk.MetricsRegistry
}

func startMetricsFake(t *testing.T, f *metricsFake) string {
	t.Helper()
	f.Lifecycle.Metadata = pluginsdk.Metadata{ID: "com.example.metrics", Version: "0.1.0", ExtensionTypes: []string{"document_parser"}}
	if f.serveMetrics {
		f.Lifecycle.Metrics = f.registry
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	pluginpb.RegisterPluginLifecycleServer(server, f)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// newMetricsManager builds a Manager whose byID holds one fake running plugin
// bound to the given gRPC address. This bypasses runtime.Start (process spawn)
// because the metrics path only needs a dialable endpoint.
func newMetricsManager(t *testing.T, address string, serveMetrics bool, registry *pluginsdk.MetricsRegistry) *Manager {
	t.Helper()
	m := NewManager(t.TempDir())
	manifest := *mustTestManifest(t)
	manifest.Spec.Entrypoint.GRPCAddress = address
	m.mu.Lock()
	m.byID[manifest.Metadata.ID] = &Plugin{
		Manifest:     manifest,
		Directory:    t.TempDir(),
		Status:       StatusRunning,
		DiscoveredAt: time.Now().UTC(),
	}
	m.mu.Unlock()
	return m
}

func mustTestManifest(t *testing.T) *Manifest {
	t.Helper()
	manifest, err := ParseManifest([]byte(testManifestYAML))
	if err != nil {
		t.Fatalf("parse test manifest: %v", err)
	}
	return manifest
}

func TestCollectMetricsStoresSamples(t *testing.T) {
	registry := pluginsdk.NewMetricsRegistry()
	registry.Counter("documents_parsed", map[string]string{"type": "md"}).Add(3)
	registry.Gauge("queue_depth", nil).Set(7)
	registry.Histogram("parse_seconds", nil).Observe(0.25)

	fake := &metricsFake{serveMetrics: true, registry: registry}
	address := startMetricsFake(t, fake)
	manager := newMetricsManager(t, address, true, registry)

	if err := manager.CollectMetrics(context.Background(), "com.example.signed"); err != nil {
		t.Fatalf("CollectMetrics: %v", err)
	}
	snapshot, ok := manager.MetricsSnapshot("com.example.signed")
	if !ok {
		t.Fatal("expected cached snapshot")
	}
	if snapshot.Unavailable {
		t.Fatalf("snapshot marked unavailable: %s", snapshot.Reason)
	}
	if len(snapshot.Samples) != 3 {
		t.Fatalf("expected 3 samples, got %d", len(snapshot.Samples))
	}
	byName := map[string]*MetricSampleView{}
	for _, s := range snapshot.Samples {
		byName[s.Name] = s
	}
	if byName["documents_parsed"].Value != 3 || byName["documents_parsed"].Kind != "counter" {
		t.Errorf("counter sample wrong: %+v", byName["documents_parsed"])
	}
	if byName["queue_depth"].Value != 7 || byName["queue_depth"].Kind != "gauge" {
		t.Errorf("gauge sample wrong: %+v", byName["queue_depth"])
	}
	h := byName["parse_seconds"]
	if h.Kind != "histogram" || h.Value != 0.25 || len(h.Counts) != 12 {
		t.Errorf("histogram sample wrong: %+v", h)
	}
}

func TestCollectMetricsUnimplementedMarksUnavailable(t *testing.T) {
	// Plugin built against an older SDK: GetMetrics answers Unimplemented.
	// The host must record unavailable instead of an error.
	fake := &metricsFake{serveMetrics: false}
	address := startMetricsFake(t, fake)
	manager := newMetricsManager(t, address, false, nil)

	if err := manager.CollectMetrics(context.Background(), "com.example.signed"); err != nil {
		t.Fatalf("CollectMetrics should not fail on Unimplemented, got: %v", err)
	}
	snapshot, ok := manager.MetricsSnapshot("com.example.signed")
	if !ok || !snapshot.Unavailable {
		t.Fatal("expected unavailable snapshot")
	}
	if len(snapshot.Samples) != 0 {
		t.Errorf("expected no samples, got %d", len(snapshot.Samples))
	}
}

func TestCollectMetricsRequiresRunningPlugin(t *testing.T) {
	manager := NewManager(t.TempDir())
	err := manager.CollectMetrics(context.Background(), "no-such-plugin")
	if err == nil {
		t.Fatal("expected error for unknown plugin")
	}
}

func TestCollectMetricsDialFailureMarkedUnavailable(t *testing.T) {
	// Nothing listens on this address: dial fails, snapshot unavailable.
	manager := newMetricsManager(t, "127.0.0.1:1", false, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.CollectMetrics(ctx, "com.example.signed"); err == nil {
		t.Fatal("expected dial error")
	}
	snapshot, ok := manager.MetricsSnapshot("com.example.signed")
	if !ok || !snapshot.Unavailable {
		t.Fatal("expected unavailable snapshot after dial failure")
	}
}

// Direct client-level test of the Unimplemented path.
func TestClientGetMetricsUnimplemented(t *testing.T) {
	fake := &metricsFake{serveMetrics: false}
	address := startMetricsFake(t, fake)
	conn, err := grpc.Dial(address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client, err := Dial(context.Background(), address)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	_, err = client.GetMetrics(context.Background())
	if err == nil || status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented, got %v", err)
	}
}
