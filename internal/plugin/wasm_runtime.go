package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	wasi "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"google.golang.org/grpc"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

// wasmEngine is the seam between the host's wasm runtime (wazero) and the
// gRPC facade. Tests substitute a fake engine; production uses wazeroModule.
//
// ABI (all exports are optional except describe):
//
//	describe() -> ptr            returns a pointer to a NUL-terminated JSON
//	                               document: {"engine_name": "...",
//	                               "description": "...", "file_types": ["md", ...],
//	                               "capabilities": [...]}
//	parse(len) -> ptr             parses len bytes previously written into the
//	                               buffer exposed by input_buffer(); returns
//	                               NUL-terminated JSON:
//	                               {"markdown_content": "...", "metadata": {...}}
//	input_buffer() -> ptr        returns the address of a writable buffer the
//	                               host fills with input bytes before calling parse
//	health_check() -> ptr        optional; returns a NUL-terminated status token
//	                               ("serving" / "degraded" / "stopped"). When
//	                               absent, the facade reports SERVING — matching
//	                               the lifecycle-only behaviour of older modules.
//
// Strings live in the module's exported linear memory "memory". The host
// never frees module memory: alloc must be provided by the module if it
// needs input bytes written in (the facade writes at the address the module
// exposes via the exported "input_buffer" function when present).
type wasmEngine interface {
	Describe() (*wasmDescriptor, error)
	Parse(fileType, fileName string, content []byte) (*wasmParseResult, error)
	HealthCheck() (string, error)
	Close() error
}

// wasmHealthServing is the status string a module returns from health_check
// when it is fully operational. The facade maps it to the gRPC SERVING enum.
const wasmHealthServing = "serving"

type wasmDescriptor struct {
	EngineName   string   `json:"engine_name"`
	Description  string   `json:"description"`
	FileTypes    []string `json:"file_types"`
	Capabilities []string `json:"capabilities"`
}

type wasmParseResult struct {
	MarkdownContent string            `json:"markdown_content"`
	Metadata        map[string]string `json:"metadata"`
}

// wazeroEngine instantiates a .wasm module with the wazero runtime (pure Go,
// no CGO) and adapts its exported functions to wasmEngine. Modules run with
// WASI enabled (stdout/stderr discarded) and no host functions: no network,
// no filesystem, no clock — the isolation the manifest promises.
type wazeroEngine struct {
	runtime wazero.Runtime
	module  api.Module
	memory  api.Memory
}

func newWazeroEngine(ctx context.Context, wasmPath string) (*wazeroEngine, error) {
	wasmBytes, err := readFileAll(wasmPath)
	if err != nil {
		return nil, fmt.Errorf("read wasm module: %w", err)
	}
	runtime := wazero.NewRuntime(ctx)
	// WASI is enabled so TinyGo modules can run their runtime init; the
	// module still gets no host capabilities beyond memory.
	wasi.MustInstantiate(ctx, runtime)
	// The default ModuleConfig invokes `_start` on instantiation. TinyGo's
	// wasi `_start` runs main() and then proc_exit(0), which would close the
	// module before the host can call describe/parse. We are driving the
	// module purely through exported functions, so start no entrypoint.
	module, err := runtime.InstantiateWithConfig(ctx, wasmBytes, wazero.NewModuleConfig().WithStartFunctions())
	if err != nil {
		runtime.Close(ctx)
		return nil, fmt.Errorf("instantiate wasm module %s: %w", wasmPath, err)
	}
	memory := module.ExportedMemory("memory")
	if memory == nil {
		runtime.Close(ctx)
		return nil, fmt.Errorf("wasm module %s exports no linear memory", wasmPath)
	}
	return &wazeroEngine{runtime: runtime, module: module, memory: memory}, nil
}

// newWasmEngine is the engine factory used by startWasmEngine. It is a
// package-level variable so tests can substitute a fake engine without
// compiling a real .wasm module (the wazero integration itself is exercised
// by examples/wasm-parser-plugin, which builds with TinyGo).
var newWasmEngine = func(ctx context.Context, wasmPath string) (wasmEngine, error) {
	engine, err := newWazeroEngine(ctx, wasmPath)
	if err != nil {
		return nil, err
	}
	return engine, nil
}

// readString reads a NUL-terminated string starting at ptr.
func (e *wazeroEngine) readString(ptr uint32) (string, error) {
	if ptr == 0 {
		return "", fmt.Errorf("wasm returned null pointer")
	}
	data, ok := e.memory.Read(ptr, e.memory.Size()-ptr)
	if !ok {
		return "", fmt.Errorf("wasm memory read failed at %d", ptr)
	}
	for i, b := range data {
		if b == 0 {
			return string(data[:i]), nil
		}
	}
	return "", fmt.Errorf("wasm string at %d is not NUL-terminated", ptr)
}

func (e *wazeroEngine) Describe() (*wasmDescriptor, error) {
	fn := e.module.ExportedFunction("describe")
	if fn == nil {
		return nil, fmt.Errorf("wasm module exports no describe function")
	}
	results, err := fn.Call(context.Background())
	if err != nil {
		return nil, fmt.Errorf("wasm describe call: %w", err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("wasm describe returned no pointer")
	}
	raw, err := e.readString(uint32(results[0]))
	if err != nil {
		return nil, err
	}
	var descriptor wasmDescriptor
	if err := json.Unmarshal([]byte(raw), &descriptor); err != nil {
		return nil, fmt.Errorf("wasm describe JSON: %w", err)
	}
	return &descriptor, nil
}

func (e *wazeroEngine) Parse(fileType, fileName string, content []byte) (*wasmParseResult, error) {
	fn := e.module.ExportedFunction("parse")
	if fn == nil {
		return nil, fmt.Errorf("wasm module exports no parse function")
	}
	// Input ABI: the module exports input_buffer() -> ptr and keeps a
	// buffer large for the largest expected document. Without it the host
	// cannot hand bytes to a sandboxed module.
	bufFn := e.module.ExportedFunction("input_buffer")
	if bufFn == nil {
		return nil, fmt.Errorf("wasm module exports no input_buffer function")
	}
	ptrs, err := bufFn.Call(context.Background())
	if err != nil || len(ptrs) == 0 {
		return nil, fmt.Errorf("wasm input_buffer call: %v", err)
	}
	ptr := uint32(ptrs[0])
	if !e.memory.Write(ptr, content) {
		return nil, fmt.Errorf("wasm input write of %d bytes failed", len(content))
	}
	results, err := fn.Call(context.Background(), uint64(len(content)))
	if err != nil {
		return nil, fmt.Errorf("wasm parse call: %w", err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("wasm parse returned no pointer")
	}
	raw, err := e.readString(uint32(results[0]))
	if err != nil {
		return nil, err
	}
	var parsed wasmParseResult
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("wasm parse JSON: %w", err)
	}
	return &parsed, nil
}

// HealthCheck calls the module's optional health_check export. A module that
// does not export the function is treated as SERVING so older modules keep
// working without recompilation. A non-"serving" token maps to NOT_SERVING
// so the host's periodic monitor can react to a degraded module.
func (e *wazeroEngine) HealthCheck() (string, error) {
	fn := e.module.ExportedFunction("health_check")
	if fn == nil {
		return wasmHealthServing, nil
	}
	results, err := fn.Call(context.Background())
	if err != nil {
		return "", fmt.Errorf("wasm health_check call: %w", err)
	}
	if len(results) == 0 {
		return "", fmt.Errorf("wasm health_check returned no pointer")
	}
	raw, err := e.readString(uint32(results[0]))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.ToLower(raw)), nil
}

func (e *wazeroEngine) Close() error {
	return e.runtime.Close(context.Background())
}

// readFileAll is a tiny indirection over os.ReadFile kept local so the wasm
// runtime has no direct os import besides through this helper.
func readFileAll(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// wasmFacade is a gRPC server that fronts a wasmEngine, so a wasm plugin goes
// through the exact same lifecycle, health, identity, and audit chain as
// process/container plugins: the host dials it like any other plugin.
type wasmFacade struct {
	pluginpb.UnimplementedDocumentParserPluginServer
	engine        wasmEngine
	mu            sync.Mutex
	descriptor    *wasmDescriptor
	manifestID    string
	manifestVer   string
	startedAtUnix int64
}

func newWasmFacade(manifestID, manifestVersion string, engine wasmEngine) *wasmFacade {
	return &wasmFacade{
		engine:        engine,
		manifestID:    manifestID,
		manifestVer:   manifestVersion,
		startedAtUnix: 0,
	}
}

func (f *wasmFacade) Describe(ctx context.Context, _ *pluginpb.DocumentParserDescribeRequest) (*pluginpb.DocumentParserDescribeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.descriptor == nil {
		descriptor, err := f.engine.Describe()
		if err != nil {
			return nil, fmt.Errorf("wasm describe: %w", err)
		}
		f.descriptor = descriptor
	}
	return &pluginpb.DocumentParserDescribeResponse{
		EngineName:   f.descriptor.EngineName,
		Description:  f.descriptor.Description,
		FileTypes:    f.descriptor.FileTypes,
		Capabilities: f.descriptor.Capabilities,
	}, nil
}

func (f *wasmFacade) Parse(ctx context.Context, request *pluginpb.DocumentParserParseRequest) (*pluginpb.DocumentParserParseResponse, error) {
	result, err := f.engine.Parse(request.GetFileType(), request.GetFileName(), request.GetFileContent())
	if err != nil {
		return nil, fmt.Errorf("wasm parse: %w", err)
	}
	return &pluginpb.DocumentParserParseResponse{
		MarkdownContent: result.MarkdownContent,
		Metadata:        result.Metadata,
	}, nil
}

// startWasmEngine instantiates the module and serves its gRPC facade on the
// manifest-declared address. The returned startedPlugin carries the engine
// and server so Runtime.Stop tears them down like any other runtime.
func startWasmEngine(ctx context.Context, plugin Plugin) (*startedPlugin, error) {
	engine, err := newWasmEngine(ctx, filepath.Join(plugin.Directory, plugin.Manifest.Spec.Entrypoint.WasmModule))
	if err != nil {
		return nil, err
	}
	facade := newWasmFacade(plugin.Manifest.Metadata.ID, plugin.Manifest.Metadata.Version, engine)
	listener, err := net.Listen("tcp", plugin.Manifest.Spec.Entrypoint.GRPCAddress)
	if err != nil {
		_ = engine.Close()
		return nil, fmt.Errorf("wasm facade listen %s: %w", plugin.Manifest.Spec.Entrypoint.GRPCAddress, err)
	}
	server := grpc.NewServer()
	pluginpb.RegisterPluginLifecycleServer(server, &wasmLifecycle{manifestID: plugin.Manifest.Metadata.ID, manifestVersion: plugin.Manifest.Metadata.Version, engine: engine})
	pluginpb.RegisterDocumentParserPluginServer(server, facade)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	return &startedPlugin{
		wasmEngine: engine,
		wasmServer: server,
		wasmServe:  serveErr,
	}, nil
}

// stop closes the engine and stops the facade server.
func (s *startedPlugin) stopWasm() error {
	if s.wasmServer != nil {
		s.wasmServer.Stop()
	}
	if s.wasmEngine != nil {
		return s.wasmEngine.Close()
	}
	return nil
}

// wasmLifecycle answers the lifecycle RPCs for a wasm plugin from manifest
// data — the module itself only implements describe/parse/health_check.
// HealthCheck delegates to the module's exported health_check when present so
// a degraded module surfaces through the same periodic monitor as process
// plugins; modules without the export report SERVING for backward compat.
type wasmLifecycle struct {
	pluginpb.UnimplementedPluginLifecycleServer
	manifestID      string
	manifestVersion string
	engine          wasmEngine
}

func (l *wasmLifecycle) GetInfo(context.Context, *pluginpb.GetInfoRequest) (*pluginpb.PluginInfo, error) {
	return &pluginpb.PluginInfo{
		Id:             l.manifestID,
		Version:        l.manifestVersion,
		ExtensionTypes: []string{"document_parser"},
	}, nil
}

func (l *wasmLifecycle) HealthCheck(context.Context, *pluginpb.HealthCheckRequest) (*pluginpb.HealthCheckResponse, error) {
	if l.engine == nil {
		return &pluginpb.HealthCheckResponse{Status: pluginpb.HealthCheckResponse_STATUS_SERVING}, nil
	}
	token, err := l.engine.HealthCheck()
	if err != nil {
		return &pluginpb.HealthCheckResponse{Status: pluginpb.HealthCheckResponse_STATUS_NOT_SERVING}, nil
	}
	if token == wasmHealthServing {
		return &pluginpb.HealthCheckResponse{Status: pluginpb.HealthCheckResponse_STATUS_SERVING}, nil
	}
	return &pluginpb.HealthCheckResponse{Status: pluginpb.HealthCheckResponse_STATUS_NOT_SERVING}, nil
}

func (l *wasmLifecycle) ValidateConfig(context.Context, *pluginpb.ValidateConfigRequest) (*pluginpb.ValidateConfigResponse, error) {
	return &pluginpb.ValidateConfigResponse{Valid: true}, nil
}
