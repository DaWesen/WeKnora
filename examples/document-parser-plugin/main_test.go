package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"unicode/utf8"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// startTestServer starts the plugin's gRPC server on an ephemeral port and
// returns a gRPC client. It mirrors how the host connects to a plugin.
func startTestServer(t *testing.T) (pluginpb.DocumentParserPluginClient, func()) {
	t.Helper()
	impl := &server{
		Lifecycle: pluginsdk.Lifecycle{
			Metadata: pluginsdk.Metadata{
				ID:             "com.weknora.parser-markdown-plain",
				Version:        "0.1.0",
				ExtensionTypes: []string{"document_parser"},
			},
		},
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := pluginsdk.New(impl, pluginsdk.DocumentParserService(impl))
	go grpcServer.Serve(listener)
	conn, err := grpc.NewClient(
		listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	cleanup := func() { conn.Close(); grpcServer.GracefulStop() }
	return pluginpb.NewDocumentParserPluginClient(conn), cleanup
}

func TestDescribeDeclaresMarkdownEngine(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Describe(context.Background(), &pluginpb.DocumentParserDescribeRequest{})
	require.NoError(t, err)
	assert.Equal(t, "markdown-plain", response.GetEngineName())
	assert.Contains(t, response.GetFileTypes(), "md")
	assert.Contains(t, response.GetFileTypes(), "txt")
}

func TestParseRejectsUnsupportedFileType(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	_, err := client.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileType: "pdf", FileContent: []byte("whatever"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported file type")
}

func TestParseMarkdownWithFrontMatterAndHeadingPromotion(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	input := "---\ntitle: My Notes\nauthor: \"Alice\"\ntags: demo\n---\n## Section Title\n\nBody text here.\n"
	response, err := client.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileName: "notes.md", FileType: "md", FileContent: []byte(input),
	})
	require.NoError(t, err)
	assert.Empty(t, response.GetError())

	// Front matter stripped and surfaced as metadata.
	assert.NotContains(t, response.GetMarkdownContent(), "My Notes\n")
	assert.Equal(t, "My Notes", response.GetMetadata()["frontmatter.title"])
	assert.Equal(t, "Alice", response.GetMetadata()["frontmatter.author"])
	assert.Equal(t, "demo", response.GetMetadata()["frontmatter.tags"])

	// First heading promoted to level 1.
	assert.Contains(t, response.GetMarkdownContent(), "# Section Title")
	assert.NotContains(t, response.GetMarkdownContent(), "## Section Title")

	// Source bookkeeping.
	assert.Equal(t, "notes.md", response.GetMetadata()["source_file"])
	assert.Equal(t, fmt.Sprintf("%d", len(input)), response.GetMetadata()["bytes"])
}

func TestParseNormalizesLineEndings(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	input := "## Title\r\n\r\nWindows content.\rMore.\n"
	response, err := client.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileName: "win.md", FileType: "markdown", FileContent: []byte(input),
	})
	require.NoError(t, err)
	assert.NotContains(t, response.GetMarkdownContent(), "\r")
	assert.Contains(t, response.GetMarkdownContent(), "Windows content.\nMore.")
	assert.Contains(t, response.GetMarkdownContent(), "# Title")
}

func TestParsePlainTextBecomesMarkdown(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	input := "My Document Title\n\nFirst paragraph.\nStill first.\n\nSecond paragraph.\n"
	response, err := client.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileName: "readme.txt", FileType: "txt", FileContent: []byte(input),
	})
	require.NoError(t, err)

	content := response.GetMarkdownContent()
	assert.True(t, startsWith(content, "# My Document Title"), "first line becomes the title, got: %q", content)
	assert.Contains(t, content, "First paragraph.\nStill first.")
	assert.Contains(t, content, "Second paragraph.")
}

func TestParseUnicodeContent(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	input := "## 中文标题\n\n正文内容，包含 emoji 🎉。\n"
	response, err := client.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileName: "zh.md", FileType: "md", FileContent: []byte(input),
	})
	require.NoError(t, err)
	assert.Contains(t, response.GetMarkdownContent(), "# 中文标题")
	assert.Contains(t, response.GetMarkdownContent(), "正文内容，包含 emoji 🎉。")
	runes := fmt.Sprintf("%d", utf8.RuneCountInString(input))
	assert.Equal(t, runes, response.GetMetadata()["runes"])
}

func TestParseEmptyMarkdown(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileName: "empty.md", FileType: "md", FileContent: nil,
	})
	require.NoError(t, err)
	assert.Empty(t, response.GetMarkdownContent())
}

func TestParseFrontMatterOnly(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	input := "---\ntitle: Only Meta\n---\n"
	response, err := client.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileName: "meta.md", FileType: "md", FileContent: []byte(input),
	})
	require.NoError(t, err)
	assert.Equal(t, "Only Meta", response.GetMetadata()["frontmatter.title"])
	assert.Empty(t, response.GetMarkdownContent())
}

func TestParseUnterminatedFrontMatterTreatedAsContent(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	// A leading "---" without a closing delimiter stays as content.
	input := "---\ntitle: dangling\nno closing\n"
	response, err := client.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileName: "dangling.md", FileType: "md", FileContent: []byte(input),
	})
	require.NoError(t, err)
	assert.NotContains(t, response.GetMetadata(), "frontmatter.title")
	assert.Contains(t, response.GetMarkdownContent(), "title: dangling")
}

func TestParseTitleOverrideRecordedInMetadata(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	response, err := client.Parse(context.Background(), &pluginpb.DocumentParserParseRequest{
		FileName: "a.md", FileType: "md",
		FileContent: []byte("## Heading\n\nBody.\n"), Title: "Custom Title",
	})
	require.NoError(t, err)
	assert.Equal(t, "Custom Title", response.GetMetadata()["title_override"])
}

func startsWith(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }
