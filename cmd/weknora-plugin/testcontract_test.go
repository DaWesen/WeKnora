package main

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginpkg "github.com/Tencent/WeKnora/internal/plugin"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
)

// fakeParser is a minimal in-process plugin used to exercise checkContract
// against a live gRPC server without spawning a real plugin binary.
type fakeParser struct {
	pluginsdk.Lifecycle
	pluginpb.UnimplementedDocumentParserPluginServer

	infoID     string
	infoTypes  []string
	engineName string
	caps       []string
}

func (f *fakeParser) Describe(context.Context, *pluginpb.DocumentParserDescribeRequest) (*pluginpb.DocumentParserDescribeResponse, error) {
	return &pluginpb.DocumentParserDescribeResponse{
		EngineName:   f.engineName,
		Description:  "fake",
		FileTypes:    []string{"md"},
		Capabilities: f.caps,
	}, nil
}

func startFakePlugin(t *testing.T, f *fakeParser) (*grpc.ClientConn, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := pluginsdk.New(f, pluginsdk.DocumentParserService(f))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.Dial(listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(), grpc.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("dial fake plugin: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, listener.Addr().String()
}

func testManifest(id string, caps []string) *pluginpkg.Manifest {
	return &pluginpkg.Manifest{
		APIVersion: pluginpkg.APIVersionV1,
		Kind:       "Plugin",
		Metadata:   pluginpkg.Metadata{ID: id, Name: "Test", Version: "0.1.0"},
		Spec: pluginpkg.Spec{
			ExtensionType:  pluginpkg.ExtensionTypeDocumentParser,
			WeKnoraVersion: ">=0.1.0",
			Capabilities:   caps,
		},
	}
}

func TestCheckContractNoDrift(t *testing.T) {
	f := &fakeParser{infoID: "com.example.ok", infoTypes: []string{"document_parser"}, engineName: "fake"}
	f.Metadata = pluginsdk.Metadata{ID: "com.example.ok", Version: "0.1.0", ExtensionTypes: []string{"document_parser"}}
	conn, _ := startFakePlugin(t, f)
	drifts, err := checkContract(conn, testManifest("com.example.ok", nil))
	if err != nil {
		t.Fatalf("checkContract: %v", err)
	}
	if len(drifts) != 0 {
		t.Fatalf("expected no drifts, got %+v", drifts)
	}
}

func TestCheckContractDetectsIDDrift(t *testing.T) {
	f := &fakeParser{infoID: "com.example.runtime", infoTypes: []string{"document_parser"}}
	f.Metadata = pluginsdk.Metadata{ID: "com.example.runtime", Version: "0.1.0", ExtensionTypes: []string{"document_parser"}}
	conn, _ := startFakePlugin(t, f)
	// Manifest claims a different ID than what the plugin reports.
	drifts, err := checkContract(conn, testManifest("com.example.manifest", nil))
	if err != nil {
		t.Fatalf("checkContract: %v", err)
	}
	found := false
	for _, d := range drifts {
		if d.field == "metadata.id" {
			found = true
			if d.manifest != "com.example.manifest" || d.runtime != "com.example.runtime" {
				t.Errorf("unexpected drift pair: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("expected metadata.id drift, got %+v", drifts)
	}
}

func TestCheckContractDetectsCapabilityDrift(t *testing.T) {
	f := &fakeParser{infoID: "com.example.caps", infoTypes: []string{"document_parser"}, caps: []string{"stream"}}
	f.Metadata = pluginsdk.Metadata{ID: "com.example.caps", Version: "0.1.0", ExtensionTypes: []string{"document_parser"}}
	conn, _ := startFakePlugin(t, f)
	// Manifest does not declare "stream" but Describe advertises it — the
	// same rule the host loader enforces at registration time.
	drifts, err := checkContract(conn, testManifest("com.example.caps", nil))
	if err != nil {
		t.Fatalf("checkContract: %v", err)
	}
	if len(drifts) == 0 {
		t.Fatal("expected capability drift, got none")
	}
}

func TestCapabilityDrifts(t *testing.T) {
	manifest := []string{"stream", "ocr"}
	describe := []string{"stream", "table"}
	drifts := capabilityDrifts("capabilities", manifest, describe)
	if len(drifts) != 1 || drifts[0].runtime != "table" {
		t.Fatalf("expected only 'table' drift, got %+v", drifts)
	}
	if len(capabilityDrifts("capabilities", manifest, manifest)) != 0 {
		t.Fatal("identical caps should not drift")
	}
}

func TestContainsString(t *testing.T) {
	if !containsString([]string{"a", "b"}, "b") {
		t.Error("expected true")
	}
	if containsString([]string{"a"}, "b") {
		t.Error("expected false")
	}
}
