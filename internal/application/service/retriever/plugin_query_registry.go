package retriever

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

// PluginQueryRegistry holds read-only external retrievers. They are deliberately
// separate from RetrieveEngineRegistry because the plugin protocol has no index lifecycle RPCs.
type PluginQueryRegistry struct {
	mu      sync.RWMutex
	engines map[types.RetrieverEngineType]interfaces.RetrieveEngine
}

func NewPluginQueryRegistry() *PluginQueryRegistry {
	return &PluginQueryRegistry{engines: make(map[types.RetrieverEngineType]interfaces.RetrieveEngine)}
}

func (r *PluginQueryRegistry) Register(engine interfaces.RetrieveEngine) error {
	if engine == nil {
		return fmt.Errorf("plugin retriever is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.engines[engine.EngineType()]; exists {
		return fmt.Errorf("plugin retriever %q already registered", engine.EngineType())
	}
	r.engines[engine.EngineType()] = engine
	return nil
}

func (r *PluginQueryRegistry) Get(engineType types.RetrieverEngineType) (interfaces.RetrieveEngine, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	engine, ok := r.engines[engineType]
	return engine, ok
}

func (r *PluginQueryRegistry) List() []interfaces.RetrieveEngine {
	r.mu.RLock()
	result := make([]interfaces.RetrieveEngine, 0, len(r.engines))
	for _, engine := range r.engines {
		result = append(result, engine)
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].EngineType() < result[j].EngineType() })
	return result
}

type PluginLoader struct {
	queryRegistry *PluginQueryRegistry
	indexRegistry interfaces.RetrieveEngineRegistry
}

func NewPluginLoader(queryRegistry *PluginQueryRegistry, indexRegistry interfaces.RetrieveEngineRegistry) *PluginLoader {
	return &PluginLoader{queryRegistry: queryRegistry, indexRegistry: indexRegistry}
}

func (*PluginLoader) Type() plugin.ExtensionType { return plugin.ExtensionTypeRetriever }

func (l *PluginLoader) Load(ctx context.Context, manager *plugin.Manager, discovered plugin.Plugin) error {
	if l.queryRegistry == nil && l.indexRegistry == nil {
		return fmt.Errorf("plugin retriever registries are nil")
	}
	if discovered.Manifest.Spec.ExtensionType != l.Type() {
		return fmt.Errorf("expected %q plugin, got %q", l.Type(), discovered.Manifest.Spec.ExtensionType)
	}
	pluginID := discovered.Manifest.Metadata.ID
	if err := manager.StartOrRestart(ctx, pluginID, nil); err != nil {
		return fmt.Errorf("start retriever plugin %q: %w", pluginID, err)
	}
	client, err := manager.Connect(ctx, pluginID)
	if err != nil {
		return fmt.Errorf("connect retriever plugin %q: %w", pluginID, err)
	}
	defer client.Close()

	description, err := pluginpb.NewRetrieverPluginClient(client.Conn()).Describe(ctx, &pluginpb.RetrieverDescribeRequest{})
	if err != nil {
		return fmt.Errorf("describe retriever plugin %q: %w", pluginID, err)
	}
	engineType := types.RetrieverEngineType(strings.TrimSpace(description.GetEngineType()))
	if engineType == "" {
		return fmt.Errorf("retriever plugin %q returned an empty engine type", pluginID)
	}
	support := pluginRetrieverTypes(description.GetRetrieverTypes())
	if len(support) == 0 {
		return fmt.Errorf("retriever plugin %q returned no supported retriever types", pluginID)
	}
	if err := plugin.ValidateDescribeCapabilities(discovered.Manifest.Spec.Capabilities, description.GetCapabilities()); err != nil {
		return fmt.Errorf("retriever plugin %q: %w", pluginID, err)
	}

	base := pluginRetrieveEngine{
		manager: manager, pluginID: pluginID, engineType: engineType, support: support,
	}
	if hasIndexCapability(discovered.Manifest.Spec.Capabilities) {
		if l.indexRegistry == nil {
			return fmt.Errorf("plugin %q declares index capability but no index registry is available", pluginID)
		}
		if err := requireEmbeddingCapability(support, description.GetCapabilities()); err != nil {
			return fmt.Errorf("retriever plugin %q: %w", pluginID, err)
		}
		return l.indexRegistry.Register(newPluginIndexService(base))
	}
	if l.queryRegistry == nil {
		return fmt.Errorf("plugin %q has no index registry and no query registry is available", pluginID)
	}
	return l.queryRegistry.Register(&base)
}

type pluginRetrieveEngine struct {
	manager    *plugin.Manager
	pluginID   string
	engineType types.RetrieverEngineType
	support    []types.RetrieverType
}

func (e *pluginRetrieveEngine) EngineType() types.RetrieverEngineType { return e.engineType }
func (e *pluginRetrieveEngine) Support() []types.RetrieverType {
	return append([]types.RetrieverType(nil), e.support...)
}

func (e *pluginRetrieveEngine) Retrieve(ctx context.Context, params types.RetrieveParams) ([]*types.RetrieveResult, error) {
	if err := e.manager.StartOrRestart(ctx, e.pluginID, nil); err != nil {
		return nil, err
	}
	client, err := e.manager.Connect(ctx, e.pluginID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	response, err := pluginpb.NewRetrieverPluginClient(client.Conn()).Retrieve(ctx, &pluginpb.RetrieveRequest{
		Query: params.Query, Embedding: params.Embedding, KnowledgeBaseIds: params.KnowledgeBaseIDs,
		KnowledgeIds: params.KnowledgeIDs, TagIds: params.TagIDs, ExcludeKnowledgeIds: params.ExcludeKnowledgeIDs,
		ExcludeChunkIds: params.ExcludeChunkIDs, TopK: int32(params.TopK), Threshold: params.Threshold,
		KnowledgeType: params.KnowledgeType, RetrieverType: string(params.RetrieverType),
	})
	if err != nil {
		return nil, fmt.Errorf("retrieve with plugin %s: %w", e.pluginID, err)
	}
	hits := make([]*types.IndexWithScore, 0, len(response.Results))
	for _, hit := range response.Results {
		hits = append(hits, &types.IndexWithScore{
			ID: hit.Id, Content: hit.Content, SourceID: hit.SourceId, SourceType: pluginSourceType(hit.SourceType),
			ChunkID: hit.ChunkId, KnowledgeID: hit.KnowledgeId, KnowledgeBaseID: hit.KnowledgeBaseId,
			TagID: hit.TagId, Score: hit.Score, IsEnabled: hit.IsEnabled,
		})
	}
	return []*types.RetrieveResult{{
		Results: hits, RetrieverEngineType: e.engineType, RetrieverType: params.RetrieverType,
	}}, nil
}

func pluginRetrieverTypes(retrieverTypes []string) []types.RetrieverType {
	result := make([]types.RetrieverType, 0, len(retrieverTypes))
	seen := make(map[types.RetrieverType]struct{}, len(retrieverTypes))
	for _, retrieverType := range retrieverTypes {
		var mapped types.RetrieverType
		switch strings.ToLower(strings.TrimSpace(retrieverType)) {
		case "vector":
			mapped = types.VectorRetrieverType
		case "keywords":
			mapped = types.KeywordsRetrieverType
		default:
			continue
		}
		if _, exists := seen[mapped]; !exists {
			seen[mapped] = struct{}{}
			result = append(result, mapped)
		}
	}
	return result
}

// requireEmbeddingCapability rejects plugins that would receive vector-retrieval
// index records without declaring that they accept host-computed embeddings.
// The IndexRecord embedding field and the "embedding" capability were added to
// the protocol together; an older plugin whose Describe omits the capability
// would silently store text-only records and degrade vector recall.
func requireEmbeddingCapability(support []types.RetrieverType, describeCapabilities []string) error {
	if !slices.Contains(support, types.VectorRetrieverType) {
		return nil
	}
	if slices.Contains(describeCapabilities, "embedding") {
		return nil
	}
	return fmt.Errorf("plugin supports vector retrieval but its Describe does not declare the %q capability; upgrade the plugin SDK and echo all manifest capabilities from Describe", "embedding")
}

func pluginSourceType(value string) types.SourceType {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "passage":
		return types.PassageSourceType
	case "summary":
		return types.SummarySourceType
	default:
		return types.ChunkSourceType
	}
}
