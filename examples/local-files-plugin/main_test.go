package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pluginpb "github.com/Tencent/WeKnora/internal/plugin/proto"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type syncStream struct {
	grpc.ServerStream
	events []*pluginpb.SyncEvent
}

func (s *syncStream) Send(event *pluginpb.SyncEvent) error {
	s.events = append(s.events, event)
	return nil
}

func (s *syncStream) Context() context.Context     { return context.Background() }
func (s *syncStream) SetHeader(metadata.MD) error  { return nil }
func (s *syncStream) SendHeader(metadata.MD) error { return nil }
func (s *syncStream) SetTrailer(metadata.MD)       {}
func (s *syncStream) SendMsg(any) error            { return nil }
func (s *syncStream) RecvMsg(any) error            { return nil }

func TestSyncOnlyEmitsChangedFiles(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "first.md"), []byte("first"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "second.txt"), []byte("second"), 0o644))

	implementation := &server{}
	first := &syncStream{}
	require.NoError(t, implementation.Sync(&pluginpb.SyncRequest{Config: map[string]string{"rootPath": root}}, first))
	require.Len(t, upserts(first.events), 2)
	firstCursor := completedCursor(first.events)

	require.NoError(t, os.WriteFile(filepath.Join(root, "second.txt"), []byte("changed"), 0o644))
	second := &syncStream{}
	require.NoError(t, implementation.Sync(&pluginpb.SyncRequest{Config: map[string]string{"rootPath": root}, Cursor: firstCursor}, second))
	changed := upserts(second.events)
	require.Len(t, changed, 1)
	require.Equal(t, "second.txt", changed[0].SourceId)
}

func upserts(events []*pluginpb.SyncEvent) []*pluginpb.UpsertDocument {
	var documents []*pluginpb.UpsertDocument
	for _, event := range events {
		if document := event.GetUpsertDocument(); document != nil {
			documents = append(documents, document)
		}
	}
	return documents
}

func completedCursor(events []*pluginpb.SyncEvent) string {
	for _, event := range events {
		if completed := event.GetCompleted(); completed != nil {
			return completed.Cursor
		}
	}
	return ""
}
