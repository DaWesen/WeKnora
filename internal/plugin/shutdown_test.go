package plugin

import (
	"context"
	"io/fs"
	"net"
	"sync/atomic"
	"testing"
	"time"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type shutdownTestServer struct {
	pluginpb.UnimplementedPluginLifecycleServer
	pluginpb.UnimplementedDataSourcePluginServer
	calls           atomic.Int32
	validateCalls   atomic.Int32
	validationError []*pluginpb.FieldError
}

func (s *shutdownTestServer) ValidateConfig(_ context.Context, _ *pluginpb.ValidateConfigRequest) (*pluginpb.ValidateConfigResponse, error) {
	s.validateCalls.Add(1)
	return &pluginpb.ValidateConfigResponse{
		Valid:  len(s.validationError) == 0,
		Errors: s.validationError,
	}, nil
}

func (s *shutdownTestServer) Shutdown(context.Context, *pluginpb.ShutdownRequest) (*pluginpb.ShutdownResponse, error) {
	s.calls.Add(1)
	return &pluginpb.ShutdownResponse{}, nil
}

func TestManagerStopRequestsGracefulShutdownBeforeRuntimeStop(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	implementation := &shutdownTestServer{}
	grpcServer := grpc.NewServer()
	pluginpb.RegisterPluginLifecycleServer(grpcServer, implementation)
	pluginpb.RegisterDataSourcePluginServer(grpcServer, implementation)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()

	manager := NewManager(t.TempDir())
	const id = "com.example.graceful-stop"
	manager.byID[id] = &Plugin{
		Status: StatusRunning,
		Manifest: Manifest{
			Metadata: Metadata{ID: id},
			Spec:     Spec{Entrypoint: Entrypoint{GRPCAddress: listener.Addr().String()}},
		},
	}
	manager.runtime.started[id] = &startedPlugin{}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, manager.Stop(ctx, id))
	require.EqualValues(t, 1, implementation.calls.Load())
	require.False(t, manager.runtime.IsStarted(id))

	plugin, ok := manager.Get(id)
	require.True(t, ok)
	require.Equal(t, StatusDisabled, plugin.Status)
}

func TestManagerValidateRuntimeConfig(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	implementation := &shutdownTestServer{validationError: []*pluginpb.FieldError{
		{Field: "token", Message: "must be configured"},
		{Field: "endpoint", Message: "invalid"},
	}}
	grpcServer := grpc.NewServer()
	pluginpb.RegisterPluginLifecycleServer(grpcServer, implementation)
	pluginpb.RegisterDataSourcePluginServer(grpcServer, implementation)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()

	manager := NewManager(t.TempDir())
	plugin := Plugin{Manifest: Manifest{Spec: Spec{Entrypoint: Entrypoint{GRPCAddress: listener.Addr().String()}}}}
	err = manager.validateRuntimeConfig(context.Background(), plugin, map[string]string{"token": "secret"})
	require.ErrorContains(t, err, "endpoint, token")
	require.EqualValues(t, 1, implementation.validateCalls.Load())
}

func TestManagerValidateRuntimeConfigAcceptsPluginResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	implementation := &shutdownTestServer{}
	grpcServer := grpc.NewServer()
	pluginpb.RegisterPluginLifecycleServer(grpcServer, implementation)
	pluginpb.RegisterDataSourcePluginServer(grpcServer, implementation)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()

	manager := NewManager(t.TempDir())
	plugin := Plugin{Manifest: Manifest{Spec: Spec{Entrypoint: Entrypoint{GRPCAddress: listener.Addr().String()}}}}
	require.NoError(t, manager.validateRuntimeConfig(context.Background(), plugin, map[string]string{"rootPath": "safe"}))
	require.EqualValues(t, 1, implementation.validateCalls.Load())
}

func TestManagerStopMissingPlugin(t *testing.T) {
	manager := NewManager(t.TempDir())
	require.ErrorIs(t, manager.Stop(context.Background(), "missing"), fs.ErrNotExist)
}
