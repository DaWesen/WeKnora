package plugin

import (
	"context"
	"io/fs"
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

func TestStartOrRestartDoesNotBypassFailedRuntimePolicy(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["com.example.files"] = &Plugin{
		Manifest:  validRestartManifest(),
		Status:    StatusFailed,
		LastError: "plugin process exited",
	}

	err := manager.StartOrRestart(context.Background(), "com.example.files", nil)
	require.ErrorContains(t, err, "automatic restart is disabled")

	current, ok := manager.Get("com.example.files")
	require.True(t, ok)
	require.Equal(t, StatusFailed, current.Status)
	require.Equal(t, "plugin process exited", current.LastError)
	events := manager.AuditEvents(AuditQuery{
		PluginID: "com.example.files",
		Action:   AuditActionPluginRestartDenied,
	})
	require.Len(t, events, 1)
}

func TestStartOrRestartRejectsUnknownPlugin(t *testing.T) {
	manager := NewManager(t.TempDir())

	err := manager.StartOrRestart(context.Background(), "missing", nil)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestStartOrRestartDoesNotMarkInvalidConfigurationAsRuntimeFailure(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := validRestartManifest()
	manifest.Spec.ConfigSchema = map[string]any{
		"required": []any{"rootPath"},
		"properties": map[string]any{
			"rootPath": map[string]any{"type": "string"},
		},
	}
	manager.byID["com.example.files"] = &Plugin{
		Manifest: manifest,
		Status:   StatusDiscovered,
	}

	err := manager.StartOrRestart(context.Background(), "com.example.files", nil)
	require.ErrorContains(t, err, "rootPath")

	current, ok := manager.Get("com.example.files")
	require.True(t, ok)
	require.Equal(t, StatusDiscovered, current.Status)
	require.Empty(t, current.LastError)
	events := manager.AuditEvents(AuditQuery{
		PluginID: "com.example.files",
		Action:   AuditActionPluginStartFailed,
	})
	require.Len(t, events, 1)
	require.Equal(t, "denied", events[0].Outcome)
	require.Equal(t, "manifest_config", events[0].Details["stage"])
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

func TestMarkRuntimeFailedSetsStatusAndAudits(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["com.example.files"] = &Plugin{Status: StatusRunning}

	require.NoError(t, manager.MarkRuntimeFailed("com.example.files", context.DeadlineExceeded))
	plugin, ok := manager.Get("com.example.files")
	require.True(t, ok)
	require.Equal(t, StatusFailed, plugin.Status)
	require.Equal(t, "plugin runtime failed", plugin.LastError)

	events := manager.AuditEvents(AuditQuery{PluginID: "com.example.files", Action: AuditActionPluginRuntimeFailed})
	require.Len(t, events, 1)
	require.Equal(t, "failed", events[0].Outcome)
	require.Equal(t, "plugin runtime failed", events[0].Message)
}

func TestMarkRuntimeFailedRequiresCause(t *testing.T) {
	manager := NewManager(t.TempDir())
	require.ErrorContains(t, manager.MarkRuntimeFailed("com.example.files", nil), "cause is required")
}

func TestRestartConfigIsCopiedAtBothBoundaries(t *testing.T) {
	manager := NewManager(t.TempDir())
	config := map[string]string{"token": "initial", "rootPath": "C:/files"}
	manager.rememberRestartConfig("com.example.files", config)
	config["token"] = "changed-by-caller"

	retained, ok := manager.restartConfig("com.example.files")
	require.True(t, ok)
	require.Equal(t, "initial", retained["token"])
	retained["token"] = "changed-by-reader"

	again, ok := manager.restartConfig("com.example.files")
	require.True(t, ok)
	require.Equal(t, "initial", again["token"])
}

func TestHandleRuntimeFailureWithoutRetainedConfigStaysFailed(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := validRestartManifest()
	manifest.Spec.RestartPolicy = &RestartPolicy{
		Enabled:       true,
		MaxAttempts:   2,
		WindowSeconds: 60,
	}
	manager.byID["com.example.files"] = &Plugin{Manifest: manifest, Status: StatusRunning}

	manager.handleRuntimeFailure("com.example.files", context.DeadlineExceeded)

	current, ok := manager.Get("com.example.files")
	require.True(t, ok)
	require.Equal(t, StatusFailed, current.Status)
	require.Equal(t, "plugin runtime failed", current.LastError)
	require.False(t, manager.runtime.IsStarted("com.example.files"))

	events := manager.AuditEvents(AuditQuery{PluginID: "com.example.files"})
	require.Len(t, events, 2)
	require.Equal(t, AuditActionPluginRestartDenied, events[0].Action)
	require.Equal(t, "denied", events[0].Outcome)
	require.NotContains(t, events[0].Message, "token")
	require.Equal(t, AuditActionPluginRuntimeFailed, events[1].Action)
}

func TestHandleRuntimeFailureDoesNotRestartWhenPolicyDisabled(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["com.example.files"] = &Plugin{
		Manifest: validRestartManifest(),
		Status:   StatusRunning,
	}
	manager.rememberRestartConfig("com.example.files", map[string]string{"token": "secret"})

	manager.handleRuntimeFailure("com.example.files", context.DeadlineExceeded)

	current, ok := manager.Get("com.example.files")
	require.True(t, ok)
	require.Equal(t, StatusFailed, current.Status)
	require.False(t, manager.runtime.IsStarted("com.example.files"))
	events := manager.AuditEvents(AuditQuery{PluginID: "com.example.files"})
	require.Len(t, events, 1)
	require.Equal(t, AuditActionPluginRuntimeFailed, events[0].Action)
}

func TestReplacementRecoveryWaitsForPreviousRecovery(t *testing.T) {
	manager := NewManager(t.TempDir())
	firstContext, first := manager.beginAutomaticRecovery("com.example.files")
	secondContext, second := manager.beginAutomaticRecovery("com.example.files")

	select {
	case <-firstContext.Done():
	case <-time.After(time.Second):
		t.Fatal("previous automatic recovery was not cancelled")
	}
	ready := make(chan bool, 1)
	go func() { ready <- waitForPreviousRecovery(secondContext, second) }()
	select {
	case <-ready:
		t.Fatal("replacement recovery did not wait for its predecessor")
	case <-time.After(20 * time.Millisecond):
	}

	manager.endAutomaticRecovery("com.example.files", first)
	select {
	case started := <-ready:
		require.True(t, started)
	case <-time.After(time.Second):
		t.Fatal("replacement recovery did not continue after its predecessor exited")
	}
	manager.endAutomaticRecovery("com.example.files", second)
}

func TestStopCancelsEntireAutomaticRecoveryChain(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["com.example.files"] = &Plugin{
		Manifest: validRestartManifest(),
		Status:   StatusFailed,
	}
	firstContext, first := manager.beginAutomaticRecovery("com.example.files")
	secondContext, second := manager.beginAutomaticRecovery("com.example.files")
	for _, recovery := range []struct {
		id       string
		ctx      context.Context
		recovery *automaticRecovery
	}{
		{id: "com.example.files", ctx: firstContext, recovery: first},
		{id: "com.example.files", ctx: secondContext, recovery: second},
	} {
		go func() {
			<-recovery.ctx.Done()
			manager.endAutomaticRecovery(recovery.id, recovery.recovery)
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, manager.Stop(ctx, "com.example.files"))
	for _, recoveryContext := range []context.Context{firstContext, secondContext} {
		select {
		case <-recoveryContext.Done():
		case <-time.After(time.Second):
			t.Fatal("automatic recovery chain was not cancelled")
		}
	}
}

func TestStopForgetsRetainedRestartConfig(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["com.example.files"] = &Plugin{
		Manifest: validRestartManifest(),
		Status:   StatusRunning,
	}
	manager.rememberRestartConfig("com.example.files", map[string]string{"token": "secret"})

	require.NoError(t, manager.Stop(context.Background(), "com.example.files"))
	_, ok := manager.restartConfig("com.example.files")
	require.False(t, ok)
}

func TestStopClearsHealthState(t *testing.T) {
	manager := NewManager(t.TempDir())
	plugin := validRestartManifest()
	plugin.Spec.HealthCheck = &HealthCheck{IntervalSeconds: 30, TimeoutSeconds: 5}
	manager.byID[plugin.Metadata.ID] = &Plugin{Manifest: plugin, Status: StatusRunning}
	manager.healthStates[plugin.Metadata.ID] = &healthState{
		consecutiveFailures: 1,
		lastCheckedAt:       time.Now().UTC(),
		lastFailureAt:       time.Now().UTC(),
	}

	require.NoError(t, manager.Stop(context.Background(), plugin.Metadata.ID))
	status, ok := manager.HealthStatus(plugin.Metadata.ID)
	require.True(t, ok)
	require.Zero(t, status.ConsecutiveFailures)
	require.True(t, status.LastCheckedAt.IsZero())
	require.True(t, status.LastFailureAt.IsZero())
}

func TestStopCancelsAutomaticRecovery(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["com.example.files"] = &Plugin{
		Manifest: validRestartManifest(),
		Status:   StatusFailed,
	}
	ctx, recovery := manager.beginAutomaticRecovery("com.example.files")
	go func() {
		<-ctx.Done()
		manager.endAutomaticRecovery("com.example.files", recovery)
	}()

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, manager.Stop(stopCtx, "com.example.files"))
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("automatic recovery was not cancelled")
	}
}

func TestStopAllForgetsAllRetainedRestartConfigs(t *testing.T) {
	manager := NewManager(t.TempDir())
	first := validRestartManifest()
	second := validRestartManifest()
	second.Metadata.ID = "com.example.second"
	manager.byID[first.Metadata.ID] = &Plugin{Manifest: first, Status: StatusRunning}
	manager.byID[second.Metadata.ID] = &Plugin{Manifest: second, Status: StatusFailed}
	manager.rememberRestartConfig(first.Metadata.ID, map[string]string{"token": "first"})
	manager.rememberRestartConfig(second.Metadata.ID, map[string]string{"token": "second"})

	require.NoError(t, manager.StopAll(context.Background()))
	_, firstOK := manager.restartConfig(first.Metadata.ID)
	_, secondOK := manager.restartConfig(second.Metadata.ID)
	require.False(t, firstOK)
	require.False(t, secondOK)
}

func TestStopAllClearsHealthStates(t *testing.T) {
	manager := NewManager(t.TempDir())
	first := validRestartManifest()
	second := validRestartManifest()
	second.Metadata.ID = "com.example.second"
	for _, manifest := range []Manifest{first, second} {
		manager.byID[manifest.Metadata.ID] = &Plugin{Manifest: manifest, Status: StatusFailed}
		manager.healthStates[manifest.Metadata.ID] = &healthState{consecutiveFailures: 1, lastCheckedAt: time.Now().UTC()}
	}

	require.NoError(t, manager.StopAll(context.Background()))
	require.Empty(t, manager.healthStates)
}

func TestStopAllCancelsAutomaticRecoveries(t *testing.T) {
	manager := NewManager(t.TempDir())
	first := validRestartManifest()
	second := validRestartManifest()
	second.Metadata.ID = "com.example.second"
	manager.byID[first.Metadata.ID] = &Plugin{Manifest: first, Status: StatusFailed}
	manager.byID[second.Metadata.ID] = &Plugin{Manifest: second, Status: StatusFailed}
	firstContext, firstRecovery := manager.beginAutomaticRecovery(first.Metadata.ID)
	secondContext, secondRecovery := manager.beginAutomaticRecovery(second.Metadata.ID)
	for _, recovery := range []struct {
		id       string
		ctx      context.Context
		recovery *automaticRecovery
	}{
		{id: first.Metadata.ID, ctx: firstContext, recovery: firstRecovery},
		{id: second.Metadata.ID, ctx: secondContext, recovery: secondRecovery},
	} {
		go func() {
			<-recovery.ctx.Done()
			manager.endAutomaticRecovery(recovery.id, recovery.recovery)
		}()
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, manager.StopAll(stopCtx))
	for _, recoveryContext := range []context.Context{firstContext, secondContext} {
		select {
		case <-recoveryContext.Done():
		case <-time.After(time.Second):
			t.Fatal("automatic recovery was not cancelled")
		}
	}
}

func TestStopReturnsWhenAutomaticRecoveryDoesNotExit(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["com.example.files"] = &Plugin{
		Manifest: validRestartManifest(),
		Status:   StatusFailed,
	}
	_, _ = manager.beginAutomaticRecovery("com.example.files")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, manager.Stop(ctx, "com.example.files"), context.DeadlineExceeded)
}

func TestRestartRejectsPluginOutsideFailedState(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := validRestartManifest()
	manifest.Spec.RestartPolicy = &RestartPolicy{Enabled: true, MaxAttempts: 1, WindowSeconds: 60}
	manager.byID["com.example.files"] = &Plugin{Manifest: manifest, Status: StatusRunning}

	err := manager.Restart(context.Background(), "com.example.files", nil)
	require.ErrorContains(t, err, "not in failed state")
	events := manager.AuditEvents(AuditQuery{PluginID: "com.example.files", Action: AuditActionPluginRestartDenied})
	require.Len(t, events, 1)
}

func TestRestartRejectsConcurrentAttempt(t *testing.T) {
	manager := NewManager(t.TempDir())
	manifest := validRestartManifest()
	manifest.Spec.RestartPolicy = &RestartPolicy{Enabled: true, MaxAttempts: 1, WindowSeconds: 60}
	manager.byID["com.example.files"] = &Plugin{Manifest: manifest, Status: StatusFailed}

	require.True(t, manager.beginRestart("com.example.files"))
	t.Cleanup(func() { manager.endRestart("com.example.files") })
	err := manager.Restart(context.Background(), "com.example.files", nil)
	require.ErrorContains(t, err, "already in progress")
	events := manager.AuditEvents(AuditQuery{PluginID: "com.example.files", Action: AuditActionPluginRestartDenied})
	require.Len(t, events, 1)
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
