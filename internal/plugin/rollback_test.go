package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rollbackFixture builds a discovered process plugin on disk with a manifest
// and a placeholder binary.
type rollbackFixture struct {
	root      string
	pluginDir string
	manager   *Manager
}

func newRollbackFixture(t *testing.T) *rollbackFixture {
	t.Helper()
	root := t.TempDir()
	pluginDir := filepath.Join(root, "example")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, ManifestFileName), []byte(testManifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "example"), []byte("fake binary v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(root)
	if err := manager.Discover(); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return &rollbackFixture{root: root, pluginDir: pluginDir, manager: manager}
}

func TestEnsureRollbackSnapshotCopiesFiles(t *testing.T) {
	f := newRollbackFixture(t)
	plugin, ok := f.manager.Get("com.example.signed")
	if !ok {
		t.Fatal("plugin not discovered")
	}

	// Before: no snapshot.
	if f.manager.HasRollbackSnapshot("com.example.signed") {
		t.Fatal("snapshot must not exist before first successful start")
	}

	f.manager.ensureRollbackSnapshot(*plugin)

	snapshotManifest := filepath.Join(f.pluginDir, rollbackDirName, ManifestFileName)
	data, err := os.ReadFile(snapshotManifest)
	if err != nil {
		t.Fatalf("snapshot manifest missing: %v", err)
	}
	if string(data) != testManifestYAML {
		t.Error("snapshot manifest content differs from on-disk manifest")
	}
	binData, err := os.ReadFile(filepath.Join(f.pluginDir, rollbackDirName, "example"))
	if err != nil {
		t.Fatalf("snapshot binary missing: %v", err)
	}
	if string(binData) != "fake binary v1" {
		t.Error("snapshot binary content wrong")
	}

	// Simulate an upgrade: replace manifest and binary on disk.
	upgraded := "apiVersion: weknora.plugin/v1\nkind: Plugin\nmetadata:\n  id: com.example.signed\n  name: Signed Example\n  version: 9.9.9\nspec:\n  extensionType: document_parser\n  weknoraVersion: \">=0.1.0\"\n  entrypoint:\n    type: process\n    command: [\"./example\"]\n    grpcAddress: \"127.0.0.1:59999\"\n  permissions:\n    network:\n      enabled: true\n  healthCheck:\n    intervalSeconds: 30\n    timeoutSeconds: 10\n"
	if err := os.WriteFile(filepath.Join(f.pluginDir, ManifestFileName), []byte(upgraded), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.pluginDir, "example"), []byte("fake binary v2"), 0o755); err != nil {
		t.Fatal(err)
	}

	// ensureRollbackSnapshot must NOT overwrite the v1 snapshot.
	pluginUpgraded, ok := f.manager.Get("com.example.signed")
	if !ok {
		t.Fatal("plugin lost after upgrade")
	}
	f.manager.ensureRollbackSnapshot(*pluginUpgraded)
	data, _ = os.ReadFile(snapshotManifest)
	if string(data) != testManifestYAML {
		t.Error("snapshot was overwritten by upgraded manifest — upgrades would lose their rollback target")
	}
	binData, _ = os.ReadFile(filepath.Join(f.pluginDir, rollbackDirName, "example"))
	if string(binData) != "fake binary v1" {
		t.Error("snapshot binary was overwritten by upgrade")
	}
}

func TestRollbackWithoutSnapshotFails(t *testing.T) {
	f := newRollbackFixture(t)
	err := f.manager.Rollback(context.Background(), "com.example.signed")
	if err != ErrNoRollbackSnapshot {
		t.Fatalf("expected ErrNoRollbackSnapshot, got %v", err)
	}
}

func TestRollbackWithoutPluginFails(t *testing.T) {
	f := newRollbackFixture(t)
	err := f.manager.Rollback(context.Background(), "no-such-plugin")
	if err == nil {
		t.Fatal("expected error for unknown plugin")
	}
}

func TestRollbackRestoresFilesAndAudits(t *testing.T) {
	f := newRollbackFixture(t)
	plugin, _ := f.manager.Get("com.example.signed")

	// Establish the v1 snapshot, then simulate an upgraded (broken) v2.
	f.manager.ensureRollbackSnapshot(*plugin)
	upgraded := "apiVersion: weknora.plugin/v1\nkind: Plugin\nmetadata:\n  id: com.example.signed\n  name: Signed Example\n  version: 9.9.9\nspec:\n  extensionType: document_parser\n  weknoraVersion: \">=0.1.0\"\n  entrypoint:\n    type: process\n    command: [\"./example\"]\n    grpcAddress: \"127.0.0.1:59999\"\n  permissions:\n    network:\n      enabled: true\n  healthCheck:\n    intervalSeconds: 30\n    timeoutSeconds: 10\n"
	if err := os.WriteFile(filepath.Join(f.pluginDir, ManifestFileName), []byte(upgraded), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.pluginDir, "example"), []byte("fake binary v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Discover(); err != nil {
		t.Fatal(err)
	}

	// Rollback: the fake binary cannot actually serve gRPC, so the restart
	// inside Rollback fails — but the files on disk must be restored to v1
	// and the failure must be audited.
	err := f.manager.Rollback(context.Background(), "com.example.signed")
	if err == nil {
		t.Fatal("expected start failure for fake binary")
	}

	manifestBytes, _ := os.ReadFile(filepath.Join(f.pluginDir, ManifestFileName))
	if string(manifestBytes) != testManifestYAML {
		t.Errorf("live manifest not restored: %q", string(manifestBytes))
	}
	binBytes, _ := os.ReadFile(filepath.Join(f.pluginDir, "example"))
	if string(binBytes) != "fake binary v1" {
		t.Errorf("live binary not restored: %q", string(binBytes))
	}

	// The restored plugin must be re-discovered as v0.1.0.
	restored, ok := f.manager.Get("com.example.signed")
	if !ok {
		t.Fatal("restored plugin missing from registry")
	}
	if restored.Manifest.Metadata.Version != "0.1.0" {
		t.Errorf("restored version = %s, want 0.1.0", restored.Manifest.Metadata.Version)
	}

	// Audit trail: rollback was attempted (failed because binary is fake).
	events := f.manager.AuditEvents(AuditQuery{Action: AuditActionPluginRollbackFailed})
	if len(events) == 0 {
		t.Fatal("expected plugin.rollback_failed audit event")
	}
}

func TestMaybeAutoRollbackRequiresSnapshot(t *testing.T) {
	f := newRollbackFixture(t)
	if f.manager.maybeAutoRollback("com.example.signed") {
		t.Fatal("must not attempt rollback without a snapshot")
	}

	plugin, _ := f.manager.Get("com.example.signed")
	f.manager.ensureRollbackSnapshot(*plugin)
	if !f.manager.maybeAutoRollback("com.example.signed") {
		t.Fatal("expected rollback attempt with snapshot present")
	}

	// The async rollback audits its (expected, fake-binary) failure; wait
	// briefly for it so the audit assertion is stable.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events := f.manager.AuditEvents(AuditQuery{Action: AuditActionPluginRollbackFailed})
		if len(events) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("async rollback did not run within deadline")
}
