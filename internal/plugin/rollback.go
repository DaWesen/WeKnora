package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
)

// rollbackDirName is the directory (inside a plugin's own directory) holding
// the last-known-good snapshot: the plugin.yaml and, for process plugins, the
// entrypoint binary. A snapshot is taken on the first successful start and
// deliberately NOT refreshed afterwards — that way, after an operator drops
// in an upgrade, the snapshot still holds the previous working version.
const rollbackDirName = ".weknora-rollback"

// ErrNoRollbackSnapshot is returned by Rollback when the plugin has no
// last-known-good snapshot to restore from.
var ErrNoRollbackSnapshot = errors.New("plugin has no rollback snapshot")

// rollbackDir returns the snapshot directory for a plugin directory.
func rollbackDir(pluginDir string) string {
	return filepath.Join(pluginDir, rollbackDirName)
}

// HasRollbackSnapshot reports whether a last-known-good snapshot exists for
// the plugin, either as a candidate for manual rollback or automatic
// recovery after restart-budget exhaustion.
func (m *Manager) HasRollbackSnapshot(id string) bool {
	plugin, ok := m.Get(id)
	if !ok {
		return false
	}
	_, err := os.Stat(filepath.Join(rollbackDir(plugin.Directory), ManifestFileName))
	return err == nil
}

// ensureRollbackSnapshot copies the on-disk plugin.yaml (raw bytes, so any
// signature stays valid) plus the process entrypoint binary into the snapshot
// directory. It is a no-op when a snapshot already exists. Container plugins
// snapshot only the manifest — restoring the image relies on the tag still
// being present in the local image cache, which the docs call out.
func (m *Manager) ensureRollbackSnapshot(plugin Plugin) {
	snapshotDir := rollbackDir(plugin.Directory)
	manifestTarget := filepath.Join(snapshotDir, ManifestFileName)
	if _, err := os.Stat(manifestTarget); err == nil {
		return // snapshot from an earlier version still stands
	}
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		logger.Warnf(context.Background(), "[Plugin] cannot create rollback snapshot dir=%s error=%v", snapshotDir, err)
		return
	}
	if err := copyFile(filepath.Join(plugin.Directory, ManifestFileName), manifestTarget); err != nil {
		logger.Warnf(context.Background(), "[Plugin] cannot snapshot manifest for rollback id=%s error=%v", plugin.Manifest.Metadata.ID, err)
		return
	}
	// Process plugins: snapshot the binary referenced by entrypoint.command
	// so rollback restores a runnable artifact, not just metadata.
	if plugin.Manifest.Spec.Entrypoint.Type == "process" && len(plugin.Manifest.Spec.Entrypoint.Command) > 0 {
		bin := plugin.Manifest.Spec.Entrypoint.Command[0]
		if !filepath.IsAbs(bin) && !strings.HasPrefix(bin, "./") {
			bin = "./" + bin
		}
		src := filepath.Join(plugin.Directory, bin)
		if _, err := os.Stat(src); err == nil {
			target := filepath.Join(snapshotDir, filepath.Base(src))
			if err := copyFile(src, target); err != nil {
				logger.Warnf(context.Background(), "[Plugin] cannot snapshot binary for rollback id=%s error=%v", plugin.Manifest.Metadata.ID, err)
			}
		}
	}
}

// Rollback restores the last-known-good snapshot of a plugin, re-discovers it,
// and starts it again. It is exposed for manual recovery (POST
// /system/admin/plugins/:id/rollback) and used by automatic recovery when the
// restart budget is exhausted. The snapshot survives the rollback, so
// repeated rollbacks converge to the same last-known-good version. Failures
// are audited as plugin.rollback_failed.
func (m *Manager) Rollback(ctx context.Context, id string) error {
	plugin, ok := m.Get(id)
	if !ok {
		return fs.ErrNotExist
	}
	snapshotDir := rollbackDir(plugin.Directory)
	manifestBackup := filepath.Join(snapshotDir, ManifestFileName)
	if _, err := os.Stat(manifestBackup); err != nil {
		return ErrNoRollbackSnapshot
	}
	previousVersion := plugin.Manifest.Metadata.Version

	// Stop whatever is left of the current version (best-effort).
	if plugin.Status == StatusRunning {
		if err := m.runtime.Stop(ctx, id); err != nil {
			logger.Warnf(ctx, "[Plugin] stop before rollback failed id=%s error=%v", id, err)
		}
	}

	// Restore manifest bytes exactly: a signed manifest stays verifiable.
	if err := copyFile(manifestBackup, filepath.Join(plugin.Directory, ManifestFileName)); err != nil {
		m.recordAudit(id, AuditActionPluginRollbackFailed, "failed", "", "rollback failed", map[string]string{"error": err.Error()})
		return fmt.Errorf("restore manifest from snapshot: %w", err)
	}
	// Restore the snapshotted binary over the current one.
	if entries, err := os.ReadDir(snapshotDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() == ManifestFileName {
				continue
			}
			_ = copyFile(filepath.Join(snapshotDir, entry.Name()), filepath.Join(plugin.Directory, entry.Name()))
		}
	}

	// Re-discover so byID reflects the restored manifest, then start it.
	if err := m.Discover(); err != nil {
		m.recordAudit(id, AuditActionPluginRollbackFailed, "failed", "", "rollback failed", map[string]string{"error": err.Error()})
		return fmt.Errorf("re-discover after rollback: %w", err)
	}
	restored, ok := m.Get(id)
	if !ok {
		m.recordAudit(id, AuditActionPluginRollbackFailed, "failed", "", "rollback failed", map[string]string{"error": "restored plugin not discovered"})
		return fmt.Errorf("restored plugin %q not discovered after rollback", id)
	}
	config, _ := m.restartConfig(id)
	if err := m.StartWithConfig(ctx, id, config); err != nil {
		m.recordAudit(id, AuditActionPluginRollbackFailed, "failed", "", "rollback failed", map[string]string{"error": err.Error()})
		return fmt.Errorf("start restored plugin: %w", err)
	}
	m.recordAudit(id, AuditActionPluginRolledBack, "success", "", "plugin rolled back to last-known-good", map[string]string{
		"failed_version":   previousVersion,
		"restored_version": restored.Manifest.Metadata.Version,
	})
	logger.Infof(ctx, "[Plugin] rolled back id=%s from %s to %s", id, previousVersion, restored.Manifest.Metadata.Version)
	return nil
}

// maybeAutoRollback runs when automatic recovery ran out of restart budget.
// When a snapshot exists it takes precedence over giving up. Returns true
// when a rollback was attempted (successfully or not — either way the caller
// must not schedule further restarts of the failed version).
func (m *Manager) maybeAutoRollback(id string) bool {
	if !m.HasRollbackSnapshot(id) {
		return false
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// Rollback audits its own success or failure.
		if err := m.Rollback(ctx, id); err != nil {
			logger.Errorf(ctx, "[Plugin] automatic rollback failed id=%s error=%v", id, err)
		}
	}()
	return true
}

func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	target, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer target.Close()
	if _, err := io.Copy(target, source); err != nil {
		return err
	}
	return nil
}
