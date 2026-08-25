package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	require.Equal(t, "plugin runtime failed", plugin.LastError)

	events := manager.AuditEvents(AuditQuery{
		PluginID: "com.example.files",
		Action:   AuditActionPluginRuntimeFailed,
	})
	require.Len(t, events, 1)
	require.Equal(t, "plugin runtime failed", events[0].Message)
}

type recoveryTestServer struct {
	pluginpb.UnimplementedPluginLifecycleServer
}

type retryRecoveryTestServer struct {
	recoveryTestServer
	remainingHealthFailures int32
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

func (s *retryRecoveryTestServer) HealthCheck(ctx context.Context, request *pluginpb.HealthCheckRequest) (*pluginpb.HealthCheckResponse, error) {
	if atomic.AddInt32(&s.remainingHealthFailures, -1) >= 0 {
		return nil, status.Error(codes.Unavailable, "temporary health failure")
	}
	return s.recoveryTestServer.HealthCheck(ctx, request)
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

	manager.handleHealthCheckFailure(plugin.Manifest.Metadata.ID, nil, errors.New("health endpoint unavailable"))

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

func TestHealthMonitorRequiresConfiguredConsecutiveFailures(t *testing.T) {
	manager := NewManager(t.TempDir())
	id := "com.example.health"
	monitor := &healthMonitor{cancel: func() {}, done: make(chan struct{})}
	manager.healthMonitors[id] = monitor
	plugin := Plugin{Manifest: Manifest{Spec: Spec{HealthCheck: &HealthCheck{FailureThreshold: 2}}}}

	require.False(t, manager.recordHealthCheck(id, monitor, errors.New("first failure"), healthFailureThreshold(plugin)).failureThresholdReached)
	require.True(t, manager.recordHealthCheck(id, monitor, errors.New("second failure"), healthFailureThreshold(plugin)).failureThresholdReached)

	require.False(t, manager.recordHealthCheck(id, monitor, nil, healthFailureThreshold(plugin)).failureThresholdReached)
	require.False(t, manager.recordHealthCheck(id, monitor, errors.New("first failure after success"), healthFailureThreshold(plugin)).failureThresholdReached)
	require.True(t, manager.recordHealthCheck(id, monitor, errors.New("second failure after success"), healthFailureThreshold(plugin)).failureThresholdReached)
}

func TestHealthStatusReportsFailureProgress(t *testing.T) {
	manager := NewManager(t.TempDir())
	plugin := Plugin{
		Manifest: Manifest{
			Metadata: Metadata{ID: "com.example.health"},
			Spec:     Spec{HealthCheck: &HealthCheck{IntervalSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 3}},
		},
	}
	manager.byID[plugin.Manifest.Metadata.ID] = &plugin
	monitor := &healthMonitor{cancel: func() {}, done: make(chan struct{})}
	manager.healthMonitors[plugin.Manifest.Metadata.ID] = monitor

	require.False(t, manager.recordHealthCheck(plugin.Manifest.Metadata.ID, monitor, errors.New("health failed"), healthFailureThreshold(plugin)).failureThresholdReached)
	status, ok := manager.HealthStatus(plugin.Manifest.Metadata.ID)
	require.True(t, ok)
	require.True(t, status.Enabled)
	require.True(t, status.Monitoring)
	require.Equal(t, 3, status.FailureThreshold)
	require.Equal(t, 1, status.ConsecutiveFailures)
	require.False(t, status.LastCheckedAt.IsZero())
	require.False(t, status.LastFailureAt.IsZero())

	result := manager.recordHealthCheck(plugin.Manifest.Metadata.ID, monitor, nil, healthFailureThreshold(plugin))
	require.Equal(t, 1, result.recoveredFailures)
	manager.recordAudit(plugin.Manifest.Metadata.ID, AuditActionPluginHealthRecovered, "success", "", "plugin health check recovered", map[string]string{
		"previous_consecutive_failures": strconv.Itoa(result.recoveredFailures),
	})
	status, ok = manager.HealthStatus(plugin.Manifest.Metadata.ID)
	require.True(t, ok)
	require.Zero(t, status.ConsecutiveFailures)
	require.True(t, status.LastFailureAt.IsZero())
	events := manager.AuditEvents(AuditQuery{PluginID: plugin.Manifest.Metadata.ID, Action: AuditActionPluginHealthRecovered})
	require.Len(t, events, 1)
	require.Equal(t, "1", events[0].Details["previous_consecutive_failures"])
}

func TestStaleHealthMonitorCannotStopReplacementRuntime(t *testing.T) {
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
	current := &healthMonitor{cancel: func() {}, done: make(chan struct{})}
	stale := &healthMonitor{cancel: func() {}, done: make(chan struct{})}
	manager.healthMonitors[plugin.Manifest.Metadata.ID] = current

	manager.handleHealthCheckFailure(plugin.Manifest.Metadata.ID, stale, errors.New("stale health failure"))

	updated, ok := manager.Get(plugin.Manifest.Metadata.ID)
	require.True(t, ok)
	require.Equal(t, StatusRunning, updated.Status)
	require.True(t, manager.runtime.IsStarted(plugin.Manifest.Metadata.ID))
	require.Empty(t, manager.AuditEvents(AuditQuery{PluginID: plugin.Manifest.Metadata.ID}))
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

func TestAutomaticRecoveryRestartsAfterARecoveredPluginCrashesAgain(t *testing.T) {
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
		Status: StatusRunning,
		Manifest: Manifest{
			Metadata: Metadata{ID: "com.example.recovery", Version: "1.0.0"},
			Spec: Spec{
				ExtensionType: ExtensionTypeDataSource,
				Entrypoint: Entrypoint{
					Type:        "process",
					Command:     []string{os.Args[0], "-test.run=TestPluginRecoveryHelperProcess", "--"},
					GRPCAddress: listener.Addr().String(),
				},
				HealthCheck:   &HealthCheck{IntervalSeconds: 60, TimeoutSeconds: 1},
				RestartPolicy: &RestartPolicy{Enabled: true, MaxAttempts: 2, WindowSeconds: 60, BackoffMillis: 1},
			},
		},
	}
	manager.byID[plugin.Manifest.Metadata.ID] = &plugin
	manager.rememberRestartConfig(plugin.Manifest.Metadata.ID, map[string]string{"token": "secret"})

	firstCrash := &startedPlugin{}
	manager.runtime.started[plugin.Manifest.Metadata.ID] = firstCrash
	manager.runtime.handleProcessExit(plugin.Manifest.Metadata.ID, firstCrash, errors.New("first plugin crash"))
	require.Eventually(t, func() bool {
		current, ok := manager.Get(plugin.Manifest.Metadata.ID)
		return ok && current.Status == StatusRunning && manager.runtime.IsStarted(plugin.Manifest.Metadata.ID)
	}, time.Second, 10*time.Millisecond)

	manager.runtime.mu.Lock()
	recovered := manager.runtime.started[plugin.Manifest.Metadata.ID]
	manager.runtime.mu.Unlock()
	require.NotNil(t, recovered)
	require.NotNil(t, recovered.command)
	require.NotNil(t, recovered.command.Process)
	require.NoError(t, recovered.command.Process.Kill())

	require.Eventually(t, func() bool {
		current, ok := manager.Get(plugin.Manifest.Metadata.ID)
		status, exists := manager.RestartStatus(plugin.Manifest.Metadata.ID)
		return ok && exists && current.Status == StatusRunning && manager.runtime.IsStarted(plugin.Manifest.Metadata.ID) &&
			status.Attempts == 2 && status.Remaining == 0 && !status.Restarting
	}, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		events := manager.AuditEvents(AuditQuery{PluginID: plugin.Manifest.Metadata.ID, Action: AuditActionPluginRestarted})
		return len(events) == 2 && events[0].Details["attempt"] == "2" && events[1].Details["attempt"] == "1"
	}, time.Second, 10*time.Millisecond)
	health, ok := manager.HealthStatus(plugin.Manifest.Metadata.ID)
	require.True(t, ok)
	require.True(t, health.Monitoring)
	require.Zero(t, health.ConsecutiveFailures)
}

func TestAutomaticRecoveryRetriesUntilAStartSucceeds(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&pluginpb.PluginLifecycle_ServiceDesc, &retryRecoveryTestServer{remainingHealthFailures: 1})
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
				RestartPolicy: &RestartPolicy{Enabled: true, MaxAttempts: 2, WindowSeconds: 60, BackoffMillis: 1},
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
	require.Equal(t, 2, status.Attempts)
	require.Zero(t, status.Remaining)
	require.Eventually(t, func() bool {
		events := manager.AuditEvents(AuditQuery{PluginID: plugin.Manifest.Metadata.ID, Action: AuditActionPluginRestarted})
		return len(events) == 1
	}, time.Second, 10*time.Millisecond)
	events := manager.AuditEvents(AuditQuery{PluginID: plugin.Manifest.Metadata.ID, Action: AuditActionPluginRestarted})
	require.Equal(t, "2", events[0].Details["attempt"])
	require.NotContains(t, events[0].Message, "secret")
}

func TestAutomaticRecoveryStopsWhenRestartBudgetIsExhausted(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&pluginpb.PluginLifecycle_ServiceDesc, &retryRecoveryTestServer{remainingHealthFailures: 10})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()

	manager := NewManager(t.TempDir())
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
				RestartPolicy: &RestartPolicy{Enabled: true, MaxAttempts: 2, WindowSeconds: 60, BackoffMillis: 1},
			},
		},
	}
	manager.byID[plugin.Manifest.Metadata.ID] = &plugin
	manager.rememberRestartConfig(plugin.Manifest.Metadata.ID, map[string]string{"token": "secret"})
	crashed := &startedPlugin{}
	manager.runtime.started[plugin.Manifest.Metadata.ID] = crashed

	manager.runtime.handleProcessExit(plugin.Manifest.Metadata.ID, crashed, errors.New("plugin crashed"))

	require.Eventually(t, func() bool {
		status, ok := manager.RestartStatus(plugin.Manifest.Metadata.ID)
		return ok && status.Attempts == 2 && status.Remaining == 0 && !status.Restarting
	}, time.Second, 10*time.Millisecond)
	current, ok := manager.Get(plugin.Manifest.Metadata.ID)
	require.True(t, ok)
	require.Equal(t, StatusFailed, current.Status)
	require.False(t, manager.runtime.IsStarted(plugin.Manifest.Metadata.ID))
	events := manager.AuditEvents(AuditQuery{PluginID: plugin.Manifest.Metadata.ID, Action: AuditActionPluginRestartDenied})
	require.Len(t, events, 1)
	require.Equal(t, "restart budget exhausted", events[0].Message)
}

func TestStopCancelsAutomaticRecoveryDuringRetryBackoff(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&pluginpb.PluginLifecycle_ServiceDesc, &retryRecoveryTestServer{remainingHealthFailures: 1})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()

	manager := NewManager(t.TempDir())
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
				RestartPolicy: &RestartPolicy{Enabled: true, MaxAttempts: 3, WindowSeconds: 60, BackoffMillis: 500},
			},
		},
	}
	manager.byID[plugin.Manifest.Metadata.ID] = &plugin
	manager.rememberRestartConfig(plugin.Manifest.Metadata.ID, map[string]string{"token": "secret"})
	crashed := &startedPlugin{}
	manager.runtime.started[plugin.Manifest.Metadata.ID] = crashed

	manager.runtime.handleProcessExit(plugin.Manifest.Metadata.ID, crashed, errors.New("plugin crashed"))

	require.Eventually(t, func() bool {
		status, ok := manager.RestartStatus(plugin.Manifest.Metadata.ID)
		return ok && status.Attempts == 2 && status.Restarting
	}, 2*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, manager.Stop(ctx, plugin.Manifest.Metadata.ID))
	current, ok := manager.Get(plugin.Manifest.Metadata.ID)
	require.True(t, ok)
	require.Equal(t, StatusDisabled, current.Status)
	require.False(t, manager.runtime.IsStarted(plugin.Manifest.Metadata.ID))
	status, ok := manager.RestartStatus(plugin.Manifest.Metadata.ID)
	require.True(t, ok)
	require.Equal(t, 2, status.Attempts)
	require.False(t, status.Restarting)
}

func TestReplacementRecoveryCancelsPreviousRetryBackoff(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&pluginpb.PluginLifecycle_ServiceDesc, &retryRecoveryTestServer{remainingHealthFailures: 1})
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
				RestartPolicy: &RestartPolicy{Enabled: true, MaxAttempts: 3, WindowSeconds: 60, BackoffMillis: 500},
			},
		},
	}
	manager.byID[plugin.Manifest.Metadata.ID] = &plugin
	manager.rememberRestartConfig(plugin.Manifest.Metadata.ID, map[string]string{"token": "secret"})
	crashed := &startedPlugin{}
	manager.runtime.started[plugin.Manifest.Metadata.ID] = crashed

	manager.runtime.handleProcessExit(plugin.Manifest.Metadata.ID, crashed, errors.New("plugin crashed"))
	require.Eventually(t, func() bool {
		status, ok := manager.RestartStatus(plugin.Manifest.Metadata.ID)
		return ok && status.Attempts == 2 && status.Restarting
	}, 2*time.Second, 10*time.Millisecond)

	manager.scheduleAutomaticRecovery(plugin.Manifest.Metadata.ID)
	require.Eventually(t, func() bool {
		current, ok := manager.Get(plugin.Manifest.Metadata.ID)
		return ok && current.Status == StatusRunning && manager.runtime.IsStarted(plugin.Manifest.Metadata.ID)
	}, 2*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		status, ok := manager.RestartStatus(plugin.Manifest.Metadata.ID)
		if !ok || status.Attempts != 3 || status.Remaining != 0 || status.Restarting {
			return false
		}
		events := manager.AuditEvents(AuditQuery{PluginID: plugin.Manifest.Metadata.ID, Action: AuditActionPluginRestarted})
		return len(events) == 1 && events[0].Details["attempt"] == "3"
	}, time.Second, 10*time.Millisecond)
}

func TestStopAllCancelsMultipleAutomaticRecoveriesDuringRetryBackoff(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&pluginpb.PluginLifecycle_ServiceDesc, &retryRecoveryTestServer{remainingHealthFailures: 2})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()

	manager := NewManager(t.TempDir())
	plugins := []Plugin{
		{
			Directory: t.TempDir(),
			Status:    StatusRunning,
			Manifest: Manifest{
				Metadata: Metadata{ID: "com.example.recovery.first", Version: "1.0.0"},
				Spec: Spec{
					ExtensionType: ExtensionTypeDataSource,
					Entrypoint: Entrypoint{
						Type:        "process",
						Command:     []string{os.Args[0], "-test.run=TestPluginRecoveryHelperProcess", "--"},
						GRPCAddress: listener.Addr().String(),
					},
					RestartPolicy: &RestartPolicy{Enabled: true, MaxAttempts: 3, WindowSeconds: 60, BackoffMillis: 500},
				},
			},
		},
		{
			Directory: t.TempDir(),
			Status:    StatusRunning,
			Manifest: Manifest{
				Metadata: Metadata{ID: "com.example.recovery.second", Version: "1.0.0"},
				Spec: Spec{
					ExtensionType: ExtensionTypeDataSource,
					Entrypoint: Entrypoint{
						Type:        "process",
						Command:     []string{os.Args[0], "-test.run=TestPluginRecoveryHelperProcess", "--"},
						GRPCAddress: listener.Addr().String(),
					},
					RestartPolicy: &RestartPolicy{Enabled: true, MaxAttempts: 3, WindowSeconds: 60, BackoffMillis: 500},
				},
			},
		},
	}
	for index := range plugins {
		id := plugins[index].Manifest.Metadata.ID
		manager.byID[id] = &plugins[index]
		manager.rememberRestartConfig(id, map[string]string{"token": "secret"})
		crashed := &startedPlugin{}
		manager.runtime.started[id] = crashed
		manager.runtime.handleProcessExit(id, crashed, errors.New("plugin crashed"))
	}

	ids := []string{"com.example.recovery.first", "com.example.recovery.second"}
	require.Eventually(t, func() bool {
		for _, id := range ids {
			status, ok := manager.RestartStatus(id)
			if !ok || status.Attempts != 2 || !status.Restarting {
				return false
			}
		}
		return true
	}, 2*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, manager.StopAll(ctx))
	for _, id := range ids {
		current, ok := manager.Get(id)
		require.True(t, ok)
		require.Equal(t, StatusDisabled, current.Status)
		require.False(t, manager.runtime.IsStarted(id))
		status, ok := manager.RestartStatus(id)
		require.True(t, ok)
		require.Equal(t, 2, status.Attempts)
		require.False(t, status.Restarting)
		_, retained := manager.restartConfig(id)
		require.False(t, retained)
	}
}

func TestPluginRecoveryHelperProcess(t *testing.T) {
	if !strings.HasSuffix(os.Args[len(os.Args)-1], "--") {
		return
	}
	select {}
}
