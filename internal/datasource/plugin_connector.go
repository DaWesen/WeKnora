package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/Tencent/WeKnora/internal/plugin"
	pluginpb "github.com/Tencent/WeKnora/internal/plugin/proto"
	"github.com/Tencent/WeKnora/internal/types"
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
	if err := c.manager.Start(ctx, c.pluginID); err != nil {
		return err
	}
	client, err := c.manager.Connect(ctx, c.pluginID)
	if err != nil {
		return err
	}
	defer client.Close()

	response, err := client.ValidateCredentials(ctx, pluginConfig(config))
	if err != nil {
		return err
	}
	if !response.Valid {
		return fmt.Errorf("plugin credentials are invalid: %s", response.Message)
	}
	return nil
}

func (c *PluginConnector) ListResources(context.Context, *types.DataSourceConfig, string) ([]types.Resource, error) {
	return nil, nil
}

func (c *PluginConnector) ResolveResourceAncestors(context.Context, *types.DataSourceConfig, []string) ([]string, error) {
	return nil, nil
}

func (c *PluginConnector) FetchAll(context.Context, *types.DataSourceConfig, []string) ([]types.FetchedItem, error) {
	return nil, fmt.Errorf("plugin connector requires streaming sync")
}

func (c *PluginConnector) FetchIncremental(context.Context, *types.DataSourceConfig, *types.SyncCursor) ([]types.FetchedItem, *types.SyncCursor, error) {
	return nil, nil, fmt.Errorf("plugin connector requires streaming sync")
}

func (c *PluginConnector) FetchStream(ctx context.Context, config *types.DataSourceConfig, cursor *types.SyncCursor, handler StreamHandler) (*types.SyncCursor, error) {
	if err := c.manager.Start(ctx, c.pluginID); err != nil {
		return nil, err
	}
	client, err := c.manager.Connect(ctx, c.pluginID)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	stream, err := client.Sync(ctx, "", pluginConfig(config), readPluginCursor(cursor))
	if err != nil {
		return nil, err
	}

	var next *types.SyncCursor
	for {
		event, receiveErr := stream.Recv()
		if receiveErr == io.EOF {
			return next, nil
		}
		if receiveErr != nil {
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
			return next, fmt.Errorf("plugin sync error for %s: %s", payload.Error.SourceId, payload.Error.Message)
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

func fetchedItemFromUpsert(document *pluginpb.UpsertDocument) types.FetchedItem {
	updatedAt, _ := time.Parse(time.RFC3339, document.UpdatedAt)
	return types.FetchedItem{
		ExternalID:  document.SourceId,
		Title:       document.Name,
		Content:     document.Content,
		ContentType: document.ContentType,
		FileName:    document.Name,
		UpdatedAt:   updatedAt,
		Metadata:    document.Metadata,
	}
}
