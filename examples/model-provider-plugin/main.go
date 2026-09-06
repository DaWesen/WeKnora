// Command model-provider-plugin is a deterministic, offline model provider
// plugin. It implements Chat (echo-based streaming), Embed (feature hashing
// with L2 normalization), and Rerank (term overlap scoring) without any
// network access or API key, making it a self-contained demonstration of the
// model provider extension point and its three inference RPCs.
package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
)

const (
	providerType = "deterministic"
	embedDims    = 64
)

type server struct {
	pluginsdk.Lifecycle
	pluginpb.UnimplementedModelProviderPluginServer
}

func (s *server) Describe(context.Context, *pluginpb.ModelProviderDescribeRequest) (*pluginpb.ModelProviderDescribeResponse, error) {
	return &pluginpb.ModelProviderDescribeResponse{
		ProviderType:  providerType,
		DisplayName:  "Deterministic (Offline)",
		Description:  "Fully offline deterministic models: echo chat, feature-hash embeddings, term-overlap rerank. No API key, no network.",
		ModelTypes:   []string{"chat", "embedding", "rerank"},
		RequiresAuth: false,
		Capabilities: []string{"chat", "embedding", "rerank"},
	}, nil
}

func (s *server) ListModels(context.Context, *pluginpb.ListModelsRequest) (*pluginpb.ListModelsResponse, error) {
	return &pluginpb.ListModelsResponse{Models: []*pluginpb.PluginModel{
		{Id: "deterministic-chat", Name: "Deterministic Chat", Capabilities: []string{"chat"}},
		{Id: "deterministic-embedding", Name: "Deterministic Embedding", Capabilities: []string{"embedding"}},
		{Id: "deterministic-rerank", Name: "Deterministic Rerank", Capabilities: []string{"rerank"}},
	}}, nil
}

// Chat echoes the last user message back as a streamed reply. Content is sent
// in small word-group deltas, followed by a final usage-only chunk carrying
// finish_reason — the same shape the host adapter expects from real providers.
func (s *server) Chat(request *pluginpb.ChatRequest, stream pluginpb.ModelProviderPlugin_ChatServer) error {
	reply := buildReply(request.GetMessages())

	words := strings.Fields(reply)
	for i := 0; i < len(words); i += 3 {
		end := i + 3
		if end > len(words) {
			end = len(words)
		}
		delta := strings.Join(words[i:end], " ")
		if end != len(words) {
			delta += " "
		}
		if err := stream.Send(&pluginpb.ChatChunk{Content: delta}); err != nil {
			return err
		}
	}

	promptChars := 0
	for _, message := range request.GetMessages() {
		promptChars += len(message.GetContent())
	}
	promptTokens := promptChars / 4
	completionTokens := len(reply) / 4
	if completionTokens == 0 {
		completionTokens = 1
	}
	return stream.Send(&pluginpb.ChatChunk{
		FinishReason:     "stop",
		PromptTokens:     int32(promptTokens),
		CompletionTokens: int32(completionTokens),
		TotalTokens:      int32(promptTokens + completionTokens),
	})
}

// Embed maps each input to a deterministic unit-length vector via the hashing
// trick: every token increments or decrements one dimension chosen by its
// FNV-1a hash, then the vector is L2 normalized.
func (s *server) Embed(_ context.Context, request *pluginpb.EmbedRequest) (*pluginpb.EmbedResponse, error) {
	inputs := request.GetInputs()
	embeddings := make([]*pluginpb.Embedding, 0, len(inputs))
	for _, input := range inputs {
		embeddings = append(embeddings, &pluginpb.Embedding{Values: embedText(input)})
	}
	return &pluginpb.EmbedResponse{Embeddings: embeddings, Dimensions: embedDims}, nil
}

// Rerank scores each document by term overlap with the query and returns
// results sorted by score descending (stable for ties).
func (s *server) Rerank(_ context.Context, request *pluginpb.RerankRequest) (*pluginpb.RerankResponse, error) {
	documents := request.GetDocuments()
	results := make([]*pluginpb.RerankResult, 0, len(documents))
	for i, document := range documents {
		results = append(results, &pluginpb.RerankResult{
			Index: int32(i),
			Score: overlapScore(request.GetQuery(), document),
		})
	}
	sort.SliceStable(results, func(a, b int) bool {
		return results[a].GetScore() > results[b].GetScore()
	})
	if topN := int(request.GetTopN()); topN > 0 && topN < len(results) {
		results = results[:topN]
	}
	return &pluginpb.RerankResponse{Results: results}, nil
}

func buildReply(messages []*pluginpb.ChatMessage) string {
	lastUser := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].GetRole() == "user" {
			lastUser = messages[i].GetContent()
			break
		}
	}
	lastUser = strings.TrimSpace(lastUser)
	if lastUser == "" {
		lastUser = "(no user message)"
	}
	return fmt.Sprintf("Deterministic echo: %s", lastUser)
}

func tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func tokenSet(text string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, token := range tokenize(text) {
		set[token] = struct{}{}
	}
	return set
}

func embedText(text string) []float32 {
	vector := make([]float32, embedDims)
	for _, token := range tokenize(text) {
		hasher := fnv.New32a()
		_, _ = hasher.Write([]byte(token))
		digest := hasher.Sum32()
		sign := float32(1)
		if digest&(1<<31) != 0 {
			sign = -1
		}
		vector[digest%embedDims] += sign
	}
	norm := 0.0
	for _, value := range vector {
		norm += float64(value) * float64(value)
	}
	if norm > 0 {
		scale := float32(1 / math.Sqrt(norm))
		for i := range vector {
			vector[i] *= scale
		}
	}
	return vector
}

func overlapScore(query, document string) float64 {
	queryTokens := tokenSet(query)
	if len(queryTokens) == 0 {
		return 0
	}
	docTokens := tokenSet(document)
	overlap := 0
	for token := range queryTokens {
		if _, ok := docTokens[token]; ok {
			overlap++
		}
	}
	return float64(overlap) / float64(len(queryTokens))
}

func main() {
	implementation := &server{
		Lifecycle: pluginsdk.Lifecycle{
			Metadata: pluginsdk.Metadata{
				ID:             "com.weknora.model-provider-deterministic",
				Version:        "0.1.0",
				ExtensionTypes: []string{"model_provider"},
			},
		},
	}
	ctx, stop := pluginsdk.ContextWithSignals(context.Background())
	defer stop()
	if err := pluginsdk.ServeContext(ctx, implementation, pluginsdk.Options{
		Address:         pluginsdk.Address(),
		ShutdownTimeout: 5 * time.Second,
	}, pluginsdk.ModelProviderService(implementation)); err != nil {
		panic(fmt.Errorf("serve plugin gRPC: %w", err))
	}
}
