package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	pluginpkg "github.com/Tencent/WeKnora/internal/plugin"
)

func newLintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lint <manifest-path>",
		Short: "Validate a plugin.yaml against the host manifest rules",
		Long: `Parse and validate a plugin.yaml the same way the WeKnora host does at
startup. Reports the first validation error and exits non-zero on failure.

This command reuses the host's manifest validator (internal/plugin), so a
manifest that passes lint is guaranteed to be accepted by the host loader.
Semver range checks run against the host version compiled into this binary.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			manifest, err := pluginpkg.ParseManifest(data)
			if err != nil {
				fmt.Fprintf(os.Stderr, "lint: %s: %v\n", path, err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stdout, "ok: %s (%s, %s)\n", manifest.Metadata.ID, manifest.Spec.ExtensionType, manifest.Metadata.Version)
			return nil
		},
	}
	return cmd
}
