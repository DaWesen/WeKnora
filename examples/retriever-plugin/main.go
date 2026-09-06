// Command retriever-plugin is an in-memory vector retriever plugin. It accepts
// host-computed embeddings through the index lifecycle RPCs (SaveIndex /
// BatchSaveIndex / Delete* / CopyIndices / Update*) and answers Retrieve with
// cosine similarity ranking. All state lives in process memory, so it doubles
// as the minimal demonstration of a full external index backend.
package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
)

const engineType = "memory-vector"

type server struct {
	pluginsdk.Lifecycle
	pluginpb.UnimplementedRetrieverPluginServer

	mu      sync.RWMutex
	records map[string][]*pluginpb.IndexRecord // knowledge base ID -> records
}

func (s *server) Describe(context.Context, *pluginpb.RetrieverDescribeRequest) (*pluginpb.RetrieverDescribeResponse, error) {
	return &pluginpb.RetrieverDescribeResponse{
		EngineType:      engineType,
		Description:     "In-memory vector retriever with cosine similarity ranking. Index state is process-local and lost on restart.",
		Capabilities:    []string{"index", "vector", "embedding"},
		RetrieverTypes:  []string{"vector"},
	}, nil
}

// --- index lifecycle ---

func (s *server) SaveIndex(_ context.Context, request *pluginpb.SaveIndexRequest) (*pluginpb.SaveIndexResponse, error) {
	record := request.GetIndex()
	if record == nil || strings.TrimSpace(record.GetId()) == "" {
		return nil, fmt.Errorf("save index requires a record with an id")
	}
	if len(record.GetEmbedding()) == 0 {
		return nil, fmt.Errorf("record %s has no embedding; the host embedder must supply vectors for vector retrieval", record.GetId())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kbID := record.GetKnowledgeBaseId()
	previous := s.records[kbID]
	for i, existing := range previous {
		if existing.GetId() == record.GetId() {
			previous[i] = record
			return &pluginpb.SaveIndexResponse{}, nil
		}
	}
	s.records[kbID] = append(previous, record)
	return &pluginpb.SaveIndexResponse{}, nil
}

func (s *server) BatchSaveIndex(_ context.Context, request *pluginpb.BatchSaveIndexRequest) (*pluginpb.BatchSaveIndexResponse, error) {
	indices := request.GetIndices()
	if len(indices) == 0 {
		return &pluginpb.BatchSaveIndexResponse{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range indices {
		if record == nil || strings.TrimSpace(record.GetId()) == "" {
			return nil, fmt.Errorf("batch save index requires records with ids")
		}
		if len(record.GetEmbedding()) == 0 {
			return nil, fmt.Errorf("record %s has no embedding; the host embedder must supply vectors for vector retrieval", record.GetId())
		}
		kbID := record.GetKnowledgeBaseId()
		replaced := false
		for i, existing := range s.records[kbID] {
			if existing.GetId() == record.GetId() {
				s.records[kbID][i] = record
				replaced = true
				break
			}
		}
		if !replaced {
			s.records[kbID] = append(s.records[kbID], record)
		}
	}
	return &pluginpb.BatchSaveIndexResponse{}, nil
}

func (s *server) DeleteByChunkIDs(_ context.Context, request *pluginpb.DeleteByChunkIDsRequest) (*pluginpb.DeleteByChunkIDsResponse, error) {
	return &pluginpb.DeleteByChunkIDsResponse{}, s.deleteWhere(request.GetKnowledgeType(), func(record *pluginpb.IndexRecord) bool {
		return containsID(request.GetChunkIds(), record.GetChunkId())
	})
}

func (s *server) DeleteBySourceIDs(_ context.Context, request *pluginpb.DeleteBySourceIDsRequest) (*pluginpb.DeleteBySourceIDsResponse, error) {
	return &pluginpb.DeleteBySourceIDsResponse{}, s.deleteWhere(request.GetKnowledgeType(), func(record *pluginpb.IndexRecord) bool {
		return containsID(request.GetSourceIds(), record.GetSourceId())
	})
}

func (s *server) DeleteByKnowledgeIDs(_ context.Context, request *pluginpb.DeleteByKnowledgeIDsRequest) (*pluginpb.DeleteByKnowledgeIDsResponse, error) {
	return &pluginpb.DeleteByKnowledgeIDsResponse{}, s.deleteWhere(request.GetKnowledgeType(), func(record *pluginpb.IndexRecord) bool {
		return containsID(request.GetKnowledgeIds(), record.GetKnowledgeId())
	})
}

// deleteWhere removes records matching the predicate. A non-empty knowledgeType
// restricts deletion to that type; otherwise all knowledge bases are swept.
func (s *server) deleteWhere(knowledgeType string, match func(*pluginpb.IndexRecord) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for kbID, records := range s.records {
		kept := records[:0]
		for _, record := range records {
			if knowledgeType != "" && record.GetKnowledgeType() != knowledgeType {
				kept = append(kept, record)
				continue
			}
			if !match(record) {
				kept = append(kept, record)
			}
		}
		if len(kept) == 0 {
			delete(s.records, kbID)
		} else {
			s.records[kbID] = kept
		}
	}
	return nil
}

func (s *server) CopyIndices(_ context.Context, request *pluginpb.CopyIndicesRequest) (*pluginpb.CopyIndicesResponse, error) {
	sourceID := request.GetSourceKnowledgeBaseId()
	if sourceID == "" {
		return nil, fmt.Errorf("copy indices requires a source knowledge base id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	source, ok := s.records[sourceID]
	if !ok {
		return &pluginpb.CopyIndicesResponse{}, nil
	}
	chunkMap := request.GetSourceToTargetChunkIdMap()
	kbMap := request.GetSourceToTargetKbIdMap()
	target := request.GetTargetKnowledgeBaseId()
	for _, record := range source {
		clone := proto.Clone(record).(*pluginpb.IndexRecord)
		if mapped, ok := chunkMap[record.GetChunkId()]; ok {
			clone.ChunkId = mapped
		}
		if mapped, ok := kbMap[record.GetKnowledgeBaseId()]; ok {
			clone.KnowledgeBaseId = mapped
		} else if target != "" {
			clone.KnowledgeBaseId = target
		}
		if clone.ChunkId != record.GetChunkId() || clone.KnowledgeBaseId != record.GetKnowledgeBaseId() {
			clone.Id = clone.KnowledgeBaseId + ":" + clone.ChunkId
		}
		s.records[clone.KnowledgeBaseId] = append(s.records[clone.KnowledgeBaseId], clone)
	}
	return &pluginpb.CopyIndicesResponse{}, nil
}

func (s *server) UpdateChunkEnabledStatus(_ context.Context, request *pluginpb.UpdateChunkEnabledStatusRequest) (*pluginpb.UpdateChunkEnabledStatusResponse, error) {
	statusMap := request.GetChunkStatusMap()
	if len(statusMap) == 0 {
		return &pluginpb.UpdateChunkEnabledStatusResponse{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, records := range s.records {
		for _, record := range records {
			if enabled, ok := statusMap[record.GetChunkId()]; ok {
				record.IsEnabled = enabled
			}
		}
	}
	return &pluginpb.UpdateChunkEnabledStatusResponse{}, nil
}

func (s *server) UpdateChunkTagID(_ context.Context, request *pluginpb.UpdateChunkTagIDRequest) (*pluginpb.UpdateChunkTagIDResponse, error) {
	tagMap := request.GetChunkTagMap()
	if len(tagMap) == 0 {
		return &pluginpb.UpdateChunkTagIDResponse{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, records := range s.records {
		for _, record := range records {
			if tag, ok := tagMap[record.GetChunkId()]; ok {
				record.TagId = tag
			}
		}
	}
	return &pluginpb.UpdateChunkTagIDResponse{}, nil
}

// --- query ---

func (s *server) Retrieve(_ context.Context, request *pluginpb.RetrieveRequest) (*pluginpb.RetrieveResponse, error) {
	queryVector := request.GetEmbedding()
	if len(queryVector) == 0 {
		return nil, fmt.Errorf("vector retrieval requires a query embedding")
	}
	topK := int(request.GetTopK())
	if topK <= 0 {
		topK = 10
	}

	s.mu.RLock()
	type candidate struct {
		record *pluginpb.IndexRecord
		score  float64
	}
	var candidates []candidate
	for _, kbID := range request.GetKnowledgeBaseIds() {
		for _, record := range s.records[kbID] {
			if !record.GetIsEnabled() {
				continue
			}
			if len(request.GetKnowledgeIds()) > 0 && !containsID(request.GetKnowledgeIds(), record.GetKnowledgeId()) {
				continue
			}
			if len(request.GetTagIds()) > 0 && !containsID(request.GetTagIds(), record.GetTagId()) {
				continue
			}
			if containsID(request.GetExcludeKnowledgeIds(), record.GetKnowledgeId()) {
				continue
			}
			if containsID(request.GetExcludeChunkIds(), record.GetChunkId()) {
				continue
			}
			score, ok := cosineSimilarity(queryVector, record.GetEmbedding())
			if !ok {
				continue
			}
			candidates = append(candidates, candidate{record: record, score: score})
		}
	}
	s.mu.RUnlock()

	sort.SliceStable(candidates, func(a, b int) bool {
		return candidates[a].score > candidates[b].score
	})
	if topK < len(candidates) {
		candidates = candidates[:topK]
	}

	results := make([]*pluginpb.RetrieveHit, 0, len(candidates))
	for _, c := range candidates {
		if request.GetThreshold() > 0 && c.score < request.GetThreshold() {
			continue
		}
		results = append(results, &pluginpb.RetrieveHit{
			Id: c.record.GetId(), Content: c.record.GetContent(),
			SourceId: c.record.GetSourceId(), SourceType: c.record.GetSourceType(),
			ChunkId: c.record.GetChunkId(), KnowledgeId: c.record.GetKnowledgeId(),
			KnowledgeBaseId: c.record.GetKnowledgeBaseId(), TagId: c.record.GetTagId(),
			Score: c.score, IsEnabled: c.record.GetIsEnabled(),
		})
	}
	return &pluginpb.RetrieveResponse{Results: results}, nil
}

// cosineSimilarity returns the cosine of the angle between two vectors. The
// bool is false when either vector is empty or zero-length, which makes the
// result undefined.
func cosineSimilarity(a, b []float32) (float64, bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0, false
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB)), true
}

func containsID(ids []string, id string) bool {
	if id == "" {
		return false
	}
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func main() {
	implementation := &server{records: make(map[string][]*pluginpb.IndexRecord)}
	implementation.Lifecycle = pluginsdk.Lifecycle{
		Metadata: pluginsdk.Metadata{
			ID:             "com.weknora.retriever-memory",
			Version:        "0.1.0",
			ExtensionTypes: []string{"retriever"},
		},
	}
	ctx, stop := pluginsdk.ContextWithSignals(context.Background())
	defer stop()
	if err := pluginsdk.ServeContext(ctx, implementation, pluginsdk.Options{
		Address:         pluginsdk.Address(),
		ShutdownTimeout: 5 * time.Second,
	}, pluginsdk.RetrieverService(implementation)); err != nil {
		panic(fmt.Errorf("serve plugin gRPC: %w", err))
	}
}
