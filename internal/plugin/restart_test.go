package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManifestValidatesRestartPolicy(t *testing.T) {
	manifest := validRestartManifest()
	manifest.Spec.RestartPolicy = &RestartPolicy{
		Enabled:       true,
		MaxAttempts:   3,
		WindowSeconds: 60,
		BackoffMillis: 10,
	}
	require.NoError(t, manifest.Validate())

	manifest.Spec.RestartPolicy.MaxAttempts = 0
	require.ErrorContains(t, manifest.Validate(), "maxAttempts")
	manifest.Spec.RestartPolicy.MaxAttempts = 11
	require.ErrorContains(t, manifest.Validate(), "maxAttempts")
	manifest.Spec.RestartPolicy.MaxAttempts = 3

	manifest.Spec.RestartPolicy.WindowSeconds = 0
	require.ErrorContains(t, manifest.Validate(), "windowSeconds")
	manifest.Spec.RestartPolicy.WindowSeconds = 3601
	require.ErrorContains(t, manifest.Validate(), "windowSeconds")
	manifest.Spec.RestartPolicy.WindowSeconds = 60

	manifest.Spec.RestartPolicy.BackoffMillis = -1
	require.ErrorContains(t, manifest.Validate(), "backoffMillis")
	manifest.Spec.RestartPolicy.BackoffMillis = 60001
	require.ErrorContains(t, manifest.Validate(), "backoffMillis")
}

func TestReserveRestartAttemptEnforcesBudgetWithinWindow(t *testing.T) {
	manager := NewManager(t.TempDir())
	policy := RestartPolicy{Enabled: true, MaxAttempts: 2, WindowSeconds: 30}
	now := time.Date(2026, time.August, 21, 10, 0, 0, 0, time.UTC)

	attempt, err := manager.reserveRestartAttempt("com.example.files", policy, now)
	require.NoError(t, err)
	require.Equal(t, 1, attempt)

	attempt, err = manager.reserveRestartAttempt("com.example.files", policy, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, 2, attempt)

	_, err = manager.reserveRestartAttempt("com.example.files", policy, now.Add(2*time.Second))
	require.ErrorContains(t, err, "restart budget exhausted")

	attempt, err = manager.reserveRestartAttempt("com.example.files", policy, now.Add(32*time.Second))
	require.NoError(t, err)
	require.Equal(t, 1, attempt)
}

func TestRestartBudgetIsIndependentPerPlugin(t *testing.T) {
	manager := NewManager(t.TempDir())
	policy := RestartPolicy{Enabled: true, MaxAttempts: 1, WindowSeconds: 60}
	now := time.Now().UTC()

	_, err := manager.reserveRestartAttempt("first", policy, now)
	require.NoError(t, err)
	_, err = manager.reserveRestartAttempt("first", policy, now.Add(time.Second))
	require.ErrorContains(t, err, "restart budget exhausted")

	attempt, err := manager.reserveRestartAttempt("second", policy, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, 1, attempt)
}

func TestRestartRejectsDisabledPolicyAndAudits(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["com.example.files"] = &Plugin{
		Manifest: validRestartManifest(),
		Status:   StatusFailed,
	}

	err := manager.Restart(context.Background(), "com.example.files", nil)
	require.ErrorContains(t, err, "automatic restart is disabled")

	events := manager.AuditEvents(AuditQuery{PluginID: "com.example.files"})
	require.Len(t, events, 1)
	require.Equal(t, AuditActionPluginRestartDenied, events[0].Action)
	require.Equal(t, "denied", events[0].Outcome)
}

func TestRestartRejectsExhaustedBudgetAndAudits(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := validRestartManifest()
	manifest.Spec.RestartPolicy = &RestartPolicy{
		Enabled:       true,
		MaxAttempts:   1,
		WindowSeconds: 60,
		BackoffMillis: 0,
	}
	manager.byID["com.example.files"] = &Plugin{Manifest: manifest, Status: StatusFailed}
	require.NoError(t, manager.runtime.Stop(context.Background(), "com.example.files"))

	_, err := manager.reserveRestartAttempt("com.example.files", *manifest.Spec.RestartPolicy, time.Now().UTC())
	require.NoError(t, err)
	err = manager.Restart(context.Background(), "com.example.files", nil)
	require.ErrorContains(t, err, "restart budget exhausted")

	events := manager.AuditEvents(AuditQuery{PluginID: "com.example.files", Action: AuditActionPluginRestartDenied})
	require.Len(t, events, 1)
	require.Contains(t, requireEventMessage(t, events[0]), "restart budget exhausted")
}

func TestRestartBackoffUsesPolicyOrDefault(t *testing.T) {
	require.Equal(t, defaultRestartBackoff, restartBackoff(RestartPolicy{}))
	require.Equal(t, 123*time.Millisecond, restartBackoff(RestartPolicy{BackoffMillis: 123}))
}

func validRestartManifest() Manifest {
	return Manifest{
		APIVersion: APIVersionV1,
		Kind:       "Plugin",
		Metadata: Metadata{
			ID:      "com.example.files",
			Name:    "Files",
			Version: "1.0.0",
		},
		Spec: Spec{
			ExtensionType:  ExtensionTypeDataSource,
			WeKnoraVersion: ">=0.1.0",
			Entrypoint: Entrypoint{
				Type:        "process",
				Command:     []string{"./plugin"},
				GRPCAddress: "127.0.0.1:50051",
			},
			Permissions: Permissions{Network: NetworkPermission{Enabled: true}},
		},
	}
}

func requireEventMessage(t *testing.T, event AuditEvent) string {
	t.Helper()
	require.NotEmpty(t, event.Message)
	return event.Message
}
