package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultHealthCheckTimeout  = 10 * time.Second
	pluginContainerMemoryLimit = "512m"
	pluginContainerCPULimit    = "1"
	pluginContainerPidsLimit   = "128"
)

// Runtime starts plugin processes and containers. It owns only processes it has
// launched, so external plugin endpoints are never terminated by Stop.
type Runtime struct {
	mu            sync.Mutex
	started       map[string]*startedPlugin
	onProcessExit func(string, error)
}

type startedPlugin struct {
	command                 *exec.Cmd
	containerName           string
	filesystemPermissionKey string
}

func NewRuntime() *Runtime {
	return &Runtime{started: make(map[string]*startedPlugin)}
}

// SetProcessExitHandler registers a notification for a hosted process that
// exits outside an explicit Runtime.Stop call.
func (r *Runtime) SetProcessExitHandler(handler func(string, error)) {
	r.mu.Lock()
	r.onProcessExit = handler
	r.mu.Unlock()
}

func (r *Runtime) Start(ctx context.Context, plugin Plugin, config map[string]string) error {
	filesystemPermissionKey, err := filesystemPermissionKey(plugin.Manifest.Spec.Permissions.Filesystem.ReadOnly, config)
	if err != nil {
		return fmt.Errorf("resolve plugin filesystem permission: %w", err)
	}

	r.mu.Lock()
	started, exists := r.started[plugin.Manifest.Metadata.ID]
	r.mu.Unlock()
	if exists {
		if started.filesystemPermissionKey == filesystemPermissionKey {
			return nil
		}
		if err := r.Stop(ctx, plugin.Manifest.Metadata.ID); err != nil {
			return err
		}
	}

	var startedPluginInstance *startedPlugin
	var startErr error
	switch plugin.Manifest.Spec.Entrypoint.Type {
	case "process":
		startedPluginInstance, startErr = startProcess(ctx, plugin)
	case "container":
		startedPluginInstance, startErr = startContainer(ctx, plugin, config)
	default:
		startErr = fmt.Errorf("unsupported plugin runtime %q", plugin.Manifest.Spec.Entrypoint.Type)
	}
	if startErr != nil {
		return startErr
	}

	startedPluginInstance.filesystemPermissionKey = filesystemPermissionKey
	r.mu.Lock()
	r.started[plugin.Manifest.Metadata.ID] = startedPluginInstance
	r.mu.Unlock()
	if startedPluginInstance.command != nil {
		go r.waitForProcess(plugin.Manifest.Metadata.ID, startedPluginInstance)
	}
	if startedPluginInstance.containerName != "" {
		go r.waitForContainer(plugin.Manifest.Metadata.ID, startedPluginInstance)
	}
	return nil
}

func (r *Runtime) waitForProcess(id string, started *startedPlugin) {
	r.handleProcessExit(id, started, started.command.Wait())
}

func (r *Runtime) waitForContainer(id string, started *startedPlugin) {
	output, err := exec.Command("docker", "wait", started.containerName).CombinedOutput()
	r.handleProcessExit(id, started, containerExitError(output, err))
}

func containerExitError(output []byte, err error) error {
	if err != nil {
		return fmt.Errorf("wait for plugin container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return fmt.Errorf("plugin container exited with status %s", strings.TrimSpace(string(output)))
}

func (r *Runtime) handleProcessExit(id string, started *startedPlugin, exitErr error) {
	r.mu.Lock()
	if r.started[id] != started {
		r.mu.Unlock()
		return
	}
	delete(r.started, id)
	handler := r.onProcessExit
	r.mu.Unlock()

	if handler == nil {
		return
	}
	if exitErr == nil {
		exitErr = fmt.Errorf("plugin process exited")
	}
	handler(id, exitErr)
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

func startContainer(ctx context.Context, plugin Plugin, config map[string]string) (*startedPlugin, error) {
	name, args, err := containerRunArgs(plugin, config)
	if err != nil {
		return nil, err
	}
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("start plugin container %q: %w: %s", plugin.Manifest.Metadata.ID, err, strings.TrimSpace(string(output)))
	}
	return &startedPlugin{containerName: name}, nil
}

// containerRunArgs builds the complete Docker invocation separately from command
// execution so its isolation contract remains unit-testable without a daemon.
func containerRunArgs(plugin Plugin, config map[string]string) (string, []string, error) {
	entrypoint := plugin.Manifest.Spec.Entrypoint
	name := "weknora-plugin-" + sanitizeContainerName(plugin.Manifest.Metadata.ID)
	args := []string{
		"run", "-d", "--rm", "--name", name,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--pids-limit", pluginContainerPidsLimit,
		"--memory", pluginContainerMemoryLimit,
		"--cpus", pluginContainerCPULimit,
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
	}
	if !plugin.Manifest.Spec.Permissions.Network.Enabled {
		if !strings.HasPrefix(entrypoint.GRPCAddress, "unix://") {
			return "", nil, fmt.Errorf("network-disabled container plugin requires a unix gRPC address")
		}
		socketDir := filepath.Dir(strings.TrimPrefix(entrypoint.GRPCAddress, "unix://"))
		if err := os.MkdirAll(socketDir, 0o755); err != nil {
			return "", nil, fmt.Errorf("create plugin socket directory: %w", err)
		}
		containerSocketDir := socketDir
		if entrypoint.ContainerGRPCAddress != "" {
			containerSocketDir = path.Dir(strings.TrimPrefix(entrypoint.ContainerGRPCAddress, "unix://"))
		}
		args = append(args, "--network", "none", "--mount", fmt.Sprintf("type=bind,src=%s,dst=%s", socketDir, containerSocketDir))
	}
	for _, path := range plugin.Manifest.Spec.Permissions.Filesystem.ReadOnly {
		resolvedPath, err := resolveReadOnlyPath(path, config)
		if err != nil {
			return "", nil, fmt.Errorf("resolve plugin filesystem permission: %w", err)
		}
		args = append(args, "--mount", fmt.Sprintf("type=bind,src=%s,dst=%s,readonly", resolvedPath, resolvedPath))
	}
	grpcAddress := entrypoint.GRPCAddress
	if entrypoint.ContainerGRPCAddress != "" {
		grpcAddress = entrypoint.ContainerGRPCAddress
	}
	args = append(args, "-e", "WEKNORA_PLUGIN_GRPC_ADDRESS="+grpcAddress, entrypoint.Image)
	args = append(args, entrypoint.Command...)
	return name, args, nil
}

func filesystemPermissionKey(permissions []string, config map[string]string) (string, error) {
	paths := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		path, err := resolveReadOnlyPath(permission, config)
		if err != nil {
			return "", err
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return strings.Join(paths, "\x00"), nil
}

func resolveReadOnlyPath(permission string, config map[string]string) (string, error) {
	path := permission
	if strings.HasPrefix(permission, "${config.") && strings.HasSuffix(permission, "}") {
		key := strings.TrimSuffix(strings.TrimPrefix(permission, "${config."), "}")
		path = strings.TrimSpace(config[key])
		if path == "" {
			return "", fmt.Errorf("required config value %q is empty", key)
		}
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("filesystem permission %q must resolve to an absolute path", permission)
	}
	resolvedPath, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve filesystem permission %q: %w", permission, err)
	}
	return resolvedPath, nil
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
