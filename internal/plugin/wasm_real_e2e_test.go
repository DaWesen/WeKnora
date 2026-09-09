package plugin

import (
	"context"
	"os"
	"testing"
)

// TestRealWasmE2E drives a real TinyGo .wasm module through the production
// wazero engine: instantiate, describe, parse (CRLF/CR normalization), health
// check. It is skipped when examples/wasm-parser-plugin/parser.wasm has not
// been compiled (tinygo build -target=wasi -no-debug -o parser.wasm main.go).
//
// This covers the wazero ABI that the fake-engine unit tests cannot: module
// instantiation must not invoke `_start` (TinyGo's wasi `_start` proc_exit(0)s
// and closes the module), and parse metadata must round-trip as string values.
func TestRealWasmE2E(t *testing.T) {
	wasmPath := "../../examples/wasm-parser-plugin/parser.wasm"
	if _, err := os.Stat(wasmPath); err != nil {
		t.Skipf("parser.wasm not built: %v", err)
	}
	ctx := context.Background()

	engine, err := newWazeroEngine(ctx, wasmPath)
	if err != nil {
		t.Fatalf("instantiate real wasm module: %v", err)
	}
	defer engine.Close()

	if engine.module.IsClosed() {
		t.Fatal("module closed right after instantiation: default config must not run _start")
	}

	desc, err := engine.Describe()
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.EngineName != "wasm-markdown" {
		t.Errorf("engine_name: got %q want wasm-markdown", desc.EngineName)
	}

	content := []byte("# Hello\r\n\r\nLine2\r\nLine3\r")
	result, err := engine.Parse("md", "test.md", content)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := "# Hello\n\nLine2\nLine3\n"
	if result.MarkdownContent != want {
		t.Errorf("normalized content: got %q want %q", result.MarkdownContent, want)
	}
	if result.Metadata["line_endings"] != "lf" {
		t.Errorf("metadata: %+v", result.Metadata)
	}

	token, err := engine.HealthCheck()
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if token != wasmHealthServing {
		t.Errorf("health token: got %q want serving", token)
	}
}
