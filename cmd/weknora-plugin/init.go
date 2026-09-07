package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// extensionTypes is the canonical list of WeKnora extension points, matching
// internal/plugin/manifest.go. Keeping a copy here avoids importing the host
// internal package from this scaffolding tool; lint and test-contract do the
// real validation against the host rules.
var extensionTypes = []string{
	"datasource",
	"document_parser",
	"web_search",
	"model_provider",
	"retriever",
}

func newInitCmd() *cobra.Command {
	var (
		extType  string
		id       string
		outDir   string
		module   string
		binName  string
		grpcAddr string
		sdkPath  string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold a new plugin project",
		Long: `Generate a runnable plugin skeleton for one of the five extension types.

The output directory contains plugin.yaml, main.go, go.mod, and README.md.
After generation run:

  cd <out-dir>
  go mod tidy
  go build
  weknora-plugin lint plugin.yaml
  weknora-plugin test-contract .

By default go.mod requires the published sdk/plugin module. Pass
--sdk-path <path-to-weknora-checkout> to point the replace directive at a
local WeKnora checkout during development.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			extType = strings.TrimSpace(extType)
			if !validExtensionType(extType) {
				return fmt.Errorf("--type must be one of %v", extensionTypes)
			}
			id = strings.TrimSpace(id)
			if id == "" {
				return fmt.Errorf("--id is required (e.g. com.example.my-plugin)")
			}
			if outDir == "" {
				outDir = "./" + basename(id)
			}
			if module == "" {
				module = "github.com/example/" + basename(id)
			}
			if binName == "" {
				binName = basename(id)
			}
			if grpcAddr == "" {
				// Default to a TCP port unique enough for local dev. Authors can
				// switch to a unix:// socket on Linux/macOS.
				grpcAddr = "127.0.0.1:50071"
			}
			if sdkPath != "" {
				abs, err := filepath.Abs(sdkPath)
				if err != nil {
					return fmt.Errorf("resolve --sdk-path: %w", err)
				}
				sdkDir := filepath.Join(abs, "sdk", "plugin")
				if _, err := os.Stat(filepath.Join(sdkDir, "go.mod")); err != nil {
					return fmt.Errorf("--sdk-path %s does not look like a WeKnora checkout (missing sdk/plugin/go.mod)", sdkPath)
				}
				sdkPath = sdkDir
			}
			return scaffold(scaffoldArgs{
				Type:     extType,
				ID:       id,
				OutDir:   outDir,
				Module:   module,
				BinName:  binName,
				GRPCAddr: grpcAddr,
				SDKPath:  sdkPath,
			})
		},
	}
	cmd.Flags().StringVar(&extType, "type", "", fmt.Sprintf("extension type: one of %v", extensionTypes))
	cmd.Flags().StringVar(&id, "id", "", "plugin ID, e.g. com.example.my-parser")
	cmd.Flags().StringVar(&outDir, "out", "", "output directory (default ./<basename-of-id>)")
	cmd.Flags().StringVar(&module, "module", "", "Go module path (default github.com/example/<basename>)")
	cmd.Flags().StringVar(&binName, "bin", "", "compiled binary name (default <basename-of-id>)")
	cmd.Flags().StringVar(&grpcAddr, "grpc-addr", "", "gRPC listen address (default 127.0.0.1:50071)")
	cmd.Flags().StringVar(&sdkPath, "sdk-path", "", "local WeKnora checkout; adds a replace directive pointing at its sdk/plugin")
	_ = cmd.MarkFlagRequired("type")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func validExtensionType(t string) bool {
	for _, v := range extensionTypes {
		if v == t {
			return true
		}
	}
	return false
}

func basename(id string) string {
	parts := strings.Split(id, ".")
	if len(parts) == 0 {
		return id
	}
	last := parts[len(parts)-1]
	if last == "" && len(parts) > 1 {
		last = parts[len(parts)-2]
	}
	// Normalise common separators to hyphen.
	last = strings.ReplaceAll(last, "_", "-")
	return last
}

type scaffoldArgs struct {
	Type     string
	ID       string
	OutDir   string
	Module   string
	BinName  string
	GRPCAddr string
	// SDKPath, when set, is an absolute path to a local WeKnora checkout's
	// sdk/plugin directory. The generated go.mod then carries a replace
	// directive pointing there instead of requiring the published module.
	SDKPath string
}

func scaffold(a scaffoldArgs) error {
	if err := os.MkdirAll(a.OutDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	files := map[string]string{
		"plugin.yaml": manifestTemplate(a),
		"main.go":     mainTemplate(a),
		"go.mod":      goModTemplate(a),
		"README.md":   readmeTemplate(a),
	}
	for name, content := range files {
		path := filepath.Join(a.OutDir, name)
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("refusing to overwrite existing %s", path)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	fmt.Fprintf(os.Stdout, "scaffolded %s plugin in %s\n", a.Type, a.OutDir)
	fmt.Fprintln(os.Stdout, "next:")
	fmt.Fprintf(os.Stdout, "  cd %s\n", a.OutDir)
	fmt.Fprintln(os.Stdout, "  go mod tidy")
	fmt.Fprintln(os.Stdout, "  go build")
	fmt.Fprintln(os.Stdout, "  weknora-plugin lint plugin.yaml")
	fmt.Fprintln(os.Stdout, "  weknora-plugin test-contract .")
	return nil
}
