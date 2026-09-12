package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
	pluginproto "github.com/Tencent/WeKnora/pkg/pluginapi/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// --- 单元测试 ---

func TestConfigStore(t *testing.T) {
	s := newConfigStore()
	if _, ok := s.get("", "deepseek-chat"); ok {
		t.Fatal("expected miss before put")
	}
	s.put("m1", "deepseek-chat", dsConfig{APIKey: "k1", BaseURL: "https://a"})
	// 按 model_id 精确命中
	if c, ok := s.get("m1", ""); !ok || c.APIKey != "k1" {
		t.Fatalf("expected cached config by id, got %+v ok=%v", c, ok)
	}
	// 按 model_name 命中
	if c, ok := s.get("", "deepseek-chat"); !ok || c.APIKey != "k1" {
		t.Fatalf("expected cached config by name, got %+v ok=%v", c, ok)
	}
	// last fallback: unknown model id/name falls back to last config
	s.put("m2", "deepseek-reasoner", dsConfig{APIKey: "k2", BaseURL: "https://b"})
	if c, ok := s.get("", "unknown-model"); !ok || c.APIKey != "k2" {
		t.Fatalf("expected last fallback, got %+v ok=%v", c, ok)
	}
}

func TestValidateConfig(t *testing.T) {
	m := &dsModel{store: newConfigStore()}
	if err := m.validateConfig(context.Background(), map[string]any{}); err == nil {
		t.Fatal("expected error for missing api_key")
	}
	if err := m.validateConfig(context.Background(), map[string]any{"api_key": "sk", "model_name": "deepseek-chat", "model_id": "m1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c, ok := m.store.get("m1", "deepseek-chat")
	if !ok || c.BaseURL != defaultBaseURL {
		t.Fatalf("expected default base url, got %+v ok=%v", c, ok)
	}
}

func TestBuildRequest(t *testing.T) {
	req := pluginapi.ChatRequest{
		ModelName: "deepseek-reasoner",
		Messages: []pluginapi.ChatMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "answer", ReasoningContent: "thinking"},
		},
	}
	raw, err := buildRequest(req, true)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["model"] != "deepseek-reasoner" {
		t.Fatalf("model = %v", got["model"])
	}
	if got["stream"] != true {
		t.Fatalf("stream = %v", got["stream"])
	}
	msgs := got["messages"].([]any)
	asst := msgs[1].(map[string]any)
	if asst["reasoning_content"] != "thinking" {
		t.Fatalf("reasoning_content not preserved: %v", asst["reasoning_content"])
	}
}

// --- 端到端测试 ---

// mockDeepSeek 返回一个 OpenAI 兼容的 mock 服务。
func mockDeepSeek(t *testing.T, stream bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"你好","reasoning_content":"我在思考"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			fmt.Fprint(w, s+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(`data: {"choices":[{"delta":{"reasoning_content":"我在思考"},"finish_reason":null}]}`)
		write(`data: {"choices":[{"delta":{"content":"你好"},"finish_reason":null}]}`)
		write(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
		write(`data: [DONE]`)
	}))
}

// newTestModel 构造一个指向 mock 服务的插件实例。
func newTestModel(baseURL string) *dsModel {
	m := &dsModel{store: newConfigStore(), http: &http.Client{Timeout: 10 * time.Second}}
	_ = m.validateConfig(context.Background(), map[string]any{
		"api_key":    "sk-test",
		"base_url":   baseURL,
		"model_name": "deepseek-chat",
	})
	return m
}

func TestChatDirect(t *testing.T) {
	srv := mockDeepSeek(t, false)
	defer srv.Close()
	m := newTestModel(srv.URL)

	res, err := m.chat(context.Background(), pluginapi.ChatRequest{ModelName: "deepseek-chat"})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if res.Content != "你好" {
		t.Fatalf("content = %q", res.Content)
	}
	if res.ReasoningContent != "我在思考" {
		t.Fatalf("reasoning_content = %q", res.ReasoningContent)
	}
}

func TestChatStreamDirect(t *testing.T) {
	srv := mockDeepSeek(t, true)
	defer srv.Close()
	m := newTestModel(srv.URL)

	var content, reasoning string
	err := m.chatStream(context.Background(), pluginapi.ChatRequest{ModelName: "deepseek-chat"}, func(c pluginapi.StreamChunk) error {
		content += c.Content
		reasoning += c.ReasoningContent
		return nil
	})
	if err != nil {
		t.Fatalf("chatStream: %v", err)
	}
	if content != "你好" {
		t.Fatalf("content = %q", content)
	}
	if reasoning != "我在思考" {
		t.Fatalf("reasoning_content = %q", reasoning)
	}
}

// startPluginServer 起一个真实的插件 gRPC 服务，返回地址与清理函数。
func startPluginServer(t *testing.T, srv *httptest.Server) (addr string, stop func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr = l.Addr().String()
	_ = l.Close()

	m := &dsModel{store: newConfigStore(), http: &http.Client{Timeout: 10 * time.Second}}
	handler := pluginapi.ModelHandler{
		PluginID:         "ds.deepseek",
		Capabilities:     []string{"chat"},
		OnValidateConfig: m.validateConfig,
		OnChat:           m.chat,
		OnChatStream:     m.chatStream,
		OnHealth: func(context.Context) pluginapi.HealthResponse {
			return pluginapi.HealthResponse{State: "running"}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- pluginapi.ServeModel(ctx, addr, handler) }()

	// 等 server 就绪
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return addr, func() { cancel(); <-errCh }
}

func dialPlugin(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func TestPluginEndToEnd(t *testing.T) {
	srv := mockDeepSeek(t, false)
	defer srv.Close()
	addr, stop := startPluginServer(t, srv)
	defer stop()

	conn := dialPlugin(t, addr)
	defer conn.Close()

	control := pluginapi.NewPluginControlClient(conn)
	client := pluginapi.NewModelPluginClient(conn)
	ctx := context.Background()

	// 握手
	hs, err := control.Handshake(ctx, &pluginproto.HandshakeRequest{})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if hs.GetExtensionType() != "model" || hs.GetPluginId() != "ds.deepseek" {
		t.Fatalf("unexpected handshake: %+v", hs)
	}

	// 能力
	info, err := client.ModelInfo(ctx, &pluginproto.ModelInfoRequest{})
	if err != nil {
		t.Fatalf("modelinfo: %v", err)
	}
	if len(info.GetCapabilities()) != 1 || info.GetCapabilities()[0] != "chat" {
		t.Fatalf("unexpected capabilities: %v", info.GetCapabilities())
	}

	// 校验配置
	cfg, _ := structpb.NewStruct(map[string]any{
		"model_name": "deepseek-chat",
		"base_url":   srv.URL,
		"api_key":    "sk-test",
	})
	vr, err := client.ValidateConfig(ctx, &pluginproto.ModelValidateRequest{Config: cfg})
	if err != nil || vr.GetError() != "" {
		t.Fatalf("validateconfig: err=%v resp=%+v", err, vr)
	}

	// 对话（带 ModelContext）
	callCtx := pluginapi.WithModelContext(ctx, pluginapi.ModelContext{ModelID: "m1", ModelName: "deepseek-chat"})
	res, err := client.Chat(callCtx, &pluginproto.ModelChatRequest{
		Messages: []*pluginproto.ModelChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if res.GetError() != "" {
		t.Fatalf("chat error: %s", res.GetError())
	}
	if res.GetContent() != "你好" || res.GetReasoningContent() != "我在思考" {
		t.Fatalf("unexpected chat result: content=%q reasoning=%q", res.GetContent(), res.GetReasoningContent())
	}
}

func TestPluginStreamEndToEnd(t *testing.T) {
	srv := mockDeepSeek(t, true)
	defer srv.Close()
	addr, stop := startPluginServer(t, srv)
	defer stop()

	conn := dialPlugin(t, addr)
	defer conn.Close()
	client := pluginapi.NewModelPluginClient(conn)
	ctx := context.Background()

	cfg, _ := structpb.NewStruct(map[string]any{
		"model_name": "deepseek-chat",
		"base_url":   srv.URL,
		"api_key":    "sk-test",
	})
	if vr, err := client.ValidateConfig(ctx, &pluginproto.ModelValidateRequest{Config: cfg}); err != nil || vr.GetError() != "" {
		t.Fatalf("validateconfig: err=%v resp=%+v", err, vr)
	}

	callCtx := pluginapi.WithModelContext(ctx, pluginapi.ModelContext{ModelID: "m1", ModelName: "deepseek-chat"})
	stream, err := client.ChatStream(callCtx, &pluginproto.ModelChatRequest{
		Messages: []*pluginproto.ModelChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("chatstream: %v", err)
	}
	var content, reasoning string
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			t.Fatalf("recv: %v", err)
		}
		content += chunk.GetContent()
		reasoning += chunk.GetReasoningContent()
		if chunk.GetDone() {
			break
		}
	}
	if content != "你好" {
		t.Fatalf("content = %q", content)
	}
	if reasoning != "我在思考" {
		t.Fatalf("reasoning_content lost through stream: %q", reasoning)
	}
}

// TestOutboundHTTPClientIsGuarded 回归守卫：生产路径的出站客户端必须来自 SDK 的
// NewPluginHTTPClient。network policy = none 时任何出站都应被守卫拦截；若有人把它
// 换回裸 &http.Client{}，manifest 的 allowlist 会静默失效，而断言会立刻失败。
func TestOutboundHTTPClientIsGuarded(t *testing.T) {
	t.Setenv("WEKNORA_PLUGIN_NETWORK_POLICY", "none")

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	client := newOutboundHTTPClient()
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("guarded client must block outbound requests when network policy is none")
	}
	if !strings.Contains(err.Error(), "plugin network blocked") {
		t.Fatalf("expected the SDK network guard to deny the request, got: %v", err)
	}
}
