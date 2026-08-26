package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/Tencent/WeKnora/internal/types"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const pluginCursorKey = "plugin_cursor"

// PluginConnector adapts an external gRPC datasource plugin to the existing
// StreamingConnector contract. The service can therefore reuse its normal
// document ingestion, deletion, and checkpoint handling paths.
type PluginConnector struct {
	manager  *plugin.Manager
	pluginID string
}

func NewPluginConnector(manager *plugin.Manager, pluginID string) *PluginConnector {
	return &PluginConnector{manager: manager, pluginID: pluginID}
}

func (c *PluginConnector) Type() string { return c.pluginID }

func (c *PluginConnector) Validate(ctx context.Context, config *types.DataSourceConfig) error {
	configValues := pluginConfig(config)
	if err := c.manager.StartOrRestart(ctx, c.pluginID, configValues); err != nil {
		return err
	}
	client, err := c.manager.Connect(ctx, c.pluginID)
	if err != nil {
		c.markTransportFailure(ctx, err)
		return err
	}
	defer client.Close()

	if err := validatePluginConfig(ctx, client, configValues); err != nil {
		c.markTransportFailure(ctx, err)
		return err
	}
	response, err := pluginpb.NewDataSourcePluginClient(client.Conn()).ValidateCredentials(ctx, &pluginpb.ValidateCredentialsRequest{Config: configValues})
	if err != nil {
		c.markTransportFailure(ctx, err)
		return fmt.Errorf("validate plugin credentials: %w", err)
	}
	if err := validatePluginCredentials(response); err != nil {
		c.manager.RecordCredentialsDenied(c.pluginID)
		return err
	}
	return nil
}

func (c *PluginConnector) ListResources(ctx context.Context, config *types.DataSourceConfig, parentID string) ([]types.Resource, error) {
	client, configValues, err := c.connect(ctx, config)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	response, err := pluginpb.NewDataSourcePluginClient(client.Conn()).ListResources(ctx, &pluginpb.ListResourcesRequest{
		Config:   configValues,
		ParentId: parentID,
	})
	if err != nil {
		c.markTransportFailure(ctx, err)
		return nil, err
	}
	resources := make([]types.Resource, 0, len(response.Resources))
	for _, resource := range response.Resources {
		resources = append(resources, resourceFromPlugin(resource))
	}
	return resources, nil
}

func (c *PluginConnector) ResolveResourceAncestors(ctx context.Context, config *types.DataSourceConfig, resourceIDs []string) ([]string, error) {
	client, configValues, err := c.connect(ctx, config)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	response, err := pluginpb.NewDataSourcePluginClient(client.Conn()).ResolveResourceAncestors(ctx, &pluginpb.ResolveResourceAncestorsRequest{
		Config:      configValues,
		ResourceIds: resourceIDs,
	})
	if err != nil {
		c.markTransportFailure(ctx, err)
		return nil, err
	}
	return response.AncestorIds, nil
}

func (c *PluginConnector) FetchAll(ctx context.Context, config *types.DataSourceConfig, resourceIDs []string) ([]types.FetchedItem, error) {
	client, configValues, err := c.connect(ctx, config)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	response, err := pluginpb.NewDataSourcePluginClient(client.Conn()).FetchAll(ctx, &pluginpb.FetchAllRequest{
		Config:      configValues,
		ResourceIds: resourceIDs,
	})
	if err != nil {
		c.markTransportFailure(ctx, err)
		return nil, err
	}
	items := make([]types.FetchedItem, 0, len(response.Documents))
	for _, document := range response.Documents {
		items = append(items, fetchedItemFromDocument(document))
	}
	return items, nil
}

func (c *PluginConnector) FetchIncremental(context.Context, *types.DataSourceConfig, *types.SyncCursor) ([]types.FetchedItem, *types.SyncCursor, error) {
	return nil, nil, fmt.Errorf("plugin connector requires streaming sync")
}

func (c *PluginConnector) FetchStream(ctx context.Context, config *types.DataSourceConfig, cursor *types.SyncCursor, handler StreamHandler) (*types.SyncCursor, error) {
	configValues := pluginConfig(config)
	if err := c.manager.StartOrRestart(ctx, c.pluginID, configValues); err != nil {
		return nil, err
	}
	client, err := c.manager.Connect(ctx, c.pluginID)
	if err != nil {
		c.markTransportFailure(ctx, err)
		return nil, err
	}
	defer client.Close()

	if err := validatePluginConfig(ctx, client, configValues); err != nil {
		c.markTransportFailure(ctx, err)
		return nil, err
	}
	stream, err := pluginpb.NewDataSourcePluginClient(client.Conn()).Sync(ctx, &pluginpb.SyncRequest{
		Config:      configValues,
		Cursor:      readPluginCursor(cursor),
		ResourceIds: config.ResourceIDs,
	})
	if err != nil {
		c.markTransportFailure(ctx, err)
		return nil, err
	}

	var next *types.SyncCursor
	for {
		event, receiveErr := stream.Recv()
		if receiveErr == io.EOF {
			return next, nil
		}
		if receiveErr != nil {
			c.markTransportFailure(ctx, receiveErr)
			return next, fmt.Errorf("receive plugin sync event: %w", receiveErr)
		}

		switch payload := event.Payload.(type) {
		case *pluginpb.SyncEvent_UpsertDocument:
			if err := handler.Emit(ctx, fetchedItemFromUpsert(payload.UpsertDocument)); err != nil {
				return next, err
			}
		case *pluginpb.SyncEvent_DeleteDocument:
			if err := handler.Emit(ctx, types.FetchedItem{ExternalID: payload.DeleteDocument.SourceId, IsDeleted: true}); err != nil {
				return next, err
			}
		case *pluginpb.SyncEvent_Checkpoint:
			next = pluginSyncCursor(payload.Checkpoint.Cursor)
			if err := handler.Checkpoint(ctx, next); err != nil {
				return next, err
			}
		case *pluginpb.SyncEvent_Error:
			return next, c.syncError(ctx, payload.Error)
		case *pluginpb.SyncEvent_Completed:
			next = pluginSyncCursor(payload.Completed.Cursor)
			if err := handler.Checkpoint(ctx, next); err != nil {
				return next, err
			}
		}
	}
}

func (c *PluginConnector) Checkpoint(ctx context.Context, cursor *types.SyncCursor) error {
	return nil
}

func (c *PluginConnector) syncError(ctx context.Context, syncErr *pluginpb.SyncError) error {
	if syncErr.Code == pluginpb.SyncErrorCode_SYNC_ERROR_CODE_SECURITY_POLICY_DENIED {
		c.manager.RecordNetworkDenied(c.pluginID, syncErr.Target, syncErr.Message)
		logger.Warnf(ctx, "[Plugin] security policy denied id=%s target=%s message=%s", c.pluginID, syncErr.Target, syncErr.Message)
		return fmt.Errorf("plugin security policy denied access to %s: %s", syncErr.Target, syncErr.Message)
	}
	return fmt.Errorf("plugin sync error for %s: %s", syncErr.SourceId, syncErr.Message)
}

// markTransportFailure preserves a failed runtime state only for transport
// faults. Plugin-reported business errors, policy denials, host cancellation,
// and ingestion failures must not consume restart budget.
func (c *PluginConnector) markTransportFailure(ctx context.Context, err error) {
	if !shouldMarkRuntimeFailed(ctx, err) {
		return
	}
	if markErr := c.manager.MarkRuntimeFailed(c.pluginID, err); markErr != nil {
		logger.Warnf(ctx, "[Plugin] failed to record runtime failure id=%s error=%v", c.pluginID, markErr)
	}
}

func shouldMarkRuntimeFailed(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.Unknown, codes.Internal, codes.DataLoss:
		return true
	default:
		return false
	}
}

func (c *PluginConnector) connect(ctx context.Context, config *types.DataSourceConfig) (*plugin.Client, map[string]string, error) {
	configValues := pluginConfig(config)
	if err := c.manager.StartOrRestart(ctx, c.pluginID, configValues); err != nil {
		return nil, nil, err
	}
	client, err := c.manager.Connect(ctx, c.pluginID)
	if err != nil {
		c.markTransportFailure(ctx, err)
		return nil, nil, err
	}
	if err := validatePluginConfig(ctx, client, configValues); err != nil {
		_ = client.Close()
		c.markTransportFailure(ctx, err)
		return nil, nil, err
	}
	return client, configValues, nil
}

// validatePluginCredentials intentionally does not return the plugin response
// message. A connector can include upstream identity or authorization details
// that must not be exposed to API callers or persisted in audit history.
func validatePluginCredentials(response *pluginpb.ValidateCredentialsResponse) error {
	if response == nil || !response.Valid {
		return fmt.Errorf("plugin credentials are invalid")
	}
	return nil
}

func validatePluginConfig(ctx context.Context, client *plugin.Client, config map[string]string) error {
	response, err := client.ValidateConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("validate plugin configuration: %w", err)
	}
	if response.Valid {
		return nil
	}
	if len(response.Errors) == 0 {
		return fmt.Errorf("plugin configuration is invalid")
	}
	return fmt.Errorf("plugin configuration is invalid: %s", response.Errors[0].Field)
}

func pluginConfig(config *types.DataSourceConfig) map[string]string {
	result := make(map[string]string)
	if config == nil {
		return result
	}
	for key, value := range config.Settings {
		result[key] = stringifyPluginConfig(value)
	}
	for key, value := range config.Credentials {
		result[key] = stringifyPluginConfig(value)
	}
	return result
}

func stringifyPluginConfig(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

func readPluginCursor(cursor *types.SyncCursor) string {
	if cursor == nil || cursor.ConnectorCursor == nil {
		return ""
	}
	value, _ := cursor.ConnectorCursor[pluginCursorKey].(string)
	return value
}

func pluginSyncCursor(cursor string) *types.SyncCursor {
	return &types.SyncCursor{
		LastSyncTime: time.Now().UTC(),
		ConnectorCursor: map[string]any{
			pluginCursorKey: cursor,
		},
	}
}

func resourceFromPlugin(resource *pluginpb.Resource) types.Resource {
	modifiedAt, _ := time.Parse(time.RFC3339, resource.ModifiedAt)
	metadata := make(map[string]interface{}, len(resource.Metadata))
	for key, value := range resource.Metadata {
		metadata[key] = value
	}
	return types.Resource{
		ExternalID:  resource.ExternalId,
		Name:        resource.Name,
		Type:        resource.Type,
		Description: resource.Description,
		URL:         resource.Url,
		ModifiedAt:  modifiedAt,
		ParentID:    resource.ParentId,
		HasChildren: resource.HasChildren,
		Metadata:    metadata,
	}
}

func fetchedItemFromUpsert(document *pluginpb.UpsertDocument) types.FetchedItem {
	return fetchedItem(document.SourceId, "", document.Name, document.Content, document.ContentType, "", document.UpdatedAt, document.Metadata, false)
}

func fetchedItemFromDocument(document *pluginpb.Document) types.FetchedItem {
	return fetchedItem(document.SourceId, document.SourceResourceId, document.Name, document.Content, document.ContentType, document.Url, document.UpdatedAt, document.Metadata, document.IsDeleted)
}

func fetchedItem(sourceID, sourceResourceID, name string, content []byte, contentType, url, updatedAtRaw string, metadata map[string]string, isDeleted bool) types.FetchedItem {
	updatedAt, _ := time.Parse(time.RFC3339, updatedAtRaw)
	return types.FetchedItem{
		ExternalID:       sourceID,
		SourceResourceID: sourceResourceID,
		Title:            name,
		Content:          content,
		ContentType:      contentType,
		FileName:         name,
		URL:              url,
		UpdatedAt:        updatedAt,
		Metadata:         metadata,
		IsDeleted:        isDeleted,
	}
}
