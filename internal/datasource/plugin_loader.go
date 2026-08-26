package datasource

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/plugin"
)

// DataSourceLoader installs external datasource plugins into the existing
// connector registry. It only creates adapters; Manager owns runtime lifecycle.
type DataSourceLoader struct {
	connectors *ConnectorRegistry
}

func NewDataSourceLoader(connectors *ConnectorRegistry) *DataSourceLoader {
	return &DataSourceLoader{connectors: connectors}
}

func (l *DataSourceLoader) Type() plugin.ExtensionType {
	return plugin.ExtensionTypeDataSource
}

func (l *DataSourceLoader) Load(_ context.Context, manager *plugin.Manager, discovered plugin.Plugin) error {
	if discovered.Manifest.Spec.ExtensionType != l.Type() {
		return fmt.Errorf("expected %q plugin, got %q", l.Type(), discovered.Manifest.Spec.ExtensionType)
	}
	if l.connectors == nil {
		return fmt.Errorf("connector registry is nil")
	}

	pluginID := discovered.Manifest.Metadata.ID
	if err := l.connectors.Register(NewPluginConnector(manager, pluginID)); err != nil {
		return fmt.Errorf("register connector: %w", err)
	}
	if err := RegisterPluginConnectorMetadata(pluginID, discovered.Manifest.Metadata.Name, discovered.Manifest.Metadata.Description); err != nil {
		return fmt.Errorf("register connector metadata: %w", err)
	}
	return nil
}
