// Package server provides the small runtime surface required by Go-based
// external plugins. Host discovery, isolation, and restart policy remain in
// WeKnora's internal runtime.
package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	"google.golang.org/grpc"
)

const (
	AddressEnv     = "WEKNORA_PLUGIN_GRPC_ADDRESS"
	DefaultAddress = "127.0.0.1:50071"
)

type Metadata struct {
	ID             string
	Version        string
	ExtensionTypes []string
}

// Lifecycle supplies conventional lifecycle responses for plugins that only
// need to implement a datasource service.
type Lifecycle struct {
	pluginpb.UnimplementedPluginLifecycleServer
	Metadata         Metadata
	OnValidateConfig func(context.Context, map[string]string) []*pluginpb.FieldError
	OnShutdown       func(context.Context) error
}

func (s Lifecycle) GetInfo(context.Context, *pluginpb.GetInfoRequest) (*pluginpb.PluginInfo, error) {
	return &pluginpb.PluginInfo{
		Id:             s.Metadata.ID,
		Version:        s.Metadata.Version,
		ExtensionTypes: s.Metadata.ExtensionTypes,
	}, nil
}

func (Lifecycle) HealthCheck(context.Context, *pluginpb.HealthCheckRequest) (*pluginpb.HealthCheckResponse, error) {
	return &pluginpb.HealthCheckResponse{Status: pluginpb.HealthCheckResponse_STATUS_SERVING}, nil
}

// ValidateConfig invokes the optional plugin-specific validation hook after the
// host has applied the manifest schema. Returning field errors rejects startup.
func (s Lifecycle) ValidateConfig(ctx context.Context, request *pluginpb.ValidateConfigRequest) (*pluginpb.ValidateConfigResponse, error) {
	if s.OnValidateConfig == nil {
		return &pluginpb.ValidateConfigResponse{Valid: true}, nil
	}
	errors := s.OnValidateConfig(ctx, request.GetConfig())
	return &pluginpb.ValidateConfigResponse{Valid: len(errors) == 0, Errors: errors}, nil
}

// Shutdown invokes the optional cleanup hook before acknowledging the host.
// The hook must respect the RPC context deadline.
func (s Lifecycle) Shutdown(ctx context.Context, _ *pluginpb.ShutdownRequest) (*pluginpb.ShutdownResponse, error) {
	if s.OnShutdown != nil {
		if err := s.OnShutdown(ctx); err != nil {
			return nil, err
		}
	}
	return &pluginpb.ShutdownResponse{}, nil
}

func Address() string {
	if address := strings.TrimSpace(os.Getenv(AddressEnv)); address != "" {
		return address
	}
	return DefaultAddress
}

// Listen creates the listener indicated by a plugin runtime address. Unix
// sockets are deliberately unsupported on Windows, where the host runtime
// cannot use the same socket contract.
func Listen(address string) (net.Listener, error) {
	if strings.HasPrefix(address, "unix://") {
		if runtime.GOOS == "windows" {
			return nil, fmt.Errorf("unix plugin sockets are not supported on windows")
		}
		path := strings.TrimPrefix(address, "unix://")
		if path == "" {
			return nil, fmt.Errorf("unix plugin socket path is empty")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create plugin socket directory: %w", err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale plugin socket: %w", err)
		}
		return net.Listen("unix", path)
	}
	return net.Listen("tcp", address)
}

func New(lifecycle pluginpb.PluginLifecycleServer, datasource pluginpb.DataSourcePluginServer, options ...grpc.ServerOption) *grpc.Server {
	server := grpc.NewServer(options...)
	pluginpb.RegisterPluginLifecycleServer(server, lifecycle)
	pluginpb.RegisterDataSourcePluginServer(server, datasource)
	return server
}

type Options struct {
	Address         string
	ServerOptions   []grpc.ServerOption
	ShutdownTimeout time.Duration
}

// Serve starts a plugin gRPC server until it stops unexpectedly. Plugins that
// need signal-aware process shutdown should use ServeContext.
func Serve(lifecycle pluginpb.PluginLifecycleServer, datasource pluginpb.DataSourcePluginServer, options Options) error {
	return ServeContext(context.Background(), lifecycle, datasource, options)
}

// ServeContext stops accepting work when ctx is canceled, then gives in-flight
// RPCs a bounded interval to complete before force-stopping the gRPC server.
// This allows a plugin main function to connect OS signals to both the SDK
// server and its Lifecycle.OnShutdown cleanup hook.
func ServeContext(ctx context.Context, lifecycle pluginpb.PluginLifecycleServer, datasource pluginpb.DataSourcePluginServer, options Options) error {
	address := options.Address
	if address == "" {
		address = Address()
	}
	listener, err := Listen(address)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer removeUnixSocket(address)

	grpcServer := New(lifecycle, datasource, options.ServerOptions...)
	serveResult := make(chan error, 1)
	go func() { serveResult <- grpcServer.Serve(listener) }()

	select {
	case err := <-serveResult:
		if err == grpc.ErrServerStopped {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownErr := shutdownLifecycle(lifecycle, options.ShutdownTimeout)
		shutdownServer(grpcServer, options.ShutdownTimeout)
		err := <-serveResult
		if err != nil && err != grpc.ErrServerStopped {
			return err
		}
		return shutdownErr
	}
}

func removeUnixSocket(address string) {
	if !strings.HasPrefix(address, "unix://") {
		return
	}
	_ = os.Remove(strings.TrimPrefix(address, "unix://"))
}

func shutdownLifecycle(lifecycle pluginpb.PluginLifecycleServer, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := lifecycle.Shutdown(ctx, &pluginpb.ShutdownRequest{})
	return err
}

func shutdownServer(server *grpc.Server, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		server.Stop()
		<-done
	}
}
