package plugin

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDockerNetworkDisabledContainerRejectsOutbound verifies that a Docker
// container started with --network none cannot reach an external endpoint.
// This test is skipped when Docker is not available.
func TestDockerNetworkDisabledContainerRejectsOutbound(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed; skipping real network-isolation test")
	}

	ctx := context.Background()
	containerName := "weknora-net-test-" + strings.ReplaceAll(t.Name(), "/", "-")

	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", containerName,
		"--network", "none",
		"alpine:3.19",
		"sh", "-c", "wget -q -O /dev/null http://1.1.1.1/ && echo CONNECTED || echo BLOCKED")
	output, err := cmd.CombinedOutput()

	_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()

	outputStr := string(output)
	if err != nil {
		require.Contains(t, outputStr, "BLOCKED",
			"expected BLOCKED in output, got: %s (err: %v)", outputStr, err)
	} else {
		require.Contains(t, outputStr, "BLOCKED",
			"network-isolated container must not reach external endpoints, got: %s", outputStr)
		require.NotContains(t, outputStr, "CONNECTED",
			"network-isolated container must not print CONNECTED")
	}
}

// TestDockerNetworkDisabledContainerRunArgs verifies the container run args
// include --network none when the manifest declares network disabled.
// This is a code-level test that does not require Docker.
func TestDockerNetworkDisabledContainerRunArgs(t *testing.T) {
	manifest := Manifest{
		APIVersion: APIVersionV1,
		Kind:       "Plugin",
		Metadata:   Metadata{ID: "net-test", Name: "Net Test", Version: "0.1.0"},
		Spec: Spec{
			ExtensionType:  ExtensionTypeDataSource,
			WeKnoraVersion: ">=0.1.0",
			Capabilities:   []string{"sync"},
			Entrypoint: Entrypoint{
				Type:                 "container",
				Image:                "alpine:3.19",
				GRPCAddress:          "unix:///tmp/plugin.sock",
				ContainerGRPCAddress: "unix:///run/plugin.sock",
			},
			Permissions: Permissions{
				Network: NetworkPermission{Enabled: false},
				Filesystem: FilesystemPermission{
					ReadOnly: []string{"${config.rootPath}"},
				},
			},
		},
	}
	require.NoError(t, manifest.Validate())

	rootDir := t.TempDir()
	_, args, err := containerRunArgs(Plugin{Manifest: manifest}, map[string]string{"rootPath": rootDir})
	require.NoError(t, err)

	require.Contains(t, args, "--network")
	idx := -1
	for i, a := range args {
		if a == "--network" {
			idx = i
			break
		}
	}
	require.GreaterOrEqual(t, idx, 0)
	require.Less(t, idx+1, len(args))
	require.Equal(t, "none", args[idx+1])
	require.Contains(t, args, "--cap-drop")
	require.Contains(t, args, "ALL")
	require.Contains(t, args, "--read-only")
}
