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
	Metadata Metadata
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

func (Lifecycle) ValidateConfig(context.Context, *pluginpb.ValidateConfigRequest) (*pluginpb.ValidateConfigResponse, error) {
	return &pluginpb.ValidateConfigResponse{Valid: true}, nil
}

func (Lifecycle) Shutdown(context.Context, *pluginpb.ShutdownRequest) (*pluginpb.ShutdownResponse, error) {
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
	Address       string
	ServerOptions []grpc.ServerOption
}

func Serve(lifecycle pluginpb.PluginLifecycleServer, datasource pluginpb.DataSourcePluginServer, options Options) error {
	address := options.Address
	if address == "" {
		address = Address()
	}
	listener, err := Listen(address)
	if err != nil {
		return err
	}
	defer listener.Close()
	return New(lifecycle, datasource, options.ServerOptions...).Serve(listener)
}
