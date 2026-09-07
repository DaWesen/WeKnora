package docparser

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
)

// PluginLoader exposes an external document parser as a regular parser engine.
type PluginLoader struct{}

func NewPluginLoader() *PluginLoader { return &PluginLoader{} }

func (*PluginLoader) Type() plugin.ExtensionType { return plugin.ExtensionTypeDocumentParser }

func (l *PluginLoader) Load(ctx context.Context, manager *plugin.Manager, discovered plugin.Plugin) error {
	if discovered.Manifest.Spec.ExtensionType != l.Type() {
		return fmt.Errorf("expected %q plugin, got %q", l.Type(), discovered.Manifest.Spec.ExtensionType)
	}
	pluginID := discovered.Manifest.Metadata.ID
	if err := manager.StartOrRestart(ctx, pluginID, nil); err != nil {
		return fmt.Errorf("start document parser plugin %q: %w", pluginID, err)
	}
	client, err := manager.Connect(ctx, pluginID)
	if err != nil {
		return fmt.Errorf("connect document parser plugin %q: %w", pluginID, err)
	}
	defer client.Close()

	description, err := pluginpb.NewDocumentParserPluginClient(client.Conn()).Describe(ctx, &pluginpb.DocumentParserDescribeRequest{})
	if err != nil {
		return fmt.Errorf("describe document parser plugin %q: %w", pluginID, err)
	}
	name := strings.TrimSpace(description.GetEngineName())
	if name == "" {
		return fmt.Errorf("document parser plugin %q returned an empty engine name", pluginID)
	}
	if len(description.GetFileTypes()) == 0 {
		return fmt.Errorf("document parser plugin %q returned no file types", pluginID)
	}
	if err := plugin.ValidateDescribeCapabilities(discovered.Manifest.Spec.Capabilities, description.GetCapabilities()); err != nil {
		return fmt.Errorf("document parser plugin %q: %w", pluginID, err)
	}
	if _, exists := lookupEngine(name); exists {
		return fmt.Errorf("parser engine %q already registered", name)
	}

	RegisterEngine(&pluginEngine{
		manager:     manager,
		pluginID:     pluginID,
		name:         name,
		description:  description.GetDescription(),
		fileTypes:    append([]string(nil), description.GetFileTypes()...),
		supportsStream: slices.Contains(description.GetCapabilities(), "stream"),
	})
	return nil
}

type pluginEngine struct {
	manager        *plugin.Manager
	pluginID       string
	name           string
	description    string
	fileTypes      []string
	supportsStream bool
}

func (e *pluginEngine) Name() string        { return e.name }
func (e *pluginEngine) Description() string { return e.description }
func (e *pluginEngine) FileTypes(bool) []string {
	return append([]string(nil), e.fileTypes...)
}
func (e *pluginEngine) CheckAvailable(bool, map[string]string) (bool, string) {
	return true, ""
}
func (e *pluginEngine) NewReader(_ context.Context, deps ReaderDeps) (interfaces.DocReader, error) {
	return &pluginDocumentReader{
		manager:        e.manager,
		pluginID:       e.pluginID,
		config:         cloneStringMap(deps.Overrides),
		supportsStream: e.supportsStream,
	}, nil
}

type pluginDocumentReader struct {
	manager        *plugin.Manager
	pluginID       string
	config         map[string]string
	supportsStream bool
}

func (r *pluginDocumentReader) Read(ctx context.Context, request *types.ReadRequest) (*types.ReadResult, error) {
	if request == nil {
		return nil, fmt.Errorf("document read request is nil")
	}
	config := cloneStringMap(r.config)
	if err := r.manager.StartOrRestart(ctx, r.pluginID, config); err != nil {
		return nil, err
	}
	client, err := r.manager.Connect(ctx, r.pluginID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	parserClient := pluginpb.NewDocumentParserPluginClient(client.Conn())
	var response *pluginpb.DocumentParserParseResponse
	if r.supportsStream {
		response, err = r.parseStream(ctx, parserClient, config, request)
	} else {
		response, err = parserClient.Parse(ctx, &pluginpb.DocumentParserParseRequest{
			Config:      config,
			FileContent: request.FileContent,
			FileName:    request.FileName,
			FileType:    request.FileType,
			Url:         request.URL,
			Title:       request.Title,
			RequestId:   request.RequestID,
		})
		if err != nil {
			err = fmt.Errorf("parse document with plugin %s: %w", r.pluginID, err)
		}
	}
	if err != nil {
		return nil, err
	}
	images := make([]types.ImageRef, 0, len(response.Images))
	for _, image := range response.Images {
		images = append(images, types.ImageRef{
			Filename: image.Filename, OriginalRef: image.OriginalRef,
			MimeType: image.MimeType, StorageKey: image.StorageKey,
			ImageData: image.ImageData, IsOriginal: image.IsOriginal,
		})
	}
	return &types.ReadResult{
		MarkdownContent: response.MarkdownContent,
		ImageRefs:       images,
		Metadata:        response.Metadata,
		Error:           response.Error,
		IsAudio:         response.IsAudio,
		AudioData:       response.AudioData,
	}, nil
}

// parseStream uploads the document in chunks over ParseStream, falling back to
// unary Parse when the plugin does not declare the stream capability. Chunk
// size stays well below the default gRPC message limit so large documents no
// longer hit the 4MB unary ceiling.
const streamChunkSize = 1 << 20 // 1 MiB

func (r *pluginDocumentReader) parseStream(
	ctx context.Context,
	client pluginpb.DocumentParserPluginClient,
	config map[string]string,
	request *types.ReadRequest,
) (*pluginpb.DocumentParserParseResponse, error) {
	stream, err := client.ParseStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open parse stream with plugin %s: %w", r.pluginID, err)
	}
	headerSent := false
	total := len(request.FileContent)
	for offset := 0; offset < total || (!headerSent && total == 0); offset += streamChunkSize {
		end := offset + streamChunkSize
		if end > total {
			end = total
		}
		chunk := &pluginpb.DocumentParserStreamChunk{
			Data: request.FileContent[offset:end],
			Seq:  int32(offset / streamChunkSize),
			Last: end == total,
		}
		if !headerSent {
			chunk.Header = &pluginpb.DocumentParserParseRequest{
				Config: config, FileName: request.FileName, FileType: request.FileType,
				Url: request.URL, Title: request.Title, RequestId: request.RequestID,
			}
			headerSent = true
		}
		if err := stream.Send(chunk); err != nil {
			return nil, fmt.Errorf("upload chunk %d to plugin %s: %w", chunk.GetSeq(), r.pluginID, err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		return nil, fmt.Errorf("finish upload to plugin %s: %w", r.pluginID, err)
	}
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			return nil, fmt.Errorf("receive parse events from plugin %s: %w", r.pluginID, recvErr)
		}
		if result := event.GetResult(); result != nil {
			return result, nil
		}
		// Progress events are informational; keep draining until the result.
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return map[string]string{}
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
