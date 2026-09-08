// Command weknora-plugin is the developer scaffolding CLI for external WeKnora
// plugins. It generates a runnable skeleton for any of the five extension
// types, lints a manifest against the host's validation rules, runs a
// contract test that boots the plugin and checks its Describe output against
// the manifest, and signs manifests with ed25519 keys for hosts that enforce
// signature verification.
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
//	weknora-plugin sign --gen-key ./keys --key-id my-key ./my-parser/plugin.yaml
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
  sign            Sign a plugin.yaml with an ed25519 key (host verifies when
                  WEKNORA_PLUGIN_TRUSTED_KEYS is configured)

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
	root.AddCommand(newSignCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
