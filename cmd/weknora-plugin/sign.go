package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	pluginpkg "github.com/Tencent/WeKnora/internal/plugin"
)

func newSignCmd() *cobra.Command {
	var (
		keyFile   string
		keyID     string
		genKeyDir string
	)
	cmd := &cobra.Command{
		Use:   "sign <manifest-path>",
		Short: "Sign a plugin.yaml with an ed25519 private key",
		Long: `Sign a plugin manifest so it passes host signature verification
(WEKNORA_PLUGIN_TRUSTED_KEYS).

Two modes:

1. With --gen-key <dir>: generate a new key pair, write <keyId>.pub /
   <keyId>.key into <dir>, and sign the manifest with the private key. Use
   this once to bootstrap a trust root. Distribute the .pub file into the
   host's trusted keys directory; keep the .key file secret.

2. With --key <path>: sign using an existing 32-byte raw ed25519 private
   key file (hex not accepted — raw bytes only).

The signature is written back into the manifest's signature: block. Signing
is idempotent: an existing signature block is replaced.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			manifestPath := args[0]
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				return fmt.Errorf("read manifest: %w", err)
			}
			// Validate first — no point signing an invalid manifest.
			manifest, err := pluginpkg.ParseManifest(data)
			if err != nil {
				return fmt.Errorf("manifest fails validation: %w", err)
			}

			var priv ed25519.PrivateKey
			switch {
			case genKeyDir != "":
				if keyID == "" {
					return fmt.Errorf("--key-id is required with --gen-key")
				}
				pub, generated, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					return fmt.Errorf("generate key: %w", err)
				}
				if err := os.MkdirAll(genKeyDir, 0o700); err != nil {
					return fmt.Errorf("create key dir: %w", err)
				}
				pubPath := filepath.Join(genKeyDir, keyID+".pub")
				keyPath := filepath.Join(genKeyDir, keyID+".key")
				if err := os.WriteFile(pubPath, pub, 0o644); err != nil {
					return fmt.Errorf("write public key: %w", err)
				}
				if err := os.WriteFile(keyPath, generated, 0o600); err != nil {
					return fmt.Errorf("write private key: %w", err)
				}
				priv = generated
				fmt.Fprintf(os.Stdout, "generated key pair in %s: %s.pub (public) + %s.key (private, keep secret)\n", genKeyDir, keyID, keyID)
			case keyFile != "":
				raw, err := os.ReadFile(keyFile)
				if err != nil {
					return fmt.Errorf("read private key: %w", err)
				}
				if len(raw) != ed25519.PrivateKeySize {
					return fmt.Errorf("private key file %s: expected %d raw bytes, got %d", keyFile, ed25519.PrivateKeySize, len(raw))
				}
				priv = ed25519.PrivateKey(raw)
				if keyID == "" {
					return fmt.Errorf("--key-id is required with --key")
				}
			default:
				return fmt.Errorf("either --key <path> or --gen-key <dir> is required")
			}

			// Derive the signature over the canonical (re-marshalled,
			// signature-stripped) payload.
			manifest.Signature = &pluginpkg.Signature{
				Algorithm: pluginpkg.SignatureAlgorithm,
				KeyID:     keyID,
				Sig:       "",
			}
			blankBytes, err := yaml.Marshal(manifest)
			if err != nil {
				return fmt.Errorf("marshal manifest for signing: %w", err)
			}
			sig, err := pluginpkg.SignManifest(blankBytes, priv)
			if err != nil {
				return fmt.Errorf("sign manifest: %w", err)
			}
			sig.KeyID = keyID
			manifest.Signature = sig
			out, err := yaml.Marshal(manifest)
			if err != nil {
				return fmt.Errorf("marshal signed manifest: %w", err)
			}
			if err := os.WriteFile(manifestPath, out, 0o644); err != nil {
				return fmt.Errorf("write signed manifest: %w", err)
			}
			fmt.Fprintf(os.Stdout, "signed %s (keyId=%s)\n", manifestPath, keyID)
			return nil
		},
	}
	cmd.Flags().StringVar(&keyFile, "key", "", "path to a 32-byte raw ed25519 private key file")
	cmd.Flags().StringVar(&keyID, "key-id", "", "keyId the host will use to look up the public key (required)")
	cmd.Flags().StringVar(&genKeyDir, "gen-key", "", "generate a new key pair into this directory and sign with it")
	return cmd
}
