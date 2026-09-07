// Command weknora-plugin is the developer scaffolding CLI for external WeKnora
// plugins. It generates a runnable skeleton for any of the five extension
// types, lints a manifest against the host's validation rules, and runs a
// contract test that boots the plugin and checks its Describe output against
// the manifest.
//
// Install:
//
//	go install github.com/Tencent/WeKnora/cmd/weknora-plugin@latest
//
// Usage:
//
//	weknora-plugin init --type document_parser --id com.example.my-parser --out ./my-parser
//	weknora-plugin lint ./my-parser/plugin.yaml
//	weknora-plugin test-contract ./my-parser
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const usage = `weknora-plugin — external plugin developer toolkit

Subcommands:
  init            Scaffold a new plugin project (manifest + main.go + go.mod + README)
  lint            Validate a plugin.yaml against the host manifest rules
  test-contract   Boot the plugin and check Describe against the manifest

Run 'weknora-plugin <subcommand> --help' for per-command flags.
`

func main() {
	root := &cobra.Command{
		Use:   "weknora-plugin",
		Short: "WeKnora external plugin developer toolkit",
		Long:  usage,
	}
	root.AddCommand(newInitCmd())
	root.AddCommand(newLintCmd())
	root.AddCommand(newTestContractCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
