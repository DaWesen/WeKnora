package retriever

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

// pluginIndexService wraps an external retriever plugin that implements the
// full index lifecycle RPCs. It satisfies interfaces.RetrieveEngineService so
// it can be registered in the standard RetrieveEngineRegistry alongside
// built-in engines.
type pluginIndexService struct {
	pluginRetrieveEngine
}

var _ interfaces.RetrieveEngineService = (*pluginIndexService)(nil)

func newPluginIndexService(base pluginRetrieveEngine) *pluginIndexService {
	return &pluginIndexService{pluginRetrieveEngine: base}
}

func (s *pluginIndexService) Index(ctx context.Context, embedder embedding.Embedder, indexInfo *types.IndexInfo, retrieverTypes []types.RetrieverType) error {
	if indexInfo == nil {
		return fmt.Errorf("index info is nil")
	}
	record := toIndexRecord(indexInfo)
	if err := embedIndexRecords(ctx, embedder, []*pluginpb.IndexRecord{record}, retrieverTypes); err != nil {
		return err
	}
	if err := s.manager.StartOrRestart(ctx, s.pluginID, nil); err != nil {
		return err
	}
	client, err := s.manager.Connect(ctx, s.pluginID)
	if err != nil {
		return err
	}
	defer client.Close()
	_, err = pluginpb.NewRetrieverPluginClient(client.Conn()).SaveIndex(ctx, &pluginpb.SaveIndexRequest{
		Index: record,
	})
	if err != nil {
		return fmt.Errorf("save index with plugin %s: %w", s.pluginID, err)
	}
	return nil
}

func (s *pluginIndexService) BatchIndex(ctx context.Context, embedder embedding.Embedder, indexInfoList []*types.IndexInfo, retrieverTypes []types.RetrieverType) error {
	records := make([]*pluginpb.IndexRecord, 0, len(indexInfoList))
	for _, info := range indexInfoList {
		records = append(records, toIndexRecord(info))
	}
	if err := embedIndexRecords(ctx, embedder, records, retrieverTypes); err != nil {
		return err
	}
	if err := s.manager.StartOrRestart(ctx, s.pluginID, nil); err != nil {
		return err
	}
	client, err := s.manager.Connect(ctx, s.pluginID)
	if err != nil {
		return err
	}
	defer client.Close()
	_, err = pluginpb.NewRetrieverPluginClient(client.Conn()).BatchSaveIndex(ctx, &pluginpb.BatchSaveIndexRequest{
		Indices: records,
	})
	if err != nil {
		return fmt.Errorf("batch save index with plugin %s: %w", s.pluginID, err)
	}
	return nil
}

func (s *pluginIndexService) EstimateStorageSize(_ context.Context, _ embedding.Embedder, _ []*types.IndexInfo, _ []types.RetrieverType) int64 {
	return 0
}

func (s *pluginIndexService) DeleteByChunkIDList(ctx context.Context, chunkIDs []string, dimension int, knowledgeType string) error {
	return s.callDelete(ctx, func(c pluginpb.RetrieverPluginClient) error {
		_, err := c.DeleteByChunkIDs(ctx, &pluginpb.DeleteByChunkIDsRequest{
			ChunkIds: chunkIDs, Dimension: int32(dimension), KnowledgeType: knowledgeType,
		})
		return err
	}, "delete by chunk ids")
}

func (s *pluginIndexService) DeleteBySourceIDList(ctx context.Context, sourceIDs []string, dimension int, knowledgeType string) error {
	return s.callDelete(ctx, func(c pluginpb.RetrieverPluginClient) error {
		_, err := c.DeleteBySourceIDs(ctx, &pluginpb.DeleteBySourceIDsRequest{
			SourceIds: sourceIDs, Dimension: int32(dimension), KnowledgeType: knowledgeType,
		})
		return err
	}, "delete by source ids")
}

func (s *pluginIndexService) DeleteByKnowledgeIDList(ctx context.Context, knowledgeIDs []string, dimension int, knowledgeType string) error {
	return s.callDelete(ctx, func(c pluginpb.RetrieverPluginClient) error {
		_, err := c.DeleteByKnowledgeIDs(ctx, &pluginpb.DeleteByKnowledgeIDsRequest{
			KnowledgeIds: knowledgeIDs, Dimension: int32(dimension), KnowledgeType: knowledgeType,
		})
		return err
	}, "delete by knowledge ids")
}

func (s *pluginIndexService) CopyIndices(ctx context.Context, sourceKBID string, sourceToTargetKBIDMap, sourceToTargetChunkIDMap map[string]string, targetKBID string, dimension int, knowledgeType string) error {
	if err := s.manager.StartOrRestart(ctx, s.pluginID, nil); err != nil {
		return err
	}
	client, err := s.manager.Connect(ctx, s.pluginID)
	if err != nil {
		return err
	}
	defer client.Close()
	_, err = pluginpb.NewRetrieverPluginClient(client.Conn()).CopyIndices(ctx, &pluginpb.CopyIndicesRequest{
		SourceKnowledgeBaseId:    sourceKBID,
		SourceToTargetKbIdMap:    sourceToTargetKBIDMap,
		SourceToTargetChunkIdMap: sourceToTargetChunkIDMap,
		TargetKnowledgeBaseId:    targetKBID,
		Dimension:                int32(dimension),
		KnowledgeType:            knowledgeType,
	})
	if err != nil {
		return fmt.Errorf("copy indices with plugin %s: %w", s.pluginID, err)
	}
	return nil
}

func (s *pluginIndexService) BatchUpdateChunkEnabledStatus(ctx context.Context, chunkStatusMap map[string]bool) error {
	if err := s.manager.StartOrRestart(ctx, s.pluginID, nil); err != nil {
		return err
	}
	client, err := s.manager.Connect(ctx, s.pluginID)
	if err != nil {
		return err
	}
	defer client.Close()
	_, err = pluginpb.NewRetrieverPluginClient(client.Conn()).UpdateChunkEnabledStatus(ctx, &pluginpb.UpdateChunkEnabledStatusRequest{
		ChunkStatusMap: chunkStatusMap,
	})
	if err != nil {
		return fmt.Errorf("update chunk enabled status with plugin %s: %w", s.pluginID, err)
	}
	return nil
}

func (s *pluginIndexService) BatchUpdateChunkTagID(ctx context.Context, chunkTagMap map[string]string) error {
	if err := s.manager.StartOrRestart(ctx, s.pluginID, nil); err != nil {
		return err
	}
	client, err := s.manager.Connect(ctx, s.pluginID)
	if err != nil {
		return err
	}
	defer client.Close()
	_, err = pluginpb.NewRetrieverPluginClient(client.Conn()).UpdateChunkTagID(ctx, &pluginpb.UpdateChunkTagIDRequest{
		ChunkTagMap: chunkTagMap,
	})
	if err != nil {
		return fmt.Errorf("update chunk tag id with plugin %s: %w", s.pluginID, err)
	}
	return nil
}

func (s *pluginIndexService) callDelete(ctx context.Context, fn func(pluginpb.RetrieverPluginClient) error, op string) error {
	if err := s.manager.StartOrRestart(ctx, s.pluginID, nil); err != nil {
		return err
	}
	client, err := s.manager.Connect(ctx, s.pluginID)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := fn(pluginpb.NewRetrieverPluginClient(client.Conn())); err != nil {
		return fmt.Errorf("%s with plugin %s: %w", op, s.pluginID, err)
	}
	return nil
}

func toIndexRecord(info *types.IndexInfo) *pluginpb.IndexRecord {
	if info == nil {
		return &pluginpb.IndexRecord{}
	}
	return &pluginpb.IndexRecord{
		Id:              info.ID,
		Content:         info.Content,
		SourceId:        info.SourceID,
		SourceType:      sourceTypeToString(info.SourceType),
		ChunkId:         info.ChunkID,
		KnowledgeId:     info.KnowledgeID,
		KnowledgeBaseId: info.KnowledgeBaseID,
		KnowledgeType:   info.KnowledgeType,
		TagId:           info.TagID,
		IsEnabled:       info.IsEnabled,
		IsRecommended:   info.IsRecommended,
	}
}

func sourceTypeToString(st types.SourceType) string {
	switch st {
	case types.PassageSourceType:
		return "passage"
	case types.SummarySourceType:
		return "summary"
	default:
		return "chunk"
	}
}

// hasIndexCapability reports whether the manifest declares the "index"
// capability, meaning the plugin implements the full index lifecycle RPCs.
func hasIndexCapability(caps []string) bool {
	for _, cap := range caps {
		if strings.TrimSpace(cap) == "index" {
			return true
		}
	}
	return false
}

// embedIndexRecords fills the host-computed embeddings into records when the
// knowledge base indexes with the vector retriever type. A nil embedder in
// that case is an error: a plugin index silently filled with text-only records
// would degrade vector recall with nothing to reveal the cause.
func embedIndexRecords(ctx context.Context, embedder embedding.Embedder, records []*pluginpb.IndexRecord, retrieverTypes []types.RetrieverType) error {
	if len(records) == 0 {
		return nil
	}
	if !slices.Contains(retrieverTypes, types.VectorRetrieverType) {
		return nil
	}
	if embedder == nil {
		return fmt.Errorf("vector indexing requires an embedder: refusing to write records without embeddings")
	}
	if len(records) == 1 {
		vector, err := embedder.Embed(ctx, sanitizeForEmbedding(ctx, records[0].Content))
		if err != nil {
			return fmt.Errorf("embed index record %s: %w", records[0].Id, err)
		}
		records[0].Embedding = vector
		return nil
	}
	contents := make([]string, 0, len(records))
	for _, record := range records {
		contents = append(contents, sanitizeForEmbedding(ctx, record.Content))
	}
	vectors, err := batchEmbedWithBackoff(ctx, embedder, contents)
	if err != nil {
		return fmt.Errorf("embed %d index records: %w", len(records), err)
	}
	if len(vectors) != len(records) {
		return fmt.Errorf("embedder returned %d vectors for %d index records", len(vectors), len(records))
	}
	for i, record := range records {
		record.Embedding = vectors[i]
	}
	return nil
}
