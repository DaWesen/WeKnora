package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManagerDiscover(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "local-files")
	require.NoError(t, os.Mkdir(pluginDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pluginDir, ManifestFileName), []byte(`
apiVersion: weknora.plugin/v1
kind: Plugin
metadata:
  id: com.example.local-files
  name: Local Files
  version: 1.0.0
spec:
  extensionType: datasource
  weknoraVersion: ">=0.1.0"
  entrypoint:
    type: process
    command: ["./plugin"]
    grpcAddress: "127.0.0.1:50051"
  permissions:
    network:
      enabled: true
`), 0o600))

	manager := NewManager(root)
	require.NoError(t, manager.Discover())

	plugins := manager.List(ExtensionTypeDataSource)
	require.Len(t, plugins, 1)
	require.Equal(t, "com.example.local-files", plugins[0].Manifest.Metadata.ID)
	require.Equal(t, StatusDiscovered, plugins[0].Status)
}

func TestManifestValidateConfig(t *testing.T) {
	manifest := Manifest{Spec: Spec{ConfigSchema: map[string]any{
		"required": []any{"rootPath"},
		"properties": map[string]any{
			"rootPath": map[string]any{"type": "string"},
		},
	}}}

	require.ErrorContains(t, manifest.ValidateConfig(nil), "rootPath")
	require.NoError(t, manifest.ValidateConfig(map[string]string{"rootPath": t.TempDir()}))

	manifest.Spec.ConfigSchema["properties"] = map[string]any{
		"rootPath": map[string]any{"type": "object"},
	}
	require.ErrorContains(t, manifest.ValidateConfig(map[string]string{"rootPath": t.TempDir()}), "unsupported type")
}

func TestValidatePluginInfo(t *testing.T) {
	manifest := Manifest{
		Metadata: Metadata{ID: "com.example.local-files", Version: "1.0.0"},
		Spec:     Spec{ExtensionType: ExtensionTypeDataSource},
	}

	require.NoError(t, validatePluginInfo(manifest, "com.example.local-files", "1.0.0", []string{"datasource"}))
	require.ErrorContains(t, validatePluginInfo(manifest, "other", "1.0.0", []string{"datasource"}), "id mismatch")
	require.ErrorContains(t, validatePluginInfo(manifest, "com.example.local-files", "2.0.0", []string{"datasource"}), "version mismatch")
	require.ErrorContains(t, validatePluginInfo(manifest, "com.example.local-files", "1.0.0", []string{"retriever"}), "does not provide")
}

func TestManifestRejectsInvalidHealthCheck(t *testing.T) {
	manifest := Manifest{
		APIVersion: APIVersionV1,
		Kind:       "Plugin",
		Metadata:   Metadata{ID: "com.example.health", Name: "Health", Version: "1.0.0"},
		Spec: Spec{
			ExtensionType:  ExtensionTypeDataSource,
			WeKnoraVersion: ">=0.1.0",
			Entrypoint:     Entrypoint{Type: "process", Command: []string{"plugin"}, GRPCAddress: "127.0.0.1:50051"},
			Permissions:    Permissions{Network: NetworkPermission{Enabled: true}},
			HealthCheck:    &HealthCheck{IntervalSeconds: 2, TimeoutSeconds: 3},
		},
	}
	require.ErrorContains(t, manifest.Validate(), "must not exceed")

	manifest.Spec.HealthCheck = &HealthCheck{IntervalSeconds: 0, TimeoutSeconds: 1}
	require.ErrorContains(t, manifest.Validate(), "intervalSeconds")
	manifest.Spec.HealthCheck = &HealthCheck{IntervalSeconds: 2, TimeoutSeconds: 0}
	require.ErrorContains(t, manifest.Validate(), "timeoutSeconds")
}

func TestHealthCheckInterval(t *testing.T) {
	require.Zero(t, HealthCheckInterval(Plugin{}))
	plugin := Plugin{Manifest: Manifest{Spec: Spec{HealthCheck: &HealthCheck{IntervalSeconds: 3}}}}
	require.Equal(t, 3*time.Second, HealthCheckInterval(plugin))
}

func TestManagerRestartStatusPrunesExpiredAttempts(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["files"] = &Plugin{Manifest: Manifest{Metadata: Metadata{ID: "files"}, Spec: Spec{
		RestartPolicy: &RestartPolicy{Enabled: true, MaxAttempts: 3, WindowSeconds: 60, BackoffMillis: 100},
	}}}
	manager.restarts["files"] = &restartState{attempts: []time.Time{
		time.Now().UTC().Add(-2 * time.Hour),
		time.Now().UTC().Add(-time.Minute),
	}}

	status, ok := manager.RestartStatus("files")
	require.True(t, ok)
	require.Equal(t, 1, status.Attempts)
	require.Equal(t, 2, status.Remaining)
	require.False(t, status.Restarting)
	require.Len(t, manager.restarts["files"].attempts, 1)
}

func TestManagerRestartStatusWithoutPolicy(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["files"] = &Plugin{Manifest: Manifest{Metadata: Metadata{ID: "files"}}}
	status, ok := manager.RestartStatus("files")
	require.True(t, ok)
	require.False(t, status.Enabled)
	require.Equal(t, 0, status.Remaining)
	_, ok = manager.RestartStatus("missing")
	require.False(t, ok)
}

func TestStopAllMarksRunningPluginsDisabled(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["running"] = &Plugin{Status: StatusRunning}
	manager.byID["failed"] = &Plugin{Status: StatusFailed, LastError: "health check failed"}
	manager.byID["discovered"] = &Plugin{Status: StatusDiscovered}

	require.NoError(t, manager.StopAll(context.Background()))

	running, ok := manager.Get("running")
	require.True(t, ok)
	require.Equal(t, StatusDisabled, running.Status)
	require.Empty(t, running.LastError)
	failed, ok := manager.Get("failed")
	require.True(t, ok)
	require.Equal(t, StatusDisabled, failed.Status)
	discovered, ok := manager.Get("discovered")
	require.True(t, ok)
	require.Equal(t, StatusDiscovered, discovered.Status)
}

func TestParseManifestRejectsNetworkDisabledProcess(t *testing.T) {
	_, err := ParseManifest([]byte(`
apiVersion: weknora.plugin/v1
kind: Plugin
metadata:
  id: com.example.invalid
  name: Invalid
  version: 1.0.0
spec:
  extensionType: datasource
  weknoraVersion: ">=0.1.0"
  entrypoint:
    type: process
    command: ["./plugin"]
    grpcAddress: "127.0.0.1:50051"
  permissions:
    network:
      enabled: false
`))
	require.ErrorContains(t, err, "require a container runtime")
}

func TestParseManifestRejectsNetworkDisabledContainerTCPGRPC(t *testing.T) {
	_, err := ParseManifest([]byte(`
apiVersion: weknora.plugin/v1
kind: Plugin
metadata:
  id: com.example.invalid
  name: Invalid
  version: 1.0.0
spec:
  extensionType: datasource
  weknoraVersion: ">=0.1.0"
  entrypoint:
    type: container
    image: example/plugin:1.0.0
    grpcAddress: "127.0.0.1:50051"
  permissions:
    network:
      enabled: false
`))
	require.ErrorContains(t, err, "require unix:// gRPC addresses")
}

func TestParseManifestRejectsNetworkHostsWithoutNetworkPermission(t *testing.T) {
	_, err := ParseManifest([]byte(`
apiVersion: weknora.plugin/v1
kind: Plugin
metadata:
  id: com.example.invalid
  name: Invalid
  version: 1.0.0
spec:
  extensionType: datasource
  weknoraVersion: ">=0.1.0"
  entrypoint:
    type: process
    command: ["./plugin"]
    grpcAddress: "127.0.0.1:50051"
  permissions:
    network:
      enabled: false
      hosts: ["api.github.com"]
`))
	require.ErrorContains(t, err, "network hosts require network permission")
}
