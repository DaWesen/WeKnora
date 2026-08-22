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

	pluginpb "github.com/Tencent/WeKnora/internal/plugin/proto"
	"google.golang.org/grpc"
)

type server struct {
	pluginpb.UnimplementedPluginLifecycleServer
	pluginpb.UnimplementedDataSourcePluginServer
}

type fileState struct {
	Hash string `json:"hash"`
}

type cursor struct {
	Files map[string]fileState `json:"files"`
}

func (s *server) GetInfo(context.Context, *pluginpb.GetInfoRequest) (*pluginpb.PluginInfo, error) {
	return &pluginpb.PluginInfo{Id: "com.weknora.local-files", Version: "0.1.0", ExtensionTypes: []string{"datasource"}}, nil
}

func (s *server) HealthCheck(context.Context, *pluginpb.HealthCheckRequest) (*pluginpb.HealthCheckResponse, error) {
	return &pluginpb.HealthCheckResponse{Status: pluginpb.HealthCheckResponse_STATUS_SERVING}, nil
}

func (s *server) ValidateConfig(_ context.Context, request *pluginpb.ValidateConfigRequest) (*pluginpb.ValidateConfigResponse, error) {
	root := request.Config["rootPath"]
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return &pluginpb.ValidateConfigResponse{Valid: false, Errors: []*pluginpb.FieldError{{Field: "rootPath", Message: "must be an existing directory"}}}, nil
	}
	return &pluginpb.ValidateConfigResponse{Valid: true}, nil
}

func (s *server) Shutdown(context.Context, *pluginpb.ShutdownRequest) (*pluginpb.ShutdownResponse, error) {
	return &pluginpb.ShutdownResponse{}, nil
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

func (s *server) Sync(request *pluginpb.SyncRequest, stream pluginpb.DataSourcePlugin_SyncServer) error {
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
		if _, exists := current.Files[key]; !exists {
			if err := stream.Send(&pluginpb.SyncEvent{Payload: &pluginpb.SyncEvent_DeleteDocument{DeleteDocument: &pluginpb.DeleteDocument{SourceId: key}}}); err != nil {
				return err
			}
		}
	}
	serialized, _ := json.Marshal(current)
	return stream.Send(&pluginpb.SyncEvent{Payload: &pluginpb.SyncEvent_Completed{Completed: &pluginpb.Completed{Cursor: string(serialized)}}})
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
	address := os.Getenv("WEKNORA_PLUGIN_GRPC_ADDRESS")
	if address == "" {
		address = "127.0.0.1:50071"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		panic(fmt.Errorf("listen plugin gRPC: %w", err))
	}
	grpcServer := grpc.NewServer()
	implementation := &server{}
	pluginpb.RegisterPluginLifecycleServer(grpcServer, implementation)
	pluginpb.RegisterDataSourcePluginServer(grpcServer, implementation)
	if err := grpcServer.Serve(listener); err != nil {
		panic(err)
	}
}
