package datasource

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/stretchr/testify/require"
)

func TestDataSourceLoaderRegistersPluginConnectorAndMetadata(t *testing.T) {
	pluginID := "com.example.loader-test"
	delete(ConnectorMetadataRegistry, pluginID)
	t.Cleanup(func() { delete(ConnectorMetadataRegistry, pluginID) })

	connectors := NewConnectorRegistry()
	loader := NewDataSourceLoader(connectors)
	discovered := plugin.Plugin{Manifest: plugin.Manifest{
		Metadata: plugin.Metadata{ID: pluginID, Name: "Loader Test", Description: "External datasource"},
		Spec:     plugin.Spec{ExtensionType: plugin.ExtensionTypeDataSource},
	}}

	require.NoError(t, loader.Load(context.Background(), plugin.NewManager(t.TempDir()), discovered))
	connector, err := connectors.Get(pluginID)
	require.NoError(t, err)
	require.IsType(t, &PluginConnector{}, connector)
	require.Equal(t, "Loader Test", ConnectorMetadataRegistry[pluginID].Name)
}

func TestDataSourceLoaderRejectsUnexpectedPluginType(t *testing.T) {
	loader := NewDataSourceLoader(NewConnectorRegistry())
	err := loader.Load(context.Background(), plugin.NewManager(t.TempDir()), plugin.Plugin{Manifest: plugin.Manifest{
		Metadata: plugin.Metadata{ID: "com.example.parser"},
		Spec:     plugin.Spec{ExtensionType: plugin.ExtensionTypeDocumentParser},
	}})
	require.EqualError(t, err, "expected \"datasource\" plugin, got \"document_parser\"")
}
