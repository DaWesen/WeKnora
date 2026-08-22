package datasource

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/plugin"
	pluginpb "github.com/Tencent/WeKnora/internal/plugin/proto"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPluginConnectorSyncErrorSecurityPolicyDenied(t *testing.T) {
	manager := plugin.NewManager(t.TempDir())
	connector := &PluginConnector{manager: manager, pluginID: "com.example.local-files"}

	err := connector.syncError(context.Background(), &pluginpb.SyncError{
		Code:    pluginpb.SyncErrorCode_SYNC_ERROR_CODE_SECURITY_POLICY_DENIED,
		Target:  "api.example.com:443",
		Message: "outbound network is disabled",
	})

	if err == nil || err.Error() != "plugin security policy denied access to api.example.com:443: outbound network is disabled" {
		t.Fatalf("unexpected security policy error: %v", err)
	}
	events := manager.AuditEvents(plugin.AuditQuery{PluginID: "com.example.local-files"})
	if len(events) != 1 || events[0].Action != plugin.AuditActionPluginNetworkDenied || events[0].Target != "api.example.com:443" {
		t.Fatalf("security denial audit was not recorded: %#v", events)
	}
}

func TestPluginTransportFailuresAreEligibleForRecovery(t *testing.T) {
	require.True(t, shouldMarkRuntimeFailed(context.Background(), status.Error(codes.Unavailable, "connection dropped")))
	require.True(t, shouldMarkRuntimeFailed(context.Background(), status.Error(codes.Internal, "stream broke")))
	require.False(t, shouldMarkRuntimeFailed(context.Background(), status.Error(codes.InvalidArgument, "bad config")))
	require.False(t, shouldMarkRuntimeFailed(context.Background(), context.Canceled))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, shouldMarkRuntimeFailed(canceled, status.Error(codes.Unavailable, "host cancelled")))
}

func TestRegisterPluginConnectorMetadata(t *testing.T) {
	pluginID := "com.example.test-plugin"
	delete(ConnectorMetadataRegistry, pluginID)
	t.Cleanup(func() { delete(ConnectorMetadataRegistry, pluginID) })

	if err := RegisterPluginConnectorMetadata(pluginID, "Test Plugin", "External test datasource"); err != nil {
		t.Fatal(err)
	}
	metadata := ConnectorMetadataRegistry[pluginID]
	if metadata.Name != "Test Plugin" || metadata.AuthType != "none" {
		t.Fatalf("unexpected plugin metadata: %#v", metadata)
	}
	if err := RegisterPluginConnectorMetadata(pluginID, "Test Plugin", "External test datasource"); err == nil {
		t.Fatal("expected duplicate metadata registration to fail")
	}
}

func TestFeishuMetadataDoesNotAdvertiseWebhook(t *testing.T) {
	meta := ConnectorMetadataRegistry[types.ConnectorTypeFeishu]

	for _, capability := range meta.Capabilities {
		if capability == "webhook" {
			t.Fatalf("Feishu connector should not advertise webhook until webhook sync is implemented")
		}
	}
}
