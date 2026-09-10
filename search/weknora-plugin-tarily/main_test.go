package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
	pluginproto "github.com/Tencent/WeKnora/pkg/pluginapi/proto"
)

func handler() pluginapi.WebSearchHandler {
	return pluginapi.WebSearchHandler{
		PluginID:     "weknora.tarily",
		Capabilities: []string{"search"},
		OnSearch:     search,
	}
}

// TestHandshakeContract verifies the control-plane handshake reports the
// search extension type, the correct plugin id, and the declared capability.
func TestHandshakeContract(t *testing.T) {
	resp, err := handler().Handshake(context.Background(), &pluginproto.HandshakeRequest{})
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	var out pluginapi.HandshakeResponse
	if err := pluginapi.DecodeHandshake(resp, &out); err != nil {
		t.Fatalf("decode handshake: %v", err)
	}
	if out.ProtocolVersion != pluginapi.ProtocolVersionV1 {
		t.Errorf("protocol_version = %q, want %q", out.ProtocolVersion, pluginapi.ProtocolVersionV1)
	}
	if out.ExtensionType != "search" {
		t.Errorf("extension_type = %q, want search", out.ExtensionType)
	}
	if out.PluginID != "weknora.tarily" {
		t.Errorf("plugin_id = %q, want weknora.tarily", out.PluginID)
	}
	if len(out.Capabilities) != 1 || out.Capabilities[0] != "search" {
		t.Errorf("capabilities = %v, want [search]", out.Capabilities)
	}
}

// TestHealthContract verifies the control-plane health check returns a
// non-empty state.
func TestHealthContract(t *testing.T) {
	resp, err := handler().Health(context.Background(), &pluginproto.HealthRequest{})
	if err != nil {
		t.Fatalf("health failed: %v", err)
	}
	var out pluginapi.HealthResponse
	if err := pluginapi.DecodeHealth(resp, &out); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if out.State == "" {
		t.Error("health state should not be empty")
	}
}

// TestSearchRequiresQuery verifies an empty query is rejected before any
// network call is attempted.
func TestSearchRequiresQuery(t *testing.T) {
	_, err := search(context.Background(), pluginapi.WebSearchRequest{Query: "", APIKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("expected query error, got %v", err)
	}
}

// TestSearchRequiresAPIKey verifies a missing API key is rejected before any
// network call is attempted.
func TestSearchRequiresAPIKey(t *testing.T) {
	_, err := search(context.Background(), pluginapi.WebSearchRequest{Query: "q", APIKey: ""})
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("expected api_key error, got %v", err)
	}
}
