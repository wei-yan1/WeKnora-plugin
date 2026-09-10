package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestParseSupportsDeclaredTextTypes(t *testing.T) {
	response, err := parse(context.Background(), pluginapi.ParserRequest{
		FileContent: []byte("  # Hello\n\nworld  \n"),
		FileName:    "README.md",
		FileType:    ".MD",
		ParserEngineOverrides: map[string]string{
			"trim_whitespace": "true",
		},
	})
	if err != nil {
		t.Fatalf("parse returned error: %v", err)
	}
	if response.MarkdownContent != "# Hello\n\nworld" {
		t.Fatalf("unexpected Markdown output: %q", response.MarkdownContent)
	}
	if response.Metadata["engine"] != "builtinb" {
		t.Fatalf("unexpected engine metadata: %q", response.Metadata["engine"])
	}
}

func TestParseAppliesManifestDeclaredConfiguration(t *testing.T) {
	response, err := parse(context.Background(), pluginapi.ParserRequest{
		FileContent: []byte("# Hello\nbody"),
		FileType:    "md",
		ParserEngineOverrides: map[string]string{
			"trim_whitespace": "false",
			"title_prefix":    "[BuiltinB] ",
		},
	})
	if err != nil {
		t.Fatalf("parse returned error: %v", err)
	}
	if response.MarkdownContent != "# [BuiltinB] Hello\nbody" {
		t.Fatalf("configuration was not applied: %q", response.MarkdownContent)
	}
}

func TestParseRejectsUndeclaredType(t *testing.T) {
	_, err := parse(context.Background(), pluginapi.ParserRequest{
		FileContent: []byte("pdf bytes"),
		FileType:    "pdf",
	})
	if err == nil {
		t.Fatal("expected unsupported file type error")
	}
}

func TestParseCSVToMarkdownTable(t *testing.T) {
	response, err := parse(context.Background(), pluginapi.ParserRequest{
		FileContent: []byte("name,age\nAlice,30\nBob,25\n"),
		FileType:    "csv",
	})
	if err != nil {
		t.Fatalf("parse returned error: %v", err)
	}
	md := response.MarkdownContent
	if !strings.Contains(md, "name | age") {
		t.Fatalf("missing table header in output: %q", md)
	}
	if !strings.Contains(md, "Alice | 30") {
		t.Fatalf("missing data row in output: %q", md)
	}
	if !strings.Contains(md, "---") {
		t.Fatalf("missing table separator in output: %q", md)
	}
}

func TestParseJSONToCodeBlock(t *testing.T) {
	response, err := parse(context.Background(), pluginapi.ParserRequest{
		FileContent: []byte(`{"test": "integration", "count": 42}`),
		FileType:    "json",
	})
	if err != nil {
		t.Fatalf("parse returned error: %v", err)
	}
	md := response.MarkdownContent
	if !strings.HasPrefix(md, "```json\n") {
		t.Fatalf("expected fenced json code block, got: %q", md)
	}
	if !strings.Contains(md, "integration") {
		t.Fatalf("json content missing: %q", md)
	}
}

func TestParseRejectsInvalidJSON(t *testing.T) {
	_, err := parse(context.Background(), pluginapi.ParserRequest{
		FileContent: []byte(`{not valid json}`),
		FileType:    "json",
	})
	if err == nil {
		t.Fatal("expected invalid json error")
	}
}

func TestParseRejectsEmptyContent(t *testing.T) {
	_, err := parse(context.Background(), pluginapi.ParserRequest{
		FileContent: []byte("   \n\t"),
		FileType:    "txt",
	})
	if err == nil {
		t.Fatal("expected empty content error")
	}
}

func TestPluginGRPCConformance(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	handler := newParserHandler()
	pluginapi.RegisterPluginControlServer(server, handler)
	pluginapi.RegisterParserPluginServer(server, handler)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, listener.Addr().String(), grpc.WithBlock(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial plugin: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	report := pluginapi.RunParserConformance(
		ctx,
		pluginapi.NewPluginControlClient(conn),
		pluginapi.NewParserPluginClient(conn),
		pluginapi.ParserRequest{FileContent: []byte("# Hello"), FileName: "README.md", FileType: "md"},
	)
	if !report.HandshakeOK || !report.HealthOK || !report.ParseOK || len(report.Errors) != 0 {
		t.Fatalf("parser conformance failed: %+v", report)
	}
	if report.PluginID != "example.builtin-b-parser" {
		t.Fatalf("unexpected plugin id: %q", report.PluginID)
	}
}
