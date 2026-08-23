package plugin

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
