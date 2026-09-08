// Command wasm-parser-plugin is a WebAssembly document parser plugin for
// WeKnora. It is built with TinyGo and instantiated by the host's embedded
// wazero runtime (entrypoint.type: wasm) — no separate process or Docker
// container is involved.
//
// The module exports the minimal ABI defined in internal/plugin/wasm_runtime.go:
//
//	describe()      -> ptr  NUL-terminated JSON describing the engine
//	input_buffer()  -> ptr  address of a writable buffer the host fills
//	                        with input bytes before calling parse
//	parse(len)      -> ptr  parses len bytes from input_buffer; returns
//	                        NUL-terminated JSON with markdown + metadata
//	health_check()  -> ptr  NUL-terminated status token ("serving")
//
// The linear memory is exported automatically by TinyGo's wasm target as
// "memory". The host reads returned strings from that memory and never frees
// them — the module keeps a single static output buffer so addresses stay
// stable across calls.
//
// Build:
//
//	tinygo build -target=wasi -no-debug -o parser.wasm main.go
//
// The module is deliberately small (CRLF/CR normalization only) to illustrate
// the wasm plugin shape — compare examples/document-parser-plugin for the
// full-featured process-mode parser.
package main

import (
	"strings"
	"unsafe"
)

const (
	inputBufferSize  = 1 << 20 // 1 MiB — matches the host's chunked upload ceiling
	outputBufferSize = 1 << 21 // 2 MiB — room for the parsed JSON + metadata
)

// inputBuffer is the host-writable scratch space. The host calls
// input_buffer() to learn its address, writes up to inputBufferSize bytes,
// then calls parse(len) with the byte count.
var inputBuffer [inputBufferSize]byte

// outputBuffer holds the NUL-terminated JSON returned by describe / parse /
// health_check. A single static buffer keeps addresses stable across calls
// so the host can read the result without coordinating on memory ownership.
var outputBuffer [outputBufferSize]byte

// describe is exported so the host can fetch engine metadata without
// instantiating a separate gRPC lifecycle. The returned pointer references
// a NUL-terminated JSON document inside outputBuffer.
//
//export describe
func describe() uint32 {
	const desc = `{"engine_name":"wasm-markdown","description":"TinyGo markdown normalizer (CRLF/CR -> LF). Embedded wasm runtime — no process, no container, no network.","file_types":["md","markdown","txt","text"],"capabilities":[]}`
	return writeOutput(desc + "\x00")
}

// input_buffer returns the address of the writable input scratch space.
// The host writes input bytes here before invoking parse.
//
//export input_buffer
func input_buffer() uint32 {
	return uint32(uintptr(unsafe.Pointer(&inputBuffer[0])))
}

// parse reads len bytes from inputBuffer, normalizes line endings, and
// returns a pointer to a NUL-terminated JSON document in outputBuffer
// containing the parsed markdown and metadata.
//
//export parse
func parse(length uint32) uint32 {
	if int(length) > inputBufferSize {
		return writeOutput(`{"markdown_content":"","metadata":{"error":"input exceeds buffer"}}` + "\x00")
	}
	content := string(inputBuffer[:length])
	normalized := normalizeLineEndings(content)

	// Build the JSON response manually to keep the module dependency-free.
	var b strings.Builder
	b.WriteString(`{"markdown_content":`)
	writeJSONString(&b, normalized)
	b.WriteString(`,"metadata":{"source_bytes":`)
	writeInt(&b, int64(len(content)))
	b.WriteString(`,"normalized_bytes":`)
	writeInt(&b, int64(len(normalized)))
	b.WriteString(`,"line_endings":"lf"}}`)
	return writeOutput(b.String() + "\x00")
}

// health_check is the optional health export. The host's lifecycle facade
// calls it on every periodic health check; returning anything other than
// "serving" surfaces as NOT_SERVING. Modules that omit the export are
// treated as SERVING by default.
//
//export health_check
func health_check() uint32 {
	return writeOutput("serving\x00")
}

// writeOutput copies s into outputBuffer and returns the buffer's address.
// It truncates rather than growing so the address is always valid.
func writeOutput(s string) uint32 {
	n := len(s)
	if n > outputBufferSize {
		n = outputBufferSize
	}
	copy(outputBuffer[:], s[:n])
	return uint32(uintptr(unsafe.Pointer(&outputBuffer[0])))
}

// normalizeLineEndings converts CRLF and lone CR to LF. It is the same
// normalization as examples/document-parser-plugin but implemented in a
// single pass to keep the wasm module small.
func normalizeLineEndings(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' {
			b.WriteByte('\n')
			if i+1 < len(s) && s[i+1] == '\n' {
				i++ // swallow the LF that follows CRLF
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// writeJSONString writes s as a JSON string (with surrounding quotes) into b,
// escaping the small set of characters JSON requires for safety on
// arbitrary document content.
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		case '\r':
			b.WriteString("\\r")
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

// writeInt writes a non-negative int64 in decimal to b.
func writeInt(b *strings.Builder, n int64) {
	if n < 0 {
		b.WriteByte('-')
		n = -n
	}
	if n == 0 {
		b.WriteByte('0')
		return
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	b.Write(digits[i:])
}

// main is required by TinyGo's wasm target but never invoked by the host —
// the module is driven entirely through exported functions.
func main() {}
