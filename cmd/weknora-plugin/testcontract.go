package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginpkg "github.com/Tencent/WeKnora/internal/plugin"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

func newTestContractCmd() *cobra.Command {
	var (
		startTimeout time.Duration
		dialTimeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "test-contract <plugin-dir>",
		Short: "Boot the plugin and check runtime metadata against the manifest",
		Long: `Start the plugin process declared in <dir>/plugin.yaml, dial its gRPC
address, call PluginLifecycle.GetInfo plus the extension's Describe RPC (for
types that have one), and report drift between manifest and runtime.

Checks performed:
  - manifest passes host validation (same rules as lint)
  - GetInfo().id == manifest metadata.id
  - GetInfo().extensionTypes contains manifest spec.extensionType
  - Describe succeeds (datasource has no Describe RPC; its identity is
    carried entirely by GetInfo + the host connector registration)
  - for document_parser / web_search / model_provider / retriever:
    Describe capabilities are a subset of manifest capabilities, matching
    the host loader's consistency rule

Unix socket entrypoints are not supported on Windows — use a TCP
grpcAddress in the manifest when running this command on Windows.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			manifestPath := filepath.Join(dir, "plugin.yaml")
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				return fmt.Errorf("read manifest: %w", err)
			}
			manifest, err := pluginpkg.ParseManifest(data)
			if err != nil {
				return fmt.Errorf("parse manifest: %w", err)
			}
			if manifest.Spec.Entrypoint.Type != "process" {
				return fmt.Errorf("test-contract only supports entrypoint.type=process, got %q", manifest.Spec.Entrypoint.Type)
			}
			if strings.HasPrefix(manifest.Spec.Entrypoint.GRPCAddress, "unix://") && runtime.GOOS == "windows" {
				return fmt.Errorf("unix socket entrypoint %q not supported on windows — switch the manifest grpcAddress to a TCP port", manifest.Spec.Entrypoint.GRPCAddress)
			}
			return runContract(manifest, dir, startTimeout, dialTimeout)
		},
	}
	cmd.Flags().DurationVar(&startTimeout, "start-timeout", 10*time.Second, "max time to wait for the plugin to listen")
	cmd.Flags().DurationVar(&dialTimeout, "dial-timeout", 5*time.Second, "gRPC dial timeout")
	return cmd
}

// drift records a single manifest-vs-runtime mismatch.
type drift struct {
	field    string
	manifest string
	runtime  string
}

func runContract(manifest *pluginpkg.Manifest, dir string, startTimeout, dialTimeout time.Duration) error {
	binPath := manifest.Spec.Entrypoint.Command[0]
	if !filepath.IsAbs(binPath) {
		binPath = filepath.Join(dir, binPath)
	}
	grpcAddr := manifest.Spec.Entrypoint.GRPCAddress
	// The SDK reads WEKNORA_PLUGIN_GRPC_ADDRESS at boot; point it at the
	// manifest-declared address so the contract run exercises the real
	// entrypoint configuration.
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	proc := exec.CommandContext(ctx, binPath)
	proc.Dir = dir
	proc.Env = append(os.Environ(), "WEKNORA_PLUGIN_GRPC_ADDRESS="+grpcAddr)
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	if err := proc.Start(); err != nil {
		return fmt.Errorf("start plugin binary %s: %w", binPath, err)
	}
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()
	if err := waitForListen(ctx, grpcAddr); err != nil {
		return fmt.Errorf("plugin did not listen on %s within %s: %w", grpcAddr, startTimeout, err)
	}

	conn, err := grpc.Dial(grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithTimeout(dialTimeout))
	if err != nil {
		return fmt.Errorf("dial plugin gRPC: %w", err)
	}
	defer conn.Close()

	drifts, err := checkContract(conn, manifest)
	if err != nil {
		return err
	}
	if len(drifts) == 0 {
		fmt.Fprintln(os.Stdout, "contract: ok")
		return nil
	}
	fmt.Fprintln(os.Stderr, "contract: drift detected")
	for _, d := range drifts {
		fmt.Fprintf(os.Stderr, "  %s: manifest=%q runtime=%q\n", d.field, d.manifest, d.runtime)
	}
	os.Exit(1)
	return nil
}

// checkContract runs the lifecycle + describe probes and returns drifts.
func checkContract(conn *grpc.ClientConn, manifest *pluginpkg.Manifest) ([]drift, error) {
	var drifts []drift
	callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lifecycle := pluginpb.NewPluginLifecycleClient(conn)
	info, err := lifecycle.GetInfo(callCtx, &pluginpb.GetInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("GetInfo: %w", err)
	}
	if info.GetId() != manifest.Metadata.ID {
		drifts = append(drifts, drift{"metadata.id", manifest.Metadata.ID, info.GetId()})
	}
	if info.GetVersion() != manifest.Metadata.Version {
		drifts = append(drifts, drift{"metadata.version", manifest.Metadata.Version, info.GetVersion()})
	}
	if !containsString(info.GetExtensionTypes(), string(manifest.Spec.ExtensionType)) {
		drifts = append(drifts, drift{
			"spec.extensionType",
			string(manifest.Spec.ExtensionType),
			strings.Join(info.GetExtensionTypes(), ","),
		})
	}

	// Health: a freshly booted plugin must report SERVING.
	health, err := lifecycle.HealthCheck(callCtx, &pluginpb.HealthCheckRequest{})
	if err != nil {
		return nil, fmt.Errorf("HealthCheck: %w", err)
	}
	if health.GetStatus() != pluginpb.HealthCheckResponse_STATUS_SERVING {
		drifts = append(drifts, drift{"health", "STATUS_SERVING", health.GetStatus().String()})
	}

	manifestCaps := sortedCopy(manifest.Spec.Capabilities)

	switch manifest.Spec.ExtensionType {
	case pluginpkg.ExtensionTypeDocumentParser:
		c := pluginpb.NewDocumentParserPluginClient(conn)
		d, err := c.Describe(callCtx, &pluginpb.DocumentParserDescribeRequest{})
		if err != nil {
			return nil, fmt.Errorf("Describe: %w", err)
		}
		fmt.Fprintf(os.Stdout, "describe: engine=%s fileTypes=%v capabilities=%v\n", d.GetEngineName(), d.GetFileTypes(), d.GetCapabilities())
		drifts = append(drifts, capabilityDrifts("capabilities", manifestCaps, d.GetCapabilities())...)
	case pluginpkg.ExtensionTypeWebSearch:
		c := pluginpb.NewWebSearchPluginClient(conn)
		d, err := c.Describe(callCtx, &pluginpb.WebSearchDescribeRequest{})
		if err != nil {
			return nil, fmt.Errorf("Describe: %w", err)
		}
		fmt.Fprintf(os.Stdout, "describe: provider=%s display=%s capabilities=%v\n", d.GetProviderType(), d.GetDisplayName(), d.GetCapabilities())
		drifts = append(drifts, capabilityDrifts("capabilities", manifestCaps, d.GetCapabilities())...)
	case pluginpkg.ExtensionTypeModelProvider:
		c := pluginpb.NewModelProviderPluginClient(conn)
		d, err := c.Describe(callCtx, &pluginpb.ModelProviderDescribeRequest{})
		if err != nil {
			return nil, fmt.Errorf("Describe: %w", err)
		}
		fmt.Fprintf(os.Stdout, "describe: provider=%s display=%s modelTypes=%v capabilities=%v\n", d.GetProviderType(), d.GetDisplayName(), d.GetModelTypes(), d.GetCapabilities())
		drifts = append(drifts, capabilityDrifts("capabilities", manifestCaps, d.GetCapabilities())...)
	case pluginpkg.ExtensionTypeRetriever:
		c := pluginpb.NewRetrieverPluginClient(conn)
		d, err := c.Describe(callCtx, &pluginpb.RetrieverDescribeRequest{})
		if err != nil {
			return nil, fmt.Errorf("Describe: %w", err)
		}
		fmt.Fprintf(os.Stdout, "describe: engine=%s retrieverTypes=%v capabilities=%v\n", d.GetEngineType(), d.GetRetrieverTypes(), d.GetCapabilities())
		drifts = append(drifts, capabilityDrifts("capabilities", manifestCaps, d.GetCapabilities())...)
	case pluginpkg.ExtensionTypeDataSource:
		// DataSource has no Describe RPC; identity rides on GetInfo and the
		// host-side connector registration. Just prove the service answers.
		c := pluginpb.NewDataSourcePluginClient(conn)
		resp, err := c.ValidateCredentials(callCtx, &pluginpb.ValidateCredentialsRequest{})
		if err != nil {
			return nil, fmt.Errorf("ValidateCredentials: %w", err)
		}
		fmt.Fprintf(os.Stdout, "validateCredentials: valid=%v\n", resp.GetValid())
	}
	return drifts, nil
}

// capabilityDrifts flags Describe capabilities that are not declared in the
// manifest — the same rule the host loader enforces at registration time.
func capabilityDrifts(field string, manifestCaps, describeCaps []string) []drift {
	var out []drift
	for _, c := range describeCaps {
		if !containsString(manifestCaps, c) {
			out = append(out, drift{field + " (describe not declared in manifest)", strings.Join(manifestCaps, ","), c})
		}
	}
	return out
}

func containsString(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// waitForListen polls a TCP or unix address until it accepts a connection or
// ctx is done. The plugin process typically needs a few hundred milliseconds
// to boot the gRPC server.
func waitForListen(ctx context.Context, addr string) error {
	if strings.HasPrefix(addr, "unix://") {
		path := strings.TrimPrefix(addr, "unix://")
		return pollListen(ctx, func() (net.Conn, error) { return dialUnix(path) })
	}
	return pollListen(ctx, func() (net.Conn, error) { return dialTCP(addr) })
}

func pollListen(ctx context.Context, dial func() (net.Conn, error)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := dial()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}
