package server

import (
	"context"
	"net"
	"testing"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

func TestLifecycleDefaultsExposeMetadata(t *testing.T) {
	lifecycle := Lifecycle{Metadata: Metadata{
		ID:             "com.example.files",
		Version:        "1.2.3",
		ExtensionTypes: []string{"datasource"},
	}}

	info, err := lifecycle.GetInfo(context.Background(), &pluginpb.GetInfoRequest{})
	if err != nil {
		t.Fatalf("get info: %v", err)
	}
	if info.Id != "com.example.files" || info.Version != "1.2.3" || len(info.ExtensionTypes) != 1 {
		t.Fatalf("unexpected plugin info: %#v", info)
	}

	health, err := lifecycle.HealthCheck(context.Background(), &pluginpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if health.Status != pluginpb.HealthCheckResponse_STATUS_SERVING {
		t.Fatalf("unexpected health status: %s", health.Status)
	}
}

func TestListenTCP(t *testing.T) {
	listener, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	if _, ok := listener.Addr().(*net.TCPAddr); !ok {
		t.Fatalf("expected TCP listener, got %T", listener.Addr())
	}
}

func TestAddressUsesEnvironment(t *testing.T) {
	t.Setenv(AddressEnv, "127.0.0.1:60071")
	if actual := Address(); actual != "127.0.0.1:60071" {
		t.Fatalf("expected configured address, got %q", actual)
	}
}
