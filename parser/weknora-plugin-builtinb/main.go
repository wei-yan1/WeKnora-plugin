package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
)

// supportedTypes 声明本插件能解析的文件类型。必须是纯文本格式——实现是把内容
// 转成 Markdown 文本，二进制/富格式（pdf/docx/xlsx/图片/音频）无法处理。
// 与 plugin.yaml 的 metadata.file_types 保持一致。
var supportedTypes = map[string]bool{
	"md":       true,
	"markdown": true,
	"txt":      true,
	"csv":      true,
	"json":     true,
}

type parserConfig struct {
	TrimWhitespace bool
	TitlePrefix    string
}

func parseConfig(overrides map[string]string) parserConfig {
	config := parserConfig{TrimWhitespace: true}
	if raw, ok := overrides["trim_whitespace"]; ok {
		config.TrimWhitespace = !strings.EqualFold(strings.TrimSpace(raw), "false")
	}
	config.TitlePrefix = overrides["title_prefix"]
	return config
}

func parse(_ context.Context, request pluginapi.ParserRequest) (pluginapi.ParserResponse, error) {
	fileType := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(request.FileType), "."))
	if !supportedTypes[fileType] {
		return pluginapi.ParserResponse{}, fmt.Errorf("unsupported file type: %s", request.FileType)
	}
	if len(request.FileContent) == 0 {
		return pluginapi.ParserResponse{}, errors.New("file_content is required")
	}

	config := parseConfig(request.ParserEngineOverrides)

	var content string
	switch fileType {
	case "csv":
		md, err := csvToMarkdown(request.FileContent)
		if err != nil {
			return pluginapi.ParserResponse{}, fmt.Errorf("csv conversion failed: %w", err)
		}
		content = md
	case "json":
		md, err := jsonToMarkdown(request.FileContent)
		if err != nil {
			return pluginapi.ParserResponse{}, fmt.Errorf("json conversion failed: %w", err)
		}
		content = md
	default: // md / markdown / txt
		content = string(request.FileContent)
	}
	if config.TrimWhitespace {
		content = strings.TrimSpace(content)
	}
	if content == "" {
		return pluginapi.ParserResponse{}, errors.New("file_content is empty")
	}
	// title_prefix 只对 Markdown 标题语义有意义，不注入到表格/代码块前面
	if config.TitlePrefix != "" && (fileType == "md" || fileType == "markdown") {
		content = addTitlePrefix(content, config.TitlePrefix)
	}

	return pluginapi.ParserResponse{
		MarkdownContent: content,
		Metadata: map[string]string{
			"engine":    "builtinb",
			"file_name": request.FileName,
		},
	}, nil
}

// csvToMarkdown 把 CSV 转成 Markdown 表格：首行为表头，后续行为数据行。
func csvToMarkdown(raw []byte) (string, error) {
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return "", err
	}
	if len(records) == 0 {
		return "", errors.New("csv file has no rows")
	}
	width := 0
	for _, row := range records {
		if len(row) > width {
			width = len(row)
		}
	}
	var b strings.Builder
	writeRow := func(row []string) {
		for i := 0; i < width; i++ {
			if i > 0 {
				b.WriteString(" | ")
			}
			if i < len(row) {
				b.WriteString(strings.ReplaceAll(row[i], "|", "\\|"))
			}
		}
		b.WriteString("\n")
	}
	writeRow(records[0])
	for i := 0; i < width; i++ {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString("---")
	}
	b.WriteString("\n")
	for _, row := range records[1:] {
		writeRow(row)
	}
	return b.String(), nil
}

// jsonToMarkdown 校验 JSON 并把内容包进 fenced code block，保证可切分/可读。
func jsonToMarkdown(raw []byte) (string, error) {
	if !json.Valid(raw) {
		return "", errors.New("invalid json content")
	}
	return "```json\n" + string(raw) + "\n```", nil
}

func addTitlePrefix(content, prefix string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "#") {
			indent := line[:len(line)-len(trimmed)]
			markerEnd := 0
			for markerEnd < len(trimmed) && trimmed[markerEnd] == '#' {
				markerEnd++
			}

			if markerEnd < len(trimmed) && trimmed[markerEnd] == ' ' {
				lines[i] = indent + trimmed[:markerEnd+1] + prefix + trimmed[markerEnd+1:]
			} else {
				lines[i] = indent + trimmed[:markerEnd] + " " + prefix + trimmed[markerEnd:]
			}
			return strings.Join(lines, "\n")
		}
	}
	return prefix + content
}

func newParserHandler() pluginapi.ParserHandler {
	return pluginapi.ParserHandler{
		PluginID:     "example.builtin-b-parser",
		Capabilities: []string{"parse"},
		OnHealth: func(context.Context) pluginapi.HealthResponse {
			return pluginapi.HealthResponse{State: "running", Message: "BuiltinB parser ready"}
		},
		OnParse: parse,
	}
}

func main() {
	address := flag.String("address", os.Getenv("WEKNORA_PLUGIN_ADDR"), "gRPC listen address")
	flag.Parse()
	if strings.TrimSpace(*address) == "" {
		return
	}

	err := pluginapi.ServeParser(context.Background(), *address, newParserHandler())
	if err != nil {
		panic(err)
	}
}
