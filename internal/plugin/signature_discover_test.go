package plugin

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// discoverFixture sets up a plugin root with one plugin.yaml, a trust-root
// directory, and a Manager wired with the key ring.
type discoverFixture struct {
	root      string
	pluginDir string
	ring      *KeyRing
	manager   *Manager
	priv      ed25519.PrivateKey
}

func newDiscoverFixture(t *testing.T, signed bool) *discoverFixture {
	t.Helper()
	base := t.TempDir()
	f := &discoverFixture{
		root:      filepath.Join(base, "plugins"),
		pluginDir: filepath.Join(base, "plugins", "example"),
	}
	if err := os.MkdirAll(f.pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(f.pluginDir, ManifestFileName)
	if err := os.WriteFile(manifestPath, []byte(testManifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.priv = priv
	keysDir := filepath.Join(base, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keysDir, "release.pub"), pub, 0o644); err != nil {
		t.Fatal(err)
	}
	ring, err := LoadKeyRing(keysDir)
	if err != nil {
		t.Fatal(err)
	}
	f.ring = ring
	if signed {
		signedBytes := signTestManifest(t, testManifestYAML, priv, "release")
		if err := os.WriteFile(manifestPath, signedBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.manager = NewManager(f.root)
	f.manager.SetTrustRoot(ring)
	return f
}

func TestDiscoverRejectsUnsignedWithTrustRoot(t *testing.T) {
	f := newDiscoverFixture(t, false)
	if err := f.manager.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, ok := f.manager.Get("com.example.signed"); ok {
		t.Fatal("unsigned plugin must not be discovered when trust root is set")
	}
	events := f.manager.AuditEvents(AuditQuery{Action: AuditActionPluginSignatureInvalid})
	if len(events) == 0 {
		t.Fatal("expected plugin.signature_invalid audit event")
	}
	if events[0].PluginID != "com.example.signed" {
		t.Errorf("audit event pluginId = %q, want com.example.signed", events[0].PluginID)
	}
}

func TestDiscoverAcceptsSignedWithTrustRoot(t *testing.T) {
	f := newDiscoverFixture(t, true)
	if err := f.manager.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, ok := f.manager.Get("com.example.signed"); !ok {
		t.Fatal("signed plugin must be discovered")
	}
	events := f.manager.AuditEvents(AuditQuery{Action: AuditActionPluginSignatureInvalid})
	if len(events) != 0 {
		t.Fatalf("expected no signature audit events, got %d", len(events))
	}
}

func TestDiscoverTamperedManifestRejected(t *testing.T) {
	f := newDiscoverFixture(t, true)
	manifestPath := filepath.Join(f.pluginDir, ManifestFileName)
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	// Append a trailing comment after signing — content changes, signature
	// stays the same, verification must fail.
	tampered := append([]byte(nil), original...)
	tampered = append(tampered, "\n# tampered\n"...)
	if err := os.WriteFile(manifestPath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, ok := f.manager.Get("com.example.signed"); ok {
		t.Fatal("tampered plugin must not be discovered")
	}
	events := f.manager.AuditEvents(AuditQuery{Action: AuditActionPluginSignatureInvalid})
	if len(events) == 0 {
		t.Fatal("expected plugin.signature_invalid audit event for tampered manifest")
	}
}

func TestDiscoverWithoutTrustRootAcceptsUnsigned(t *testing.T) {
	f := newDiscoverFixture(t, false)
	// Override with an empty ring: no trust root configured.
	f.manager.SetTrustRoot(&KeyRing{})
	if err := f.manager.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, ok := f.manager.Get("com.example.signed"); !ok {
		t.Fatal("unsigned plugin must be discovered when no trust root is configured")
	}
}

func TestDiscoverBadSignatureSkipsPluginButContinues(t *testing.T) {
	// One bad plugin must not prevent another good plugin from loading.
	f := newDiscoverFixture(t, true)
	// Add a second, unsigned plugin directory.
	otherDir := filepath.Join(f.root, "other")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherYAML := strings.ReplaceAll(testManifestYAML, "com.example.signed", "com.example.other")
	if err := os.WriteFile(filepath.Join(otherDir, ManifestFileName), []byte(otherYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, ok := f.manager.Get("com.example.signed"); !ok {
		t.Fatal("valid signed plugin must still be discovered")
	}
	if _, ok := f.manager.Get("com.example.other"); ok {
		t.Fatal("unsigned plugin must be skipped")
	}
}
