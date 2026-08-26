package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
)

type server struct {
	pluginsdk.Lifecycle
	pluginpb.UnimplementedDataSourcePluginServer
}

type fileState struct {
	Hash string `json:"hash"`
}

type cursor struct {
	Files map[string]fileState `json:"files"`
}

func (s *server) ValidateConfig(_ context.Context, request *pluginpb.ValidateConfigRequest) (*pluginpb.ValidateConfigResponse, error) {
	root := request.Config["rootPath"]
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return &pluginpb.ValidateConfigResponse{Valid: false, Errors: []*pluginpb.FieldError{{Field: "rootPath", Message: "must be an existing directory"}}}, nil
	}
	return &pluginpb.ValidateConfigResponse{Valid: true}, nil
}

func (s *server) ValidateCredentials(ctx context.Context, request *pluginpb.ValidateCredentialsRequest) (*pluginpb.ValidateCredentialsResponse, error) {
	result, err := s.ValidateConfig(ctx, &pluginpb.ValidateConfigRequest{Config: request.Config})
	if err != nil {
		return nil, err
	}
	message := ""
	if !result.Valid {
		message = result.Errors[0].Message
	}
	return &pluginpb.ValidateCredentialsResponse{Valid: result.Valid, Message: message}, nil
}

func (s *server) ListResources(_ context.Context, request *pluginpb.ListResourcesRequest) (*pluginpb.ListResourcesResponse, error) {
	if request.ParentId != "" {
		return &pluginpb.ListResourcesResponse{}, nil
	}
	paths, err := scanFiles(request.Config["rootPath"])
	if err != nil {
		return nil, err
	}
	root := request.Config["rootPath"]
	resources := make([]*pluginpb.Resource, 0, len(paths))
	for _, path := range paths {
		resource, err := resourceFromPath(root, path)
		if err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	return &pluginpb.ListResourcesResponse{Resources: resources}, nil
}

func (s *server) ResolveResourceAncestors(context.Context, *pluginpb.ResolveResourceAncestorsRequest) (*pluginpb.ResolveResourceAncestorsResponse, error) {
	return &pluginpb.ResolveResourceAncestorsResponse{}, nil
}

func (s *server) FetchAll(_ context.Context, request *pluginpb.FetchAllRequest) (*pluginpb.FetchAllResponse, error) {
	paths, err := scanFiles(request.Config["rootPath"])
	if err != nil {
		return nil, err
	}
	root := request.Config["rootPath"]
	documents := make([]*pluginpb.Document, 0, len(paths))
	for _, path := range paths {
		document, err := documentFromPath(root, path, request.ResourceIds)
		if err != nil {
			return nil, err
		}
		if document != nil {
			documents = append(documents, document)
		}
	}
	return &pluginpb.FetchAllResponse{Documents: documents}, nil
}

func (s *server) Sync(request *pluginpb.SyncRequest, stream pluginpb.DataSourcePlugin_SyncServer) error {
	if err := probeNetwork(request.Config["networkProbeTarget"], stream); err != nil {
		return err
	}

	root := request.Config["rootPath"]
	previous := parseCursor(request.Cursor)
	current := cursor{Files: make(map[string]fileState)}
	paths, err := scanFiles(root)
	if err != nil {
		return err
	}
	for index, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if !isSelectedResource(key, request.ResourceIds) {
			continue
		}
		hash := contentHash(content)
		current.Files[key] = fileState{Hash: hash}
		if previous.Files[key].Hash != hash {
			if err := stream.Send(&pluginpb.SyncEvent{Payload: &pluginpb.SyncEvent_UpsertDocument{UpsertDocument: &pluginpb.UpsertDocument{SourceId: key, Name: filepath.Base(path), Content: content, ContentType: contentType(path), UpdatedAt: time.Now().UTC().Format(time.RFC3339)}}}); err != nil {
				return err
			}
		}
		if err := stream.Send(&pluginpb.SyncEvent{Payload: &pluginpb.SyncEvent_Progress{Progress: &pluginpb.Progress{Completed: uint64(index + 1), Total: uint64(len(paths))}}}); err != nil {
			return err
		}
	}
	for key := range previous.Files {
		if !isSelectedResource(key, request.ResourceIds) {
			continue
		}
		if _, exists := current.Files[key]; !exists {
			if err := stream.Send(&pluginpb.SyncEvent{Payload: &pluginpb.SyncEvent_DeleteDocument{DeleteDocument: &pluginpb.DeleteDocument{SourceId: key}}}); err != nil {
				return err
			}
		}
	}
	serialized, _ := json.Marshal(current)
	return stream.Send(&pluginpb.SyncEvent{Payload: &pluginpb.SyncEvent_Completed{Completed: &pluginpb.Completed{Cursor: string(serialized)}}})
}

func probeNetwork(target string, stream pluginpb.DataSourcePlugin_SyncServer) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	connection, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err == nil {
		return connection.Close()
	}
	return stream.Send(&pluginpb.SyncEvent{Payload: &pluginpb.SyncEvent_Error{Error: &pluginpb.SyncError{
		Code:    pluginpb.SyncErrorCode_SYNC_ERROR_CODE_SECURITY_POLICY_DENIED,
		Target:  target,
		Message: fmt.Sprintf("outbound network probe failed: %v", err),
	}}})
}

func resourceFromPath(root, path string) (*pluginpb.Resource, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	return &pluginpb.Resource{
		ExternalId: filepath.ToSlash(relative),
		Name:       filepath.Base(path),
		Type:       "file",
		ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
	}, nil
}

func documentFromPath(root, path string, resourceIDs []string) (*pluginpb.Document, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	sourceID := filepath.ToSlash(relative)
	if !isSelectedResource(sourceID, resourceIDs) {
		return nil, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &pluginpb.Document{
		SourceId:         sourceID,
		SourceResourceId: sourceID,
		Name:             filepath.Base(path),
		Content:          content,
		ContentType:      contentType(path),
		UpdatedAt:        info.ModTime().UTC().Format(time.RFC3339),
	}, nil
}

func isSelectedResource(sourceID string, resourceIDs []string) bool {
	if len(resourceIDs) == 0 {
		return true
	}
	for _, resourceID := range resourceIDs {
		if sourceID == resourceID {
			return true
		}
	}
	return false
}

func scanFiles(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && (strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".txt")) {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func parseCursor(raw string) cursor {
	result := cursor{Files: make(map[string]fileState)}
	_ = json.Unmarshal([]byte(raw), &result)
	return result
}

func contentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
func contentType(path string) string {
	if strings.HasSuffix(path, ".md") {
		return "text/markdown"
	}
	return "text/plain"
}

func main() {
	implementation := &server{
		Lifecycle: pluginsdk.Lifecycle{
			Metadata: pluginsdk.Metadata{
				ID:             "com.weknora.local-files",
				Version:        "0.1.0",
				ExtensionTypes: []string{"datasource"},
			},
		},
	}
	ctx, stop := pluginsdk.ContextWithSignals(context.Background())
	defer stop()
	if err := pluginsdk.ServeContext(ctx, implementation, pluginsdk.Options{
		Address:         pluginsdk.Address(),
		ShutdownTimeout: 5 * time.Second,
	}, pluginsdk.DataSourceService(implementation)); err != nil {
		panic(fmt.Errorf("serve plugin gRPC: %w", err))
	}
}
