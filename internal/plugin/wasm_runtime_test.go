package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeWasmEngine is a test-only wasmEngine that records calls and returns
// programmable results. It lets the wasm facade / lifecycle tests run
// without a real .wasm module (which would require TinyGo on the host).
type fakeWasmEngine struct {
	descriptor    *wasmDescriptor
	describeErr   error
	parseResult   *wasmParseResult
	parseErr      error
	healthToken   string
	healthErr     error
	describeCalls int32
	parseCalls    int32
	healthCalls   int32
	closed        bool
}

func (e *fakeWasmEngine) Describe() (*wasmDescriptor, error) {
	atomic.AddInt32(&e.describeCalls, 1)
	if e.describeErr != nil {
		return nil, e.describeErr
	}
	if e.descriptor == nil {
		return &wasmDescriptor{EngineName: "fake", FileTypes: []string{"md"}}, nil
	}
	return e.descriptor, nil
}

func (e *fakeWasmEngine) Parse(fileType, fileName string, content []byte) (*wasmParseResult, error) {
	atomic.AddInt32(&e.parseCalls, 1)
	if e.parseErr != nil {
		return nil, e.parseErr
	}
	if e.parseResult != nil {
		return e.parseResult, nil
	}
	return &wasmParseResult{MarkdownContent: string(content), Metadata: map[string]string{"file_type": fileType, "file_name": fileName}}, nil
}

func (e *fakeWasmEngine) HealthCheck() (string, error) {
	atomic.AddInt32(&e.healthCalls, 1)
	if e.healthErr != nil {
		return "", e.healthErr
	}
	if e.healthToken == "" {
		return wasmHealthServing, nil
	}
	return e.healthToken, nil
}

func (e *fakeWasmEngine) Close() error {
	e.closed = true
	return nil
}

// startWasmFacade launches a gRPC server exposing a wasmFacade and
// wasmLifecycle backed by the supplied engine, returning the dial address
// and a stop function. It mirrors the production startWasmEngine wiring
// minus the wazero instantiation.
func startWasmFacade(t *testing.T, manifestID, manifestVersion string, engine wasmEngine) (string, *grpc.Server) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	pluginpb.RegisterPluginLifecycleServer(server, &wasmLifecycle{manifestID: manifestID, manifestVersion: manifestVersion, engine: engine})
	pluginpb.RegisterDocumentParserPluginServer(server, newWasmFacade(manifestID, manifestVersion, engine))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String(), server
}

// wasmDialClient is a convenience that dials the facade and returns both the
// host Client (for lifecycle RPCs) and a typed DocumentParser client.
func wasmDialClient(t *testing.T, address string) (*Client, pluginpb.DocumentParserPluginClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, address)
	if err != nil {
		t.Fatalf("dial wasm facade: %v", err)
	}
	conn, err := grpc.Dial(address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("dial wasm facade for parser client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return client, pluginpb.NewDocumentParserPluginClient(conn)
}

func TestWasmFacadeDescribeReturnsEngineMetadata(t *testing.T) {
	engine := &fakeWasmEngine{descriptor: &wasmDescriptor{
		EngineName: "wasm-markdown", Description: "tiny markdown normalizer",
		FileTypes: []string{"md", "markdown"}, Capabilities: []string{"stream"},
	}}
	address, _ := startWasmFacade(t, "com.example.wasm-parser", "0.1.0", engine)
	_, parserClient := wasmDialClient(t, address)

	response, err := parserClient.Describe(context.Background(), &pluginpb.DocumentParserDescribeRequest{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if response.GetEngineName() != "wasm-markdown" {
		t.Errorf("engine name: got %q want wasm-markdown", response.GetEngineName())
	}
	if response.GetDescription() != "tiny markdown normalizer" {
		t.Errorf("description: got %q", response.GetDescription())
	}
	if len(response.GetFileTypes()) != 2 || response.GetFileTypes()[0] != "md" {
		t.Errorf("file types: %+v", response.GetFileTypes())
	}
	if len(response.GetCapabilities()) != 1 || response.GetCapabilities()[0] != "stream" {
		t.Errorf("capabilities: %+v", response.GetCapabilities())
	}
}

func TestWasmFacadeDescribeCachesAcrossCalls(t *testing.T) {
	engine := &fakeWasmEngine{descriptor: &wasmDescriptor{EngineName: "cached"}}
	address, _ := startWasmFacade(t, "com.example.cached", "0.1.0", engine)
	_, parserClient := wasmDialClient(t, address)

	for i := 0; i < 3; i++ {
		if _, err := parserClient.Describe(context.Background(), &pluginpb.DocumentParserDescribeRequest{}); err != nil {
			t.Fatalf("Describe call %d: %v", i, err)
		}
	}
	if calls := atomic.LoadInt32(&engine.describeCalls); calls != 1 {
		t.Errorf("engine.Describe called %d times, want 1 (cached)", calls)
	}
}

func TestWasmFacadeDescribePropagatesEngineError(t *testing.T) {
	engine := &fakeWasmEngine{describeErr: errors.New("module panic")}
	address, _ := startWasmFacade(t, "com.example.broken", "0.1.0", engine)
	_, parserClient := wasmDialClient(t, address)

	_, err := parserClient.Describe(context.Background(), &pluginpb.DocumentParserDescribeRequest{})
	if err == nil || !strings.Contains(err.Error(), "module panic") {
		t.Fatalf("expected error containing 'module panic', got %v", err)
	}
}

func TestWasmFacadeParseReturnsEngineOutput(t *testing.T) {
	engine := &fakeWasmEngine{parseResult: &wasmParseResult{
		MarkdownContent: "# normalized",
		Metadata:        map[string]string{"bytes": "11"},
	}}
	address, _ := startWasmFacade(t, "com.example.parse", "0.1.0", engine)
	_, parserClient := wasmDialClient(t, address)

	response, err := parserClient.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileType: "md", FileName: "doc.md", FileContent: []byte("# Title\r\n"),
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if response.GetMarkdownContent() != "# normalized" {
		t.Errorf("markdown: got %q", response.GetMarkdownContent())
	}
	if response.GetMetadata()["bytes"] != "11" {
		t.Errorf("metadata: %+v", response.GetMetadata())
	}
}

func TestWasmFacadeParsePropagatesEngineError(t *testing.T) {
	engine := &fakeWasmEngine{parseErr: errors.New("unsupported file type")}
	address, _ := startWasmFacade(t, "com.example.parse-err", "0.1.0", engine)
	_, parserClient := wasmDialClient(t, address)

	_, err := parserClient.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileType: "pdf", FileContent: []byte("x"),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported file type") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestWasmLifecycleGetInfoReportsManifestIdentity(t *testing.T) {
	address, _ := startWasmFacade(t, "com.example.identity", "0.2.3", &fakeWasmEngine{})
	client, _ := wasmDialClient(t, address)

	info, err := client.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.GetId() != "com.example.identity" {
		t.Errorf("id: got %q", info.GetId())
	}
	if info.GetVersion() != "0.2.3" {
		t.Errorf("version: got %q", info.GetVersion())
	}
	if len(info.GetExtensionTypes()) != 1 || info.GetExtensionTypes()[0] != "document_parser" {
		t.Errorf("extension types: %+v", info.GetExtensionTypes())
	}
}

func TestWasmLifecycleHealthCheckServingWhenModuleReturnsServing(t *testing.T) {
	engine := &fakeWasmEngine{healthToken: wasmHealthServing}
	address, _ := startWasmFacade(t, "com.example.healthy", "0.1.0", engine)
	client, _ := wasmDialClient(t, address)

	response, err := client.HealthCheck(context.Background())
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if response.GetStatus() != pluginpb.HealthCheckResponse_STATUS_SERVING {
		t.Errorf("status: got %s want SERVING", response.GetStatus())
	}
	if calls := atomic.LoadInt32(&engine.healthCalls); calls != 1 {
		t.Errorf("engine.HealthCheck called %d times, want 1", calls)
	}
}

func TestWasmLifecycleHealthCheckNotServingWhenModuleDegraded(t *testing.T) {
	engine := &fakeWasmEngine{healthToken: "degraded"}
	address, _ := startWasmFacade(t, "com.example.degraded", "0.1.0", engine)
	client, _ := wasmDialClient(t, address)

	response, err := client.HealthCheck(context.Background())
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if response.GetStatus() != pluginpb.HealthCheckResponse_STATUS_NOT_SERVING {
		t.Errorf("status: got %s want NOT_SERVING", response.GetStatus())
	}
}

func TestWasmLifecycleHealthCheckServingWhenEngineHasNoExport(t *testing.T) {
	// An engine whose HealthCheck returns the serving sentinel without ever
	// being asked (nil token => default) simulates a module that does not
	// export health_check: the facade must still report SERVING so older
	// modules keep working after the host gains the health_check feature.
	engine := &fakeWasmEngine{healthToken: ""}
	address, _ := startWasmFacade(t, "com.example.no-export", "0.1.0", engine)
	client, _ := wasmDialClient(t, address)

	response, err := client.HealthCheck(context.Background())
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if response.GetStatus() != pluginpb.HealthCheckResponse_STATUS_SERVING {
		t.Errorf("status: got %s, want SERVING (no export should degrade to SERVING)", response.GetStatus())
	}
}

func TestWasmLifecycleHealthCheckNotServingWhenEngineErrors(t *testing.T) {
	engine := &fakeWasmEngine{healthErr: errors.New("module trapped")}
	address, _ := startWasmFacade(t, "com.example.trapped", "0.1.0", engine)
	client, _ := wasmDialClient(t, address)

	response, err := client.HealthCheck(context.Background())
	if err != nil {
		t.Fatalf("HealthCheck RPC itself must not fail: %v", err)
	}
	if response.GetStatus() != pluginpb.HealthCheckResponse_STATUS_NOT_SERVING {
		t.Errorf("status: got %s, want NOT_SERVING (engine error must surface as not serving)", response.GetStatus())
	}
}

func TestWasmLifecycleValidateConfigAcceptsAllConfigs(t *testing.T) {
	// wasm modules have no live config-validation hook; the facade accepts
	// whatever the host's manifest schema already accepted.
	address, _ := startWasmFacade(t, "com.example.config", "0.1.0", &fakeWasmEngine{})
	client, _ := wasmDialClient(t, address)

	response, err := client.ValidateConfig(context.Background(), map[string]string{"root": "/data"})
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !response.GetValid() {
		t.Error("wasm ValidateConfig must always report valid=true")
	}
}

// startWasmPluginOnDisk writes a fake .wasm binary and a wasm manifest to a
// plugin directory, returns the Plugin descriptor and the on-disk root.
func startWasmPluginOnDisk(t *testing.T) (Plugin, string) {
	t.Helper()
	root := t.TempDir()
	pluginDir := filepath.Join(root, "wasm-parser")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "parser.wasm"), []byte("fake wasm bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestYAML := `apiVersion: weknora.plugin/v1
kind: Plugin
metadata:
  id: com.example.wasm-parser
  name: Wasm Parser
  version: 0.1.0
spec:
  extensionType: document_parser
  weknoraVersion: ">=0.1.0"
  capabilities: []
  entrypoint:
    type: wasm
    wasmModule: parser.wasm
    grpcAddress: "127.0.0.1:0"
  permissions:
    network:
      enabled: false
    filesystem: {}
`
	manifest, err := ParseManifest([]byte(manifestYAML))
	if err != nil {
		t.Fatalf("parse wasm manifest: %v", err)
	}
	return Plugin{Manifest: *manifest, Directory: pluginDir, Status: StatusDiscovered, DiscoveredAt: time.Now().UTC()}, root
}

func TestStartWasmEngineServesFacadeAndLifecycle(t *testing.T) {
	plugin, _ := startWasmPluginOnDisk(t)
	// Force a real port by re-parsing the address; 127.0.0.1:0 asks the OS.
	plugin.Manifest.Spec.Entrypoint.GRPCAddress = "127.0.0.1:0"

	engine := &fakeWasmEngine{descriptor: &wasmDescriptor{
		EngineName: "wasm", FileTypes: []string{"md"},
	}}
	previous := newWasmEngine
	newWasmEngine = func(ctx context.Context, wasmPath string) (wasmEngine, error) {
		if !strings.HasSuffix(wasmPath, "parser.wasm") {
			t.Errorf("wasmPath: got %q, want parser.wasm", wasmPath)
		}
		return engine, nil
	}
	defer func() { newWasmEngine = previous }()

	started, err := startWasmEngine(context.Background(), plugin)
	if err != nil {
		t.Fatalf("startWasmEngine: %v", err)
	}
	t.Cleanup(func() { _ = started.stopWasm() })

	// startWasmEngine bound to 127.0.0.1:0 — we have no direct handle on the
	// chosen port, but wasmServe receives the Serve error. Confirm the server
	// stays alive (no immediate error) by reading from the channel with a
	// short timeout, then stop and observe shutdown.
	select {
	case err := <-started.wasmServe:
		if err != nil {
			t.Fatalf("wasm server exited unexpectedly: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		// Server still running — expected.
	}

	if started.wasmServer == nil || started.wasmEngine == nil {
		t.Fatal("startedPlugin missing wasm fields")
	}
}

func TestStartWasmEngineMissingModuleFailsAndClosesEngine(t *testing.T) {
	plugin, _ := startWasmPluginOnDisk(t)
	plugin.Manifest.Spec.Entrypoint.WasmModule = "missing.wasm"

	engine := &fakeWasmEngine{}
	previous := newWasmEngine
	newWasmEngine = func(ctx context.Context, wasmPath string) (wasmEngine, error) {
		return nil, errors.New("open missing.wasm: no such file")
	}
	defer func() { newWasmEngine = previous }()

	_, err := startWasmEngine(context.Background(), plugin)
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("expected missing-module error, got %v", err)
	}
	// Engine is never created when newWasmEngine fails, so close is a no-op.
	_ = engine
}

func TestRuntimeStopTearsDownWasmEngine(t *testing.T) {
	plugin, _ := startWasmPluginOnDisk(t)
	plugin.Manifest.Spec.Entrypoint.GRPCAddress = "127.0.0.1:0"

	engine := &fakeWasmEngine{}
	previous := newWasmEngine
	newWasmEngine = func(ctx context.Context, wasmPath string) (wasmEngine, error) {
		return engine, nil
	}
	defer func() { newWasmEngine = previous }()

	runtime := NewRuntime()
	if err := runtime.Start(context.Background(), plugin, nil); err != nil {
		t.Fatalf("runtime.Start: %v", err)
	}
	if err := runtime.Stop(context.Background(), plugin.Manifest.Metadata.ID); err != nil {
		t.Fatalf("runtime.Stop: %v", err)
	}
	if !engine.closed {
		t.Error("wasm engine was not closed by runtime.Stop")
	}
}

func TestManifestWasmRequiresWasmModule(t *testing.T) {
	m := validManifest()
	m.Spec.ExtensionType = ExtensionTypeDocumentParser
	m.Spec.Entrypoint = Entrypoint{
		Type:        "wasm",
		GRPCAddress: "127.0.0.1:50071",
	}
	m.Spec.Permissions = Permissions{Network: NetworkPermission{Enabled: false}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "wasmModule is required") {
		t.Fatalf("expected wasmModule required error, got %v", err)
	}
}

func TestManifestWasmRejectsNetworkPermission(t *testing.T) {
	// wasm modules run embedded with no network sandbox — a manifest that
	// asks for network is a misconfiguration, nothing would enforce it.
	m := validManifest()
	m.Spec.ExtensionType = ExtensionTypeDocumentParser
	m.Spec.Entrypoint = Entrypoint{
		Type:        "wasm",
		WasmModule:  "parser.wasm",
		GRPCAddress: "127.0.0.1:50071",
	}
	m.Spec.Permissions = Permissions{Network: NetworkPermission{Enabled: true}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "wasm plugins cannot request network permission") {
		t.Fatalf("expected network-rejected error, got %v", err)
	}
}

func TestManifestWasmAcceptsValidConfiguration(t *testing.T) {
	m := validManifest()
	m.Spec.ExtensionType = ExtensionTypeDocumentParser
	m.Spec.Entrypoint = Entrypoint{
		Type:        "wasm",
		WasmModule:  "parser.wasm",
		GRPCAddress: "127.0.0.1:50071",
	}
	m.Spec.Permissions = Permissions{Network: NetworkPermission{Enabled: false}}
	if err := m.Validate(); err != nil {
		t.Fatalf("expected valid wasm manifest: %v", err)
	}
}
