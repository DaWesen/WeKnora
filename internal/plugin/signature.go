package plugin

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SignatureAlgorithm is the only algorithm currently supported. Keeping the
// field in the manifest lets us add algorithms later without a schema break.
const SignatureAlgorithm = "ed25519"

// KeyRing maps keyId → ed25519 public key. A nil or empty KeyRing means the
// operator has not configured a trust root; in that case signature
// verification is skipped entirely (development mode).
type KeyRing struct {
	keys map[string]ed25519.PublicKey
}

// LoadKeyRing reads every *.pub file in dir as an ed25519 public key. The
// filename (without extension) is the keyId referenced by manifest
// signatures. A missing or empty dir yields an empty (but non-nil) KeyRing,
// meaning "no trust root configured".
//
// Two file formats are accepted:
//   - raw 32 bytes (exactly what `weknora-plugin sign --gen-key` writes);
//   - base64 (standard encoding) of the 32 bytes, optionally followed by a
//     single trailing newline — convenient for keys authored with `base64`.
//
// Note that a raw key is never trimmed: whitespace is significant binary
// data, so a raw file whose last byte happens to be 0x20 must survive a
// round-trip intact.
func LoadKeyRing(dir string) (*KeyRing, error) {
	ring := &KeyRing{keys: make(map[string]ed25519.PublicKey)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return ring, nil
		}
		return nil, fmt.Errorf("read trusted keys dir %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pub") {
			continue
		}
		keyID := strings.TrimSuffix(entry.Name(), ".pub")
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read trusted key %s: %w", entry.Name(), err)
		}
		key, err := parsePublicKeyFile(raw)
		if err != nil {
			return nil, fmt.Errorf("trusted key %s: %w", entry.Name(), err)
		}
		ring.keys[keyID] = key
	}
	return ring, nil
}

// parsePublicKeyFile accepts a raw 32-byte key or base64 of it with an
// optional trailing newline. Anything else is an error.
func parsePublicKeyFile(raw []byte) (ed25519.PublicKey, error) {
	if len(raw) == ed25519.PublicKeySize {
		return ed25519.PublicKey(raw), nil
	}
	// Exactly one trailing newline is tolerated for base64 files.
	content := raw
	if len(content) > 0 && content[len(content)-1] == '\n' {
		content = content[:len(content)-1]
	}
	if len(content) > 0 && content[len(content)-1] == '\r' {
		content = content[:len(content)-1]
	}
	decoded, err := base64.StdEncoding.DecodeString(string(content))
	if err != nil {
		return nil, fmt.Errorf("expected %d raw bytes or base64 thereof: %w", ed25519.PublicKeySize, err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("expected %d raw bytes (or base64 thereof), got %d bytes", ed25519.PublicKeySize, len(decoded))
	}
	return ed25519.PublicKey(decoded), nil
}

// Empty reports whether the ring holds any trusted key. An empty ring means
// "no trust root configured" — callers skip signature enforcement.
func (r *KeyRing) Empty() bool {
	return r == nil || len(r.keys) == 0
}

// Keys returns the keyIds known to this ring. Used by callers to report the
// trust root size at startup.
func (r *KeyRing) Keys() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.keys))
	for id := range r.keys {
		out = append(out, id)
	}
	return out
}

// Lookup returns the public key for keyID, or false if not trusted.
func (r *KeyRing) Lookup(keyID string) (ed25519.PublicKey, bool) {
	if r == nil {
		return nil, false
	}
	k, ok := r.keys[keyID]
	return k, ok
}

// ErrSignatureRequired is returned when a trust root is configured but the
// manifest carries no signature.
var ErrSignatureRequired = errors.New("plugin manifest signature required: trust root configured but manifest is unsigned")

// ErrUnknownKeyID is returned when the signature references a keyId not
// present in the configured trust root.
var ErrUnknownKeyID = errors.New("plugin manifest signature references an unknown keyId")

// ErrBadSignatureFormat is returned when the sig field is not valid base64
// or not 64 bytes long.
var ErrBadSignatureFormat = errors.New("plugin manifest signature is not valid base64 or wrong length")

// ErrInvalidSignature is returned when the ed25519 verification fails — the
// manifest content does not match the signature under the trusted key.
var ErrInvalidSignature = errors.New("plugin manifest signature verification failed")

// VerifySignature checks a manifest against the trust root. When the ring is
// empty, verification is skipped (development mode). manifestBytes is the raw
// on-disk YAML; it is needed because the signed payload is derived from the
// manifest content with the signature field cleared and re-marshalled for
// determinism.
func VerifySignature(manifestBytes []byte, manifest *Manifest, ring *KeyRing) error {
	if ring.Empty() {
		return nil
	}
	if manifest == nil || manifest.Signature == nil {
		return ErrSignatureRequired
	}
	if manifest.Signature.Algorithm != SignatureAlgorithm {
		return fmt.Errorf("unsupported signature algorithm %q: only %s is supported", manifest.Signature.Algorithm, SignatureAlgorithm)
	}
	pub, ok := ring.Lookup(manifest.Signature.KeyID)
	if !ok {
		return fmt.Errorf("%w: keyId %q", ErrUnknownKeyID, manifest.Signature.KeyID)
	}
	sig, err := base64.StdEncoding.DecodeString(manifest.Signature.Sig)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignatureFormat, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: expected %d bytes, got %d", ErrBadSignatureFormat, ed25519.SignatureSize, len(sig))
	}
	payload, err := signedPayload(manifestBytes)
	if err != nil {
		return fmt.Errorf("compute signature payload: %w", err)
	}
	if !ed25519.Verify(pub, payload, sig) {
		return ErrInvalidSignature
	}
	return nil
}

// SignManifest computes a signature over the manifest content using the
// provided private key. It is used by the weknora-plugin sign CLI; the host
// never signs, it only verifies.
func SignManifest(manifestBytes []byte, priv ed25519.PrivateKey) (*Signature, error) {
	payload, err := signedPayload(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("compute signature payload: %w", err)
	}
	sig := ed25519.Sign(priv, payload)
	return &Signature{
		Algorithm: SignatureAlgorithm,
		KeyID:     "", // caller fills in the keyId matching the published public key
		Sig:       base64.StdEncoding.EncodeToString(sig),
	}, nil
}

// signedPayload returns the bytes that are covered by the manifest signature.
// It parses the raw YAML, drops the signature field, and re-marshals so that
// both the signing tool and the verifying host derive an identical byte
// sequence regardless of the author's formatting choices.
func signedPayload(manifestBytes []byte) ([]byte, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(manifestBytes, &node); err != nil {
		return nil, fmt.Errorf("parse manifest for signing: %w", err)
	}
	if node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
		return nil, errors.New("manifest is not a YAML document")
	}
	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("manifest root is not a mapping")
	}
	// Remove the "signature" key from the root mapping.
	filtered := make([]*yaml.Node, 0, len(root.Content))
	for i := 0; i < len(root.Content); i += 2 {
		key := root.Content[i]
		if key.Kind == yaml.ScalarNode && key.Value == "signature" {
			// Skip both the key and its value.
			continue
		}
		filtered = append(filtered, key, root.Content[i+1])
	}
	root.Content = filtered
	out, err := yaml.Marshal(&node)
	if err != nil {
		return nil, fmt.Errorf("re-marshal manifest for signing: %w", err)
	}
	digest := sha256.Sum256(out)
	return digest[:], nil
}
