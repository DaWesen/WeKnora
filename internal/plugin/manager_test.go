package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
      enabled: false
`), 0o600))

	manager := NewManager(root)
	require.NoError(t, manager.Discover())

	plugins := manager.List(ExtensionTypeDataSource)
	require.Len(t, plugins, 1)
	require.Equal(t, "com.example.local-files", plugins[0].Manifest.Metadata.ID)
	require.Equal(t, StatusDiscovered, plugins[0].Status)
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
