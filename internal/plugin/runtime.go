package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultHealthCheckTimeout = 10 * time.Second

// Runtime starts plugin processes and containers. It owns only processes it has
// launched, so external plugin endpoints are never terminated by Stop.
type Runtime struct {
	mu      sync.Mutex
	started map[string]*startedPlugin
}

type startedPlugin struct {
	command       *exec.Cmd
	containerName string
}

func NewRuntime() *Runtime {
	return &Runtime{started: make(map[string]*startedPlugin)}
}

func (r *Runtime) Start(ctx context.Context, plugin Plugin) error {
	r.mu.Lock()
	if _, exists := r.started[plugin.Manifest.Metadata.ID]; exists {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	var started *startedPlugin
	var err error
	switch plugin.Manifest.Spec.Entrypoint.Type {
	case "process":
		started, err = startProcess(ctx, plugin)
	case "container":
		started, err = startContainer(ctx, plugin)
	default:
		err = fmt.Errorf("unsupported plugin runtime %q", plugin.Manifest.Spec.Entrypoint.Type)
	}
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.started[plugin.Manifest.Metadata.ID] = started
	r.mu.Unlock()
	return nil
}

func (r *Runtime) Stop(ctx context.Context, id string) error {
	r.mu.Lock()
	started, exists := r.started[id]
	if exists {
		delete(r.started, id)
	}
	r.mu.Unlock()
	if !exists {
		return nil
	}

	if started.command != nil && started.command.Process != nil {
		if err := started.command.Process.Kill(); err != nil && err.Error() != "os: process already finished" {
			return fmt.Errorf("stop plugin process %q: %w", id, err)
		}
	}
	if started.containerName != "" {
		if output, err := exec.CommandContext(ctx, "docker", "rm", "-f", started.containerName).CombinedOutput(); err != nil {
			return fmt.Errorf("stop plugin container %q: %w: %s", id, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func (r *Runtime) StopAll(ctx context.Context) error {
	r.mu.Lock()
	ids := make([]string, 0, len(r.started))
	for id := range r.started {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		if err := r.Stop(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func startProcess(ctx context.Context, plugin Plugin) (*startedPlugin, error) {
	entrypoint := plugin.Manifest.Spec.Entrypoint
	command := entrypoint.Command[0]
	if !filepath.IsAbs(command) {
		command = filepath.Join(plugin.Directory, command)
	}
	cmd := exec.CommandContext(ctx, command, entrypoint.Command[1:]...)
	cmd.Dir = plugin.Directory
	cmd.Env = append(os.Environ(), "WEKNORA_PLUGIN_GRPC_ADDRESS="+entrypoint.GRPCAddress)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start plugin process %q: %w", plugin.Manifest.Metadata.ID, err)
	}
	return &startedPlugin{command: cmd}, nil
}

func startContainer(ctx context.Context, plugin Plugin) (*startedPlugin, error) {
	entrypoint := plugin.Manifest.Spec.Entrypoint
	name := "weknora-plugin-" + sanitizeContainerName(plugin.Manifest.Metadata.ID)
	args := []string{"run", "-d", "--rm", "--name", name, "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m"}
	if !plugin.Manifest.Spec.Permissions.Network.Enabled {
		if !strings.HasPrefix(entrypoint.GRPCAddress, "unix://") {
			return nil, fmt.Errorf("network-disabled container plugin requires a unix gRPC address")
		}
		socketDir := filepath.Dir(strings.TrimPrefix(entrypoint.GRPCAddress, "unix://"))
		if err := os.MkdirAll(socketDir, 0o755); err != nil {
			return nil, fmt.Errorf("create plugin socket directory: %w", err)
		}
		containerSocketDir := socketDir
		if entrypoint.ContainerGRPCAddress != "" {
			containerSocketDir = filepath.Dir(strings.TrimPrefix(entrypoint.ContainerGRPCAddress, "unix://"))
		}
		args = append(args, "--network", "none", "--mount", fmt.Sprintf("type=bind,src=%s,dst=%s", socketDir, containerSocketDir))
	}
	for _, path := range plugin.Manifest.Spec.Permissions.Filesystem.ReadOnly {
		args = append(args, "--mount", fmt.Sprintf("type=bind,src=%s,dst=%s,readonly", path, path))
	}
	grpcAddress := entrypoint.GRPCAddress
	if entrypoint.ContainerGRPCAddress != "" {
		grpcAddress = entrypoint.ContainerGRPCAddress
	}
	args = append(args, "-e", "WEKNORA_PLUGIN_GRPC_ADDRESS="+grpcAddress, entrypoint.Image)
	args = append(args, entrypoint.Command...)
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("start plugin container %q: %w: %s", plugin.Manifest.Metadata.ID, err, strings.TrimSpace(string(output)))
	}
	return &startedPlugin{containerName: name}, nil
}

func sanitizeContainerName(id string) string {
	return strings.NewReplacer(".", "-", "_", "-", "/", "-").Replace(id)
}

func healthCheckTimeout(plugin Plugin) time.Duration {
	if plugin.Manifest.Spec.HealthCheck != nil && plugin.Manifest.Spec.HealthCheck.TimeoutSeconds > 0 {
		return time.Duration(plugin.Manifest.Spec.HealthCheck.TimeoutSeconds) * time.Second
	}
	return defaultHealthCheckTimeout
}
