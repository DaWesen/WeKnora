package plugin

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

const testManifestYAML = `apiVersion: weknora.plugin/v1
kind: Plugin
metadata:
  id: com.example.signed
  name: Signed Example
  version: 0.1.0
spec:
  extensionType: document_parser
  weknoraVersion: ">=0.1.0"
  capabilities: []
  entrypoint:
    type: process
    command: ["./example"]
    grpcAddress: "127.0.0.1:50099"
  permissions:
    network:
      enabled: true
    filesystem: {}
  healthCheck:
    intervalSeconds: 30
    timeoutSeconds: 10
  restartPolicy:
    enabled: true
    maxAttempts: 3
    windowSeconds: 300
    backoffMillis: 5000
  configSchema:
    type: object
    properties: {}
`

func writeTestKey(t *testing.T, dir, keyID string, pub ed25519.PublicKey) string {
	t.Helper()
	path := filepath.Join(dir, keyID+".pub")
	if err := os.WriteFile(path, pub, 0o644); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func signTestManifest(t *testing.T, manifestYAML string, priv ed25519.PrivateKey, keyID string) []byte {
	t.Helper()
	manifest, err := ParseManifest([]byte(manifestYAML))
	if err != nil {
		t.Fatalf("ParseManifest before signing: %v", err)
	}
	// Insert a blank signature so the payload derivation sees the same
	// structure (signature field present) that the final signed bytes will
	// have. The sig value is irrelevant because signedPayload strips the
	// whole signature field before marshalling.
	manifest.Signature = &Signature{Algorithm: SignatureAlgorithm, KeyID: keyID, Sig: ""}
	blankBytes, err := yamlMarshal(manifest)
	if err != nil {
		t.Fatalf("marshal blank-signed manifest: %v", err)
	}
	sig, err := SignManifest(blankBytes, priv)
	if err != nil {
		t.Fatalf("SignManifest: %v", err)
	}
	sig.KeyID = keyID
	manifest.Signature = sig
	out, err := yamlMarshal(manifest)
	if err != nil {
		t.Fatalf("re-marshal signed manifest: %v", err)
	}
	return out
}

// yamlMarshal is a thin wrapper so the test file has a single yaml import.
func yamlMarshal(m *Manifest) ([]byte, error) {
	return yaml.Marshal(m)
}

func TestVerifySignature_NoTrustRoot_SkipsCheck(t *testing.T) {
	manifest, err := ParseManifest([]byte(testManifestYAML))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	// No ring configured → verification must pass even without a signature.
	if err := VerifySignature([]byte(testManifestYAML), manifest, nil); err != nil {
		t.Fatalf("expected skip with nil ring, got: %v", err)
	}
	emptyRing := &KeyRing{}
	if err := VerifySignature([]byte(testManifestYAML), manifest, emptyRing); err != nil {
		t.Fatalf("expected skip with empty ring, got: %v", err)
	}
}

func TestVerifySignature_TrustRootConfigured_UnsignedManifestRejected(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	writeTestKey(t, dir, "test-key", pub)
	ring, err := LoadKeyRing(dir)
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	manifest, err := ParseManifest([]byte(testManifestYAML))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	err = VerifySignature([]byte(testManifestYAML), manifest, ring)
	if err != ErrSignatureRequired {
		t.Fatalf("expected ErrSignatureRequired, got: %v", err)
	}
}

func TestVerifySignature_ValidSignaturePasses(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	writeTestKey(t, dir, "test-key", pub)
	ring, err := LoadKeyRing(dir)
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	signedBytes := signTestManifest(t, testManifestYAML, priv, "test-key")
	manifest, err := ParseManifest(signedBytes)
	if err != nil {
		t.Fatalf("ParseManifest signed: %v", err)
	}
	if err := VerifySignature(signedBytes, manifest, ring); err != nil {
		t.Fatalf("expected valid signature to pass, got: %v", err)
	}
}

func TestVerifySignature_TamperedManifestFails(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	writeTestKey(t, dir, "test-key", pub)
	ring, err := LoadKeyRing(dir)
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	signedBytes := signTestManifest(t, testManifestYAML, priv, "test-key")
	// Tamper with the id field after signing — the signature must no longer
	// match.
	tampered := signedManifestTampered(signedBytes)
	manifest, err := ParseManifest(tampered)
	if err != nil {
		t.Fatalf("ParseManifest tampered: %v", err)
	}
	err = VerifySignature(tampered, manifest, ring)
	if err != ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature, got: %v", err)
	}
}

func TestVerifySignature_UnknownKeyIDRejected(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	writeTestKey(t, dir, "real-key", pub)
	ring, err := LoadKeyRing(dir)
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	// Sign with the real key but claim a keyId the host doesn't know.
	signedBytes := signTestManifest(t, testManifestYAML, priv, "bogus-key")
	manifest, err := ParseManifest(signedBytes)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	err = VerifySignature(signedBytes, manifest, ring)
	if err == nil {
		t.Fatal("expected error for unknown keyId")
	}
}

func TestLoadKeyRoundTrip(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	dir := t.TempDir()
	writeTestKey(t, dir, "round-trip", pub)
	ring, err := LoadKeyRing(dir)
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	got, ok := ring.Lookup("round-trip")
	if !ok {
		t.Fatal("Lookup failed for known keyId")
	}
	if !equalBytes(got, pub) {
		t.Error("loaded public key does not match written key")
	}
}

func TestLoadKeyRingMissingDirIsEmpty(t *testing.T) {
	ring, err := LoadKeyRing(filepath.Join(t.TempDir(), "no-such-dir"))
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got: %v", err)
	}
	if !ring.Empty() {
		t.Fatal("missing dir must yield empty ring")
	}
}

func TestLoadKeyRingRejectsShortKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "short.pub"), []byte{1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyRing(dir); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestLoadKeyRingAcceptsBase64Key(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// base64 + trailing newline — the format produced by `base64` tooling.
	encoded := base64.StdEncoding.EncodeToString(pub) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "b64.pub"), []byte(encoded), 0o644); err != nil {
		t.Fatal(err)
	}
	ring, err := LoadKeyRing(dir)
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	got, ok := ring.Lookup("b64")
	if !ok {
		t.Fatal("Lookup failed for base64 key")
	}
	if !equalBytes(got, pub) {
		t.Error("base64-decoded key does not match original")
	}
}

func TestLoadKeyRingKeepsRawKeyWithTrailingSpaceByte(t *testing.T) {
	// Regression: a raw 32-byte key whose last byte is 0x20 must round-trip
	// unchanged — whitespace trimming on binary keys corrupts them.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub[len(pub)-1] = 0x20
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spaced.pub"), pub, 0o644); err != nil {
		t.Fatal(err)
	}
	ring, err := LoadKeyRing(dir)
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	got, ok := ring.Lookup("spaced")
	if !ok {
		t.Fatal("Lookup failed")
	}
	if !equalBytes(got, pub) {
		t.Error("raw key with trailing 0x20 byte was corrupted")
	}
}

// equalBytes is a test-only helper to avoid importing bytes.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// signedManifestTampered changes the metadata.id in a signed manifest so the
// signature no longer matches. It parses the YAML, mutates the id, and
// re-marshals — keeping the signature block intact.
func signedManifestTampered(signedBytes []byte) []byte {
	manifest, err := ParseManifest(signedBytes)
	if err != nil {
		panic(err)
	}
	sig := manifest.Signature
	manifest, err = ParseManifest([]byte(testManifestYAML))
	if err != nil {
		panic(err)
	}
	manifest.Metadata.ID = "com.example.tampered"
	manifest.Signature = sig
	out, err := yamlMarshal(manifest)
	if err != nil {
		panic(err)
	}
	return out
}
