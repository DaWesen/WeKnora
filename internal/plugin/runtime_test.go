package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestHealthCheckTimeout(t *testing.T) {
	plugin := Plugin{Manifest: Manifest{Spec: Spec{HealthCheck: &HealthCheck{TimeoutSeconds: 3}}}}
	require.Equal(t, defaultHealthCheckTimeout, healthCheckTimeout(Plugin{}))
	require.Equal(t, 3*time.Second, healthCheckTimeout(plugin))
}

func TestResolveReadOnlyPath(t *testing.T) {
	directory := t.TempDir()
	resolved, err := resolveReadOnlyPath("${config.rootPath}", map[string]string{"rootPath": directory})
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(resolved))

	_, err = resolveReadOnlyPath("${config.rootPath}", nil)
	require.ErrorContains(t, err, "rootPath")

	_, err = resolveReadOnlyPath("${config.rootPath}", map[string]string{"rootPath": "relative"})
	require.ErrorContains(t, err, "must resolve to an absolute path")

	_, err = resolveReadOnlyPath("relative", nil)
	require.ErrorContains(t, err, "must resolve to an absolute path")

	_, err = resolveReadOnlyPath(filepath.Join(directory, "missing"), nil)
	require.ErrorContains(t, err, "resolve filesystem permission")
}

func TestPluginContainerResourceLimits(t *testing.T) {
	require.Equal(t, "512m", pluginContainerMemoryLimit)
	require.Equal(t, "1", pluginContainerCPULimit)
	require.Equal(t, "128", pluginContainerPidsLimit)
}

func TestContainerRunArgsApplyIsolationAndReadOnlyMounts(t *testing.T) {
	socketDir := t.TempDir()
	rootPath := t.TempDir()
	plugin := Plugin{Manifest: Manifest{
		Metadata: Metadata{ID: "com.example.local-files"},
		Spec: Spec{
			Entrypoint: Entrypoint{
				Type:                 "container",
				Image:                "example/plugin:test",
				GRPCAddress:          "unix://" + filepath.Join(socketDir, "plugin.sock"),
				ContainerGRPCAddress: "unix:///run/weknora/plugin.sock",
				Command:              []string{"--serve"},
			},
			Permissions: Permissions{
				Network:    NetworkPermission{Enabled: false},
				Filesystem: FilesystemPermission{ReadOnly: []string{"${config.rootPath}"}},
			},
		},
	}}

	name, args, err := containerRunArgs(plugin, map[string]string{"rootPath": rootPath})
	require.NoError(t, err)
	resolvedRootPath, err := filepath.EvalSymlinks(rootPath)
	require.NoError(t, err)
	require.Equal(t, "weknora-plugin-com-example-local-files", name)
	require.Equal(t, []string{
		"run", "-d", "--rm", "--name", name,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--pids-limit", "128",
		"--memory", "512m",
		"--cpus", "1",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--network", "none",
		"--mount", "type=bind,src=" + socketDir + ",dst=/run/weknora",
		"--mount", "type=bind,src=" + resolvedRootPath + ",dst=" + resolvedRootPath + ",readonly",
		"-e", "WEKNORA_PLUGIN_GRPC_ADDRESS=unix:///run/weknora/plugin.sock",
		"example/plugin:test", "--serve",
	}, args)
}

func TestContainerRunArgsRejectInvalidReadOnlyPermission(t *testing.T) {
	plugin := Plugin{Manifest: Manifest{
		Metadata: Metadata{ID: "com.example.local-files"},
		Spec: Spec{
			Entrypoint: Entrypoint{
				Type:        "container",
				Image:       "example/plugin:test",
				GRPCAddress: "unix:///tmp/plugin.sock",
			},
			Permissions: Permissions{
				Network:    NetworkPermission{Enabled: false},
				Filesystem: FilesystemPermission{ReadOnly: []string{"${config.rootPath}"}},
			},
		},
	}}

	_, _, err := containerRunArgs(plugin, nil)
	require.ErrorContains(t, err, "rootPath")
}

func TestFilesystemPermissionKey(t *testing.T) {
	first, err := filesystemPermissionKey([]string{"${config.second}", "${config.first}"}, map[string]string{
		"first":  t.TempDir(),
		"second": t.TempDir(),
	})
	require.NoError(t, err)
	require.NotEmpty(t, first)

	_, err = filesystemPermissionKey([]string{"${config.rootPath}"}, nil)
	require.ErrorContains(t, err, "rootPath")
}

func TestSanitizeContainerName(t *testing.T) {
	require.Equal(t, "com-example-local-files", sanitizeContainerName("com.example_local/files"))
}

func TestContainerExitError(t *testing.T) {
	require.EqualError(
		t,
		containerExitError([]byte("137\n"), nil),
		"plugin container exited with status 137",
	)
	require.EqualError(
		t,
		containerExitError([]byte("daemon unavailable\n"), errors.New("exit status 1")),
		"wait for plugin container: exit status 1: daemon unavailable",
	)
}

func TestUnexpectedProcessExitNotifiesHandler(t *testing.T) {
	runtime := NewRuntime()
	type exitEvent struct {
		id  string
		err error
	}
	exited := make(chan exitEvent, 1)
	runtime.SetProcessExitHandler(func(id string, err error) {
		exited <- exitEvent{id: id, err: err}
	})
	started := &startedPlugin{}
	runtime.started["com.example.files"] = started

	runtime.handleProcessExit("com.example.files", started, errors.New("plugin crashed"))
	event := <-exited
	require.Equal(t, "com.example.files", event.id)
	require.EqualError(t, event.err, "plugin crashed")
	_, running := runtime.started["com.example.files"]
	require.False(t, running)
}

func TestStoppedOrReplacedProcessExitIsIgnored(t *testing.T) {
	runtime := NewRuntime()
	exited := make(chan struct{}, 1)
	runtime.SetProcessExitHandler(func(string, error) { exited <- struct{}{} })
	old := &startedPlugin{}
	runtime.started["com.example.files"] = &startedPlugin{}

	runtime.handleProcessExit("com.example.files", old, errors.New("old process exited"))
	select {
	case <-exited:
		t.Fatal("unexpected process exit notification")
	default:
	}
}

func TestManagerReceivesUnexpectedProcessExit(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["com.example.files"] = &Plugin{Status: StatusRunning}
	started := &startedPlugin{}
	manager.runtime.started["com.example.files"] = started

	manager.runtime.handleProcessExit("com.example.files", started, errors.New("plugin crashed"))
	plugin, ok := manager.Get("com.example.files")
	require.True(t, ok)
	require.Equal(t, StatusFailed, plugin.Status)
	require.Equal(t, "plugin crashed", plugin.LastError)

	events := manager.AuditEvents(AuditQuery{
		PluginID: "com.example.files",
		Action:   AuditActionPluginRuntimeFailed,
	})
	require.Len(t, events, 1)
	require.Equal(t, "plugin crashed", events[0].Message)
}

type recoveryTestServer struct {
	pluginpb.UnimplementedPluginLifecycleServer
}

func (s *recoveryTestServer) GetInfo(context.Context, *pluginpb.GetInfoRequest) (*pluginpb.PluginInfo, error) {
	return &pluginpb.PluginInfo{
		Id:             "com.example.recovery",
		Version:        "1.0.0",
		ExtensionTypes: []string{"datasource"},
	}, nil
}

func (s *recoveryTestServer) HealthCheck(context.Context, *pluginpb.HealthCheckRequest) (*pluginpb.HealthCheckResponse, error) {
	return &pluginpb.HealthCheckResponse{Status: pluginpb.HealthCheckResponse_STATUS_SERVING}, nil
}

func (s *recoveryTestServer) ValidateConfig(context.Context, *pluginpb.ValidateConfigRequest) (*pluginpb.ValidateConfigResponse, error) {
	return &pluginpb.ValidateConfigResponse{Valid: true}, nil
}

func TestHealthCheckFailureStopsRuntimeAndMarksPluginFailed(t *testing.T) {
	manager := NewManager(t.TempDir())
	plugin := Plugin{
		Status: StatusRunning,
		Manifest: Manifest{
			Metadata: Metadata{ID: "com.example.health"},
			Spec:     Spec{},
		},
	}
	manager.byID[plugin.Manifest.Metadata.ID] = &plugin
	manager.runtime.started[plugin.Manifest.Metadata.ID] = &startedPlugin{}
	manager.rememberRestartConfig(plugin.Manifest.Metadata.ID, map[string]string{"token": "secret"})

	manager.handleHealthCheckFailure(plugin.Manifest.Metadata.ID, errors.New("health endpoint unavailable"))

	current, ok := manager.Get(plugin.Manifest.Metadata.ID)
	require.True(t, ok)
	require.Equal(t, StatusFailed, current.Status)
	require.False(t, manager.runtime.IsStarted(plugin.Manifest.Metadata.ID))
	events := manager.AuditEvents(AuditQuery{PluginID: plugin.Manifest.Metadata.ID})
	require.Len(t, events, 2)
	require.Equal(t, AuditActionPluginHealthFailed, events[0].Action)
	require.Equal(t, AuditActionPluginRuntimeFailed, events[1].Action)
	require.NotContains(t, strings.Join([]string{events[0].Message, events[1].Message}, " "), "secret")
}

func TestUnexpectedProcessExitAutomaticallyRestartsPlugin(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	grpcServer := grpc.NewServer()
	pluginpb.RegisterPluginLifecycleServer(grpcServer, &recoveryTestServer{})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()

	manager := NewManager(t.TempDir())
	defer manager.runtime.Stop(context.Background(), "com.example.recovery")
	plugin := Plugin{
		Directory: t.TempDir(),
		Status:    StatusRunning,
		Manifest: Manifest{
			Metadata: Metadata{ID: "com.example.recovery", Version: "1.0.0"},
			Spec: Spec{
				ExtensionType: ExtensionTypeDataSource,
				Entrypoint: Entrypoint{
					Type:        "process",
					Command:     []string{os.Args[0], "-test.run=TestPluginRecoveryHelperProcess", "--"},
					GRPCAddress: listener.Addr().String(),
				},
				RestartPolicy: &RestartPolicy{Enabled: true, MaxAttempts: 1, WindowSeconds: 60, BackoffMillis: 1},
			},
		},
	}
	manager.byID[plugin.Manifest.Metadata.ID] = &plugin
	manager.rememberRestartConfig(plugin.Manifest.Metadata.ID, map[string]string{"token": "secret"})
	crashed := &startedPlugin{}
	manager.runtime.started[plugin.Manifest.Metadata.ID] = crashed

	manager.runtime.handleProcessExit(plugin.Manifest.Metadata.ID, crashed, errors.New("plugin crashed"))

	require.Eventually(t, func() bool {
		current, ok := manager.Get(plugin.Manifest.Metadata.ID)
		return ok && current.Status == StatusRunning && manager.runtime.IsStarted(plugin.Manifest.Metadata.ID)
	}, time.Second, 10*time.Millisecond)
	status, ok := manager.RestartStatus(plugin.Manifest.Metadata.ID)
	require.True(t, ok)
	require.Equal(t, 1, status.Attempts)
	require.Equal(t, 0, status.Remaining)

	require.Eventually(t, func() bool {
		events := manager.AuditEvents(AuditQuery{PluginID: plugin.Manifest.Metadata.ID})
		return len(events) == 3 && events[0].Action == AuditActionPluginRestarted
	}, time.Second, 10*time.Millisecond)
	events := manager.AuditEvents(AuditQuery{PluginID: plugin.Manifest.Metadata.ID})
	require.Equal(t, AuditActionPluginStarted, events[1].Action)
	require.Equal(t, AuditActionPluginRuntimeFailed, events[2].Action)
	require.NotContains(t, strings.Join([]string{events[0].Message, events[1].Message, events[2].Message}, " "), "secret")
	require.NoError(t, manager.Stop(context.Background(), plugin.Manifest.Metadata.ID))
}

func TestPluginRecoveryHelperProcess(t *testing.T) {
	if !strings.HasSuffix(os.Args[len(os.Args)-1], "--") {
		return
	}
	select {}
}
