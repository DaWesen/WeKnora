package plugin

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHealthCheckTimeout(t *testing.T) {
	plugin := Plugin{Manifest: Manifest{Spec: Spec{HealthCheck: &HealthCheck{TimeoutSeconds: 3}}}}
	require.Equal(t, defaultHealthCheckTimeout, healthCheckTimeout(Plugin{}))
	require.Equal(t, 3*time.Second, healthCheckTimeout(plugin))
}

func TestSanitizeContainerName(t *testing.T) {
	require.Equal(t, "com-example-local-files", sanitizeContainerName("com.example_local/files"))
}
