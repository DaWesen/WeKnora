package datasource

import (
	"context"
	"testing"

	pluginpb "github.com/Tencent/WeKnora/internal/plugin/proto"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestPluginConnectorSecurityPolicyDenied(t *testing.T) {
	connector := &PluginConnector{pluginID: "com.example.local-files"}

	err := connector.syncError(context.Background(), &pluginpb.SyncError{
		Code:    pluginpb.SyncErrorCode_SYNC_ERROR_CODE_SECURITY_POLICY_DENIED,
		Target:  "api.example.com:443",
		Message: "outbound network is disabled",
	})

	if err == nil || err.Error() != "plugin security policy denied access to api.example.com:443: outbound network is disabled" {
		t.Fatalf("unexpected security policy error: %v", err)
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
