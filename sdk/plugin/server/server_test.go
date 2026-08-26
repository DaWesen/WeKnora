package server

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

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

func TestLifecycleValidateConfigInvokesHook(t *testing.T) {
	seen := map[string]string(nil)
	lifecycle := Lifecycle{OnValidateConfig: func(_ context.Context, config map[string]string) []*pluginpb.FieldError {
		seen = config
		return []*pluginpb.FieldError{{Field: "token", Message: "required"}}
	}}

	response, err := lifecycle.ValidateConfig(context.Background(), &pluginpb.ValidateConfigRequest{Config: map[string]string{"token": ""}})
	if err != nil || response.Valid || len(response.Errors) != 1 {
		t.Fatalf("unexpected validation response: %#v, err=%v", response, err)
	}
	if seen["token"] != "" || response.Errors[0].Field != "token" {
		t.Fatalf("unexpected validation hook result: seen=%#v errors=%#v", seen, response.Errors)
	}

	defaultResponse, err := (Lifecycle{}).ValidateConfig(context.Background(), &pluginpb.ValidateConfigRequest{})
	if err != nil || !defaultResponse.Valid {
		t.Fatalf("expected valid default response, got %#v err=%v", defaultResponse, err)
	}
}

func TestLifecycleShutdownInvokesHook(t *testing.T) {
	called := false
	lifecycle := Lifecycle{OnShutdown: func(context.Context) error {
		called = true
		return nil
	}}
	_, err := lifecycle.Shutdown(context.Background(), &pluginpb.ShutdownRequest{})
	if err != nil || !called {
		t.Fatalf("shutdown hook called=%t err=%v", called, err)
	}

	lifecycle.OnShutdown = func(context.Context) error { return errors.New("cleanup failed") }
	_, err = lifecycle.Shutdown(context.Background(), &pluginpb.ShutdownRequest{})
	if err == nil || err.Error() != "cleanup failed" {
		t.Fatalf("expected hook error, got %v", err)
	}
}

func TestShutdownServerStopsServer(t *testing.T) {
	grpcServer := grpc.NewServer()
	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(started)
		grpcServer.GracefulStop()
		close(finished)
	}()
	<-started

	shutdownServer(grpcServer, 50*time.Millisecond)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServeContextStopsOnCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stopped atomic.Bool
	lifecycle := Lifecycle{OnShutdown: func(context.Context) error {
		stopped.Store(true)
		return nil
	}}
	implementation := &testDataSourceServer{}
	done := make(chan error, 1)
	go func() {
		done <- ServeContext(ctx, lifecycle, Options{Address: address, ShutdownTimeout: time.Second}, DataSourceService(implementation))
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve context: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve context did not return")
	}
	if !stopped.Load() {
		t.Fatal("server context cancellation must invoke lifecycle Shutdown")
	}
}

type testDataSourceServer struct {
	pluginpb.UnimplementedDataSourcePluginServer
}

func TestShutdownLifecyclePropagatesHookError(t *testing.T) {
	lifecycle := Lifecycle{OnShutdown: func(context.Context) error {
		return errors.New("cleanup failed")
	}}
	if err := shutdownLifecycle(lifecycle, time.Second); err == nil || err.Error() != "cleanup failed" {
		t.Fatalf("expected cleanup error, got %v", err)
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

func TestRemoveUnixSocketIgnoresTCPAddresses(t *testing.T) {
	removeUnixSocket("127.0.0.1:50071")
}

func TestContextWithSignalsReturnsCancelableContext(t *testing.T) {
	ctx, stop := ContextWithSignals(context.Background())
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("signal context was not canceled")
	}
}

func TestAddressUsesEnvironment(t *testing.T) {
	t.Setenv(AddressEnv, "127.0.0.1:60071")
	if actual := Address(); actual != "127.0.0.1:60071" {
		t.Fatalf("expected configured address, got %q", actual)
	}
}
