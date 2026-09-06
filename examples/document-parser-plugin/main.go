// Command document-parser-plugin is a Markdown/plain-text document parser
// plugin. It normalizes line endings, promotes the first heading to a top-level
// title, extracts YAML front matter into metadata, and converts plain-text
// paragraphs into minimal Markdown — all offline, with no third-party
// dependencies.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
	pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
)

const (
	engineName     = "markdown-plain"
	unsupportedMsg = "unsupported file type %q: expected md, markdown, txt, or text"
)

var supportedTypes = map[string]bool{
	"md": true, "markdown": true, "txt": true, "text": true,
}

type server struct {
	pluginsdk.Lifecycle
	pluginpb.UnimplementedDocumentParserPluginServer
}

func (s *server) Describe(context.Context, *pluginpb.DocumentParserDescribeRequest) (*pluginpb.DocumentParserDescribeResponse, error) {
	return &pluginpb.DocumentParserDescribeResponse{
		EngineName:   engineName,
		Description:  "Markdown and plain text parser: line-ending normalization, front matter extraction, heading promotion. Fully offline.",
		FileTypes:    []string{"md", "markdown", "txt", "text"},
		Capabilities: []string{},
	}, nil
}

func (s *server) Parse(_ context.Context, request *pluginpb.DocumentParserParseRequest) (*pluginpb.DocumentParserParseResponse, error) {
	fileType := strings.ToLower(strings.TrimSpace(request.GetFileType()))
	if !supportedTypes[fileType] {
		return nil, fmt.Errorf(unsupportedMsg, request.GetFileType())
	}

	content := string(request.GetFileContent())
	metadata := map[string]string{
		"source_file": request.GetFileName(),
		"bytes":       fmt.Sprintf("%d", len(request.GetFileContent())),
		"runes":       fmt.Sprintf("%d", utf8.RuneCount(request.GetFileContent())),
		"parsed_at":   time.Now().UTC().Format(time.RFC3339),
	}

	content = normalizeLineEndings(content)
	content, frontMatter := extractFrontMatter(content)
	for key, value := range frontMatter {
		metadata["frontmatter."+key] = value
	}

	if fileType == "txt" || fileType == "text" {
		content = textToMarkdown(content)
	} else {
		content = promoteFirstHeading(content)
	}
	if title := strings.TrimSpace(request.GetTitle()); title != "" {
		metadata["title_override"] = title
	}

	return &pluginpb.DocumentParserParseResponse{
		MarkdownContent: strings.TrimSpace(content),
		Metadata:        metadata,
	}, nil
}

// normalizeLineEndings converts CRLF and lone CR to LF.
func normalizeLineEndings(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return strings.ReplaceAll(content, "\r", "\n")
}

// extractFrontMatter strips a leading YAML front matter block (--- delimited)
// and returns the remaining content plus the parsed key: value pairs.
func extractFrontMatter(content string) (string, map[string]string) {
	if !strings.HasPrefix(content, "---\n") {
		return content, nil
	}
	lines := strings.SplitN(content, "\n", 2)
	rest := ""
	if len(lines) == 2 {
		rest = lines[1]
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return content, nil
	}
	frontMatter := rest[:end]
	after := rest[end+len("\n---"):]
	after = strings.TrimPrefix(after, "\n")

	parsed := make(map[string]string)
	for _, line := range strings.Split(frontMatter, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		parsed[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return after, parsed
}

// promoteFirstHeading rewrites the first ##..###### heading to `# ` so the
// document has a single top-level title.
func promoteFirstHeading(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "# ") {
			// ## or deeper (but not a lone '#'); promote to level 1.
			level := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
			if level >= 2 && level <= 6 {
				lines[i] = "# " + strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
				break
			}
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(trimmed, "#") {
			// Hit non-heading, non-blank content before any heading: stop.
			break
		}
	}
	return strings.Join(lines, "\n")
}

// textToMarkdown wraps plain-text paragraphs into Markdown, adding a title
// from the first non-empty line.
func textToMarkdown(content string) string {
	paragraphs := splitParagraphs(content)
	if len(paragraphs) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("# ")
	builder.WriteString(strings.TrimSpace(paragraphs[0]))
	builder.WriteString("\n\n")
	for _, paragraph := range paragraphs[1:] {
		builder.WriteString(paragraph)
		builder.WriteString("\n\n")
	}
	return builder.String()
}

// splitParagraphs splits on blank lines and collapses internal whitespace.
func splitParagraphs(content string) []string {
	var paragraphs []string
	for _, block := range strings.Split(content, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		paragraphs = append(paragraphs, block)
	}
	return paragraphs
}

func main() {
	implementation := &server{
		Lifecycle: pluginsdk.Lifecycle{
			Metadata: pluginsdk.Metadata{
				ID:             "com.weknora.parser-markdown-plain",
				Version:        "0.1.0",
				ExtensionTypes: []string{"document_parser"},
			},
		},
	}
	ctx, stop := pluginsdk.ContextWithSignals(context.Background())
	defer stop()
	if err := pluginsdk.ServeContext(ctx, implementation, pluginsdk.Options{
		Address:         pluginsdk.Address(),
		ShutdownTimeout: 5 * time.Second,
	}, pluginsdk.DocumentParserService(implementation)); err != nil {
		panic(fmt.Errorf("serve plugin gRPC: %w", err))
	}
}
