package plugin

import (
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
	resolved, err := resolveReadOnlyPath("${config.rootPath}", map[string]string{"rootPath": t.TempDir()})
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(resolved))

	_, err = resolveReadOnlyPath("${config.rootPath}", nil)
	require.ErrorContains(t, err, "rootPath")

	_, err = resolveReadOnlyPath("${config.rootPath}", map[string]string{"rootPath": "relative"})
	require.ErrorContains(t, err, "absolute path")
}

func TestSanitizeContainerName(t *testing.T) {
	require.Equal(t, "com-example-local-files", sanitizeContainerName("com.example_local/files"))
}
