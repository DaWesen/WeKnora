package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

type syncStream struct {
	events []*pluginpb.SyncEvent
}

func (s *syncStream) Send(event *pluginpb.SyncEvent) error {
	s.events = append(s.events, event)
	return nil
}

func (s *syncStream) Context() context.Context          { return context.Background() }
func (s *syncStream) RecvMsg(any) error                  { return nil }
func (s *syncStream) SendHeader(metadata.MD) error       { return nil }
func (s *syncStream) SendMsg(any) error                  { return nil }
func (s *syncStream) SetHeader(metadata.MD) error        { return nil }
func (s *syncStream) SetTrailer(metadata.MD)             {}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

func TestValidateConfigRejectsMissingRootPath(t *testing.T) {
	response, err := (&server{}).ValidateConfig(context.Background(), &pluginpb.ValidateConfigRequest{})
	require.NoError(t, err)
	require.False(t, response.Valid)
	require.Len(t, response.Errors, 1)
	assert.Equal(t, "rootPath", response.Errors[0].Field)
}

func TestSyncEmitsOnlyChangedFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.md": "alpha v1", "b.md": "beta v1",
	})

	first := &syncStream{}
	require.NoError(t, (&server{}).Sync(&pluginpb.SyncRequest{Config: map[string]string{"rootPath": root}}, first))
	upserts := 0
	var cursor string
	for _, event := range first.events {
		if up := event.GetUpsertDocument(); up != nil {
			upserts++
		}
		if done := event.GetCompleted(); done != nil {
			cursor = done.GetCursor()
		}
	}
	require.Equal(t, 2, upserts, "first sync must upsert both files")

	// Change one file; only it is re-synced.
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), []byte("alpha v2"), 0o644))
	second := &syncStream{}
	require.NoError(t, (&server{}).Sync(&pluginpb.SyncRequest{
		Config: map[string]string{"rootPath": root}, Cursor: cursor,
	}, second))
	upserts, deletes := 0, 0
	for _, event := range second.events {
		if event.GetUpsertDocument() != nil {
			upserts++
		}
		if event.GetDeleteDocument() != nil {
			deletes++
		}
	}
	assert.Equal(t, 1, upserts, "only the changed file is re-upserted")
	assert.Zero(t, deletes)
}

func TestSyncReportsDeletion(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"keep.md": "keep", "drop.md": "drop"})

	first := &syncStream{}
	require.NoError(t, (&server{}).Sync(&pluginpb.SyncRequest{Config: map[string]string{"rootPath": root}}, first))
	var cursor string
	for _, event := range first.events {
		if done := event.GetCompleted(); done != nil {
			cursor = done.GetCursor()
		}
	}

	require.NoError(t, os.Remove(filepath.Join(root, "drop.md")))
	second := &syncStream{}
	require.NoError(t, (&server{}).Sync(&pluginpb.SyncRequest{
		Config: map[string]string{"rootPath": root}, Cursor: cursor,
	}, second))
	deletes := 0
	for _, event := range second.events {
		if d := event.GetDeleteDocument(); d != nil {
			deletes++
			assert.Equal(t, "drop.md", d.GetSourceId())
		}
	}
	assert.Equal(t, 1, deletes)
}

func TestSyncRespectsResourceSelection(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"one.md": "1", "two.md": "2"})

	stream := &syncStream{}
	require.NoError(t, (&server{}).Sync(&pluginpb.SyncRequest{
		Config: map[string]string{"rootPath": root}, ResourceIds: []string{"two.md"},
	}, stream))
	upserts := 0
	for _, event := range stream.events {
		if up := event.GetUpsertDocument(); up != nil {
			upserts++
			assert.Equal(t, "two.md", up.GetSourceId())
		}
	}
	assert.Equal(t, 1, upserts, "only the selected resource is synced")
}
