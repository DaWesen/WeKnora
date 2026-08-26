package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type testExtensionLoader struct {
	extensionType ExtensionType
	loaded        []string
	err           error
}

func (l *testExtensionLoader) Type() ExtensionType { return l.extensionType }

func (l *testExtensionLoader) Load(_ context.Context, _ *Manager, discovered Plugin) error {
	l.loaded = append(l.loaded, discovered.Manifest.Metadata.ID)
	return l.err
}

func TestExtensionLoaderRegistryRegister(t *testing.T) {
	registry := NewExtensionLoaderRegistry()

	require.EqualError(t, registry.Register(nil), "extension loader is nil")
	require.EqualError(t, registry.Register(&testExtensionLoader{}), "extension loader type is empty")
	require.NoError(t, registry.Register(&testExtensionLoader{extensionType: ExtensionTypeDataSource}))
	require.EqualError(t, registry.Register(&testExtensionLoader{extensionType: ExtensionTypeDataSource}), "extension loader already registered for \"datasource\"")
}

func TestExtensionLoaderRegistryLoadAll(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.byID["datasource"] = &Plugin{Manifest: Manifest{Metadata: Metadata{ID: "datasource"}, Spec: Spec{ExtensionType: ExtensionTypeDataSource}}}
	manager.byID["web-search"] = &Plugin{Manifest: Manifest{Metadata: Metadata{ID: "web-search"}, Spec: Spec{ExtensionType: ExtensionTypeWebSearch}}}

	datasourceLoader := &testExtensionLoader{extensionType: ExtensionTypeDataSource}
	webSearchLoader := &testExtensionLoader{extensionType: ExtensionTypeWebSearch, err: errors.New("registry unavailable")}
	registry := NewExtensionLoaderRegistry()
	require.NoError(t, registry.Register(datasourceLoader))
	require.NoError(t, registry.Register(webSearchLoader))

	err := registry.LoadAll(context.Background(), manager)
	require.EqualError(t, err, "load plugin web-search: registry unavailable")
	require.Equal(t, []string{"datasource"}, datasourceLoader.loaded)
	require.Equal(t, []string{"web-search"}, webSearchLoader.loaded)
	require.Len(t, registry.Descriptors(), 1)
	require.Equal(t, "datasource", registry.Descriptors()[0].ID)
}

func TestExtensionLoaderRegistryLoadRejectsMissingType(t *testing.T) {
	registry := NewExtensionLoaderRegistry()
	err := registry.Load(context.Background(), NewManager(t.TempDir()), Plugin{Manifest: Manifest{Metadata: Metadata{ID: "parser"}, Spec: Spec{ExtensionType: ExtensionTypeDocumentParser}}})
	require.EqualError(t, err, "no extension loader registered for \"document_parser\"")
}

func TestExtensionLoaderRegistryDescriptorsAreIsolatedAndSorted(t *testing.T) {
	registry := NewExtensionLoaderRegistry()
	loader := &testExtensionLoader{extensionType: ExtensionTypeDataSource}
	require.NoError(t, registry.Register(loader))

	for _, id := range []string{"zeta", "alpha"} {
		require.NoError(t, registry.Load(context.Background(), nil, Plugin{Manifest: Manifest{
			Metadata: Metadata{ID: id, Name: id},
			Spec: Spec{
				ExtensionType: ExtensionTypeDataSource,
				Capabilities:  []string{"sync"},
				ConfigSchema:  map[string]any{"nested": map[string]any{"values": []any{"one"}}},
			},
		}}))
	}

	descriptors := registry.Descriptors()
	require.Equal(t, []string{"alpha", "zeta"}, []string{descriptors[0].ID, descriptors[1].ID})
	descriptors[0].Capabilities[0] = "changed"
	descriptors[0].ConfigSchema["nested"].(map[string]any)["values"].([]any)[0] = "changed"

	descriptor, ok := registry.Descriptor("alpha")
	require.True(t, ok)
	require.Equal(t, []string{"sync"}, descriptor.Capabilities)
	require.Equal(t, "one", descriptor.ConfigSchema["nested"].(map[string]any)["values"].([]any)[0])
}
