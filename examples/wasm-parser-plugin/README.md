# Wasm Markdown Normalizer Plugin

A WebAssembly document parser plugin for WeKnora, built with TinyGo and
instantiated by the host's embedded [wazero](https://wazero.io) runtime.

This is the **third `entrypoint.type`** — alongside `process` and `container`
— for plugins where spinning up a separate process or Docker container is
disproportionate to the work being done (here: a 50-line line-ending
normalizer). The wasm module runs embedded in the host process, so it shares
the exact same lifecycle, identity verification, health monitoring, and audit
chain as the other two runtimes — but with no process spawn, no container
overhead, and no network surface by construction.

## When to choose wasm

| Plugin shape | Recommended entrypoint | Why |
| --- | --- | --- |
| Heavy compute, network I/O, model inference | `process` / `container` | Real isolation, full language runtime |
| Container-level isolation required (network caps, capabilities) | `container` | Docker `--cap-drop ALL`, `--network none`, resource limits |
| Pure data transformation, <1000 LOC, no I/O | `wasm` | Embedded, no spawn overhead, no network by default |

Wasm plugins currently support **stateless `document_parser` and `web_search`
extensions only**. Datasource filesystem I/O and model_provider long-lived
connections are out of scope for the first iteration.

## Build

Install [TinyGo](https://tinygo.org/getting-started/install/), then:

```bash
cd examples/wasm-parser-plugin
make build           # produces parser.wasm
make lint            # verify plugin.yaml is structurally valid
make test-contract   # host loads the wasm, asserts GetInfo/HealthCheck/Describe
```

`parser.wasm` is a build artifact — do not commit it.

## Install

Copy `parser.wasm` and `plugin.yaml` into a subdirectory of your plugin root:

```text
/var/lib/weknora/plugins/
└── wasm-parser/
    ├── plugin.yaml
    └── parser.wasm
```

```bash
export WEKNORA_PLUGIN_DIR=/var/lib/weknora/plugins
```

The host discovers the manifest, validates `entrypoint.type: wasm` + the
`wasmModule` path, instantiates the module with wazero, and serves its gRPC
facade on the declared `grpcAddress`. From there the lifecycle is identical
to a process plugin: `GetInfo` identity verification, periodic
`HealthCheck` (delegating to the module's `health_check` export when
present), and the usual audit events.

## ABI

The module exports four functions and relies on TinyGo's default linear
memory export (`memory`). All strings are NUL-terminated and live in the
module's memory; the host never frees them.

| Export | Signature | Returns |
| --- | --- | --- |
| `describe` | `() -> ptr` | NUL-terminated JSON: `{"engine_name","description","file_types","capabilities"}` |
| `input_buffer` | `() -> ptr` | Address of a 1 MiB writable buffer the host fills before calling `parse` |
| `parse` | `(len) -> ptr` | NUL-terminated JSON: `{"markdown_content","metadata"}` after normalizing `len` bytes from `input_buffer` |
| `health_check` | `() -> ptr` | NUL-terminated status token; `"serving"` means healthy, anything else maps to NOT_SERVING |

`health_check` is **optional**: modules that predate the export or do not
need a runtime liveness signal omit it, and the host's facade reports
`SERVING` on their behalf. This keeps older modules forward-compatible.

## Security boundary

- **No network**: wasm modules get no host networking. The manifest rejects
  `permissions.network.enabled: true` for `type: wasm` at validation time.
- **No filesystem**: modules have only their linear memory; the host does
  not bridge host function calls for file I/O in this iteration.
- **Serial execution**: wazero instances run single-threaded; this is
  appropriate for low-QPS parsing but not for concurrent request serving.

## Comparison with the process-mode parser

This module is intentionally minimal (CRLF/CR → LF only). For the full
markdown parser — front matter extraction, heading promotion, plain-text
segmentation, chunked streaming upload — see
[`examples/document-parser-plugin`](../document-parser-plugin), which is the
canonical `entrypoint.type: process` reference.
