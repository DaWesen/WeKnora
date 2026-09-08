# TinyGo toolchain is required to compile the wasm module.
# Install from https://tinygo.org/getting-started/install/ and ensure
# `tinygo` is on PATH before running `make`.
TINYGO ?= tinygo
TARGET ?= wasi

.PHONY: build clean lint test-contract

# Compile main.go to a wasm module. `-no-debug` strips DWARF metadata for a
# smaller binary; WASI is enabled so TinyGo's runtime init runs even though
# the module is driven by exported functions rather than WASI main.
build:
	$(TINYGO) build -target=$(TARGET) -no-debug -o parser.wasm main.go

# Verify the manifest is structurally valid using the host's plugin CLI.
# Requires the weknora-plugin binary (see cmd/weknora-plugin).
lint: build
	weknora-plugin lint plugin.yaml

# Run the contract checker against the compiled module — starts the host,
# loads the wasm plugin, and asserts GetInfo/HealthCheck/Describe agree
# with the manifest.
test-contract: build
	weknora-plugin test-contract plugin.yaml

clean:
	rm -f parser.wasm
