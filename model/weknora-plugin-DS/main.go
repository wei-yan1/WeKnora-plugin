package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
)

// defaultBaseURL 与主仓 internal/models/provider.DeepSeekBaseURL 保持一致。
const defaultBaseURL = "https://api.deepseek.com/v1"

// configStore 是插件级配置缓存。文档第 6.1 节规定 base_url / api_key 是
// 插件级 config，由插件自己在进程内维护；宿主只在 ValidateConfig RPC 时
// 把它们传过来一次，后续 Chat / ChatStream 只经 gRPC metadata 注入
// ModelContext（model_id / model_name）。这里以 model_id 为主 key、model_name
// 为辅 key 缓存，并保留一份 last 作为全局兜底（DeepSeek 一个 provider 下通常
// 只有一个 base_url + api_key，deepseek-chat / deepseek-reasoner 等实例共享）。
type configStore struct {
	mu     sync.RWMutex
	byID   map[string]dsConfig
	byName map[string]dsConfig
	last   dsConfig
	has    bool
}

type dsConfig struct {
	APIKey  string
	BaseURL string
}

func newConfigStore() *configStore {
	return &configStore{
		byID:   make(map[string]dsConfig),
		byName: make(map[string]dsConfig),
	}
}

func (s *configStore) put(id, name string, cfg dsConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		s.byID[id] = cfg
	}
	if name != "" {
		s.byName[name] = cfg
	}
	s.last = cfg
	s.has = true
}

func (s *configStore) get(id, name string) (dsConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id != "" {
		if c, ok := s.byID[id]; ok {
			return c, true
		}
	}
	if name != "" {
		if c, ok := s.byName[name]; ok {
			return c, true
		}
	}
	if s.has {
		return s.last, true
	}
	return dsConfig{}, false
}

type dsModel struct {
	store *configStore
	http  *http.Client
}

// newOutboundHTTPClient 返回 SDK 的受控 HTTP 客户端：它读取宿主注入的网络策略
// （WEKNORA_PLUGIN_NETWORK_POLICY / WEKNORA_PLUGIN_NETWORK_ALLOWLIST），在出站
// 拨号前做白名单匹配与反 SSRF 校验，并对拒绝事件写审计日志。
//
// 切勿改回裸 &http.Client{}：那样 manifest 里的
// permissions.network: allowlist + allowed_destinations 会形同虚设，插件将可以
// 访问任意外网地址（模型插件指南"运行方式与网络声明"一节明确要求）。
func newOutboundHTTPClient() *http.Client {
	client := pluginapi.NewPluginHTTPClient()
	client.Timeout = 5 * time.Minute
	return client
}

func main() {
	addr := os.Getenv("WEKNORA_PLUGIN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9777"
	}

	m := &dsModel{
		store: newConfigStore(),
		http:  newOutboundHTTPClient(),
	}

	handler := pluginapi.ModelHandler{
		PluginID:         "ds.deepseek",
		Capabilities:     []string{"chat"},
		OnValidateConfig: m.validateConfig,
		OnChat:           m.chat,
		OnChatStream:     m.chatStream,
		OnHealth: func(context.Context) pluginapi.HealthResponse {
			return pluginapi.HealthResponse{State: "running", Message: "ds.deepseek ready"}
		},
	}

	if err := pluginapi.ServeModel(context.Background(), addr, handler); err != nil {
		panic(err)
	}
}

// validateConfig 校验插件级配置并缓存，供后续 Chat 调用取回。
func (m *dsModel) validateConfig(_ context.Context, cfg map[string]any) error {
	apiKey := strVal(cfg["api_key"])
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("api_key is required")
	}
	baseURL := strVal(cfg["base_url"])
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")

	id := strVal(cfg["model_id"])
	name := strVal(cfg["model_name"])
	m.store.put(id, name, dsConfig{APIKey: apiKey, BaseURL: baseURL})
	return nil
}

func (m *dsModel) resolveConfig(req pluginapi.ChatRequest) (dsConfig, error) {
	if c, ok := m.store.get(req.ModelID, req.ModelName); ok {
		return c, nil
	}
	return dsConfig{}, errors.New("plugin config not found; the model was not validated (api_key/base_url missing)")
}

// chat 实现非流式对话，走 DeepSeek OpenAI 兼容 /chat/completions。
func (m *dsModel) chat(ctx context.Context, req pluginapi.ChatRequest) (pluginapi.ChatResult, error) {
	cfg, err := m.resolveConfig(req)
	if err != nil {
		return pluginapi.ChatResult{}, err
	}
	body, err := buildRequest(req, false)
	if err != nil {
		return pluginapi.ChatResult{}, err
	}

	resp, err := m.do(ctx, cfg, body)
	if err != nil {
		return pluginapi.ChatResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return pluginapi.ChatResult{}, fmt.Errorf("deepseek api status %d: %s", resp.StatusCode, string(raw))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID   string `json:"id"`
					Type string `json:"type"`
					Fn   struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return pluginapi.ChatResult{}, fmt.Errorf("decode deepseek response: %w", err)
	}
	if len(out.Choices) == 0 {
		return pluginapi.ChatResult{}, errors.New("deepseek api returned no choices")
	}

	msg := out.Choices[0].Message
	result := pluginapi.ChatResult{
		Content:          msg.Content,
		ReasoningContent: msg.ReasoningContent,
		FinishReason:     out.Choices[0].FinishReason,
		Usage: pluginapi.TokenUsage{
			PromptTokens:     out.Usage.PromptTokens,
			CompletionTokens: out.Usage.CompletionTokens,
			TotalTokens:      out.Usage.TotalTokens,
		},
	}
	for _, tc := range msg.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, pluginapi.ChatToolCall{
			ID: tc.ID, Type: tc.Type, FunctionName: tc.Fn.Name, FunctionArguments: tc.Fn.Arguments,
		})
	}
	return result, nil
}

// chatStream 实现流式对话，逐块解析 SSE 并通过 emit 产出。reasoning_content
// 随 StreamChunk.ReasoningContent 一并回传，保证 thinking 模型在流式下不丢思考内容。
func (m *dsModel) chatStream(ctx context.Context, req pluginapi.ChatRequest, emit func(pluginapi.StreamChunk) error) error {
	cfg, err := m.resolveConfig(req)
	if err != nil {
		return err
	}
	body, err := buildRequest(req, true)
	if err != nil {
		return err
	}

	resp, err := m.do(ctx, cfg, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("deepseek api status %d: %s", resp.StatusCode, string(raw))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var usage pluginapi.TokenUsage
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var evt struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}
		if evt.Usage != nil {
			usage = pluginapi.TokenUsage{
				PromptTokens: evt.Usage.PromptTokens, CompletionTokens: evt.Usage.CompletionTokens, TotalTokens: evt.Usage.TotalTokens,
			}
		}
		if len(evt.Choices) == 0 {
			continue
		}
		delta := evt.Choices[0].Delta
		chunk := pluginapi.StreamChunk{
			Content:          delta.Content,
			ReasoningContent: delta.ReasoningContent,
			FinishReason:     evt.Choices[0].FinishReason,
		}
		if chunk.Content != "" || chunk.ReasoningContent != "" {
			if err := emit(chunk); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return emit(pluginapi.StreamChunk{Done: true, Usage: &usage})
}

func (m *dsModel) do(ctx context.Context, cfg dsConfig, body []byte) (*http.Response, error) {
	endpoint := cfg.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	return m.http.Do(req)
}

// buildRequest 构造 OpenAI 兼容请求体，保留 reasoning_content 以支持
// DeepSeek thinking 模型多轮回传。
func buildRequest(req pluginapi.ChatRequest, stream bool) ([]byte, error) {
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msg := map[string]any{
			"role":    m.Role,
			"content": m.Content,
		}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.ReasoningContent != "" {
			msg["reasoning_content"] = m.ReasoningContent
		}
		if len(m.ToolCalls) > 0 {
			tcs := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": tc.Type,
					"function": map[string]any{
						"name":      tc.FunctionName,
						"arguments": tc.FunctionArguments,
					},
				})
			}
			msg["tool_calls"] = tcs
		}
		msgs = append(msgs, msg)
	}

	model := req.ModelName
	if strings.TrimSpace(model) == "" {
		model = "deepseek-v4-pro"
	}

	payload := map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   stream,
	}
	applyOptions(payload, req.Options)

	return json.Marshal(payload)
}

func applyOptions(payload map[string]any, o pluginapi.ChatOptions) {
	if o.Temperature != 0 {
		payload["temperature"] = o.Temperature
	}
	if o.TopP != 0 {
		payload["top_p"] = o.TopP
	}
	if o.MaxTokens != 0 {
		payload["max_tokens"] = o.MaxTokens
	}
	if o.MaxCompletionTokens != 0 {
		payload["max_completion_tokens"] = o.MaxCompletionTokens
	}
	if o.Seed != 0 {
		payload["seed"] = o.Seed
	}
	if o.FrequencyPenalty != 0 {
		payload["frequency_penalty"] = o.FrequencyPenalty
	}
	if o.PresencePenalty != 0 {
		payload["presence_penalty"] = o.PresencePenalty
	}
	if len(o.Tools) > 0 {
		tools := make([]map[string]any, 0, len(o.Tools))
		for _, t := range o.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.FunctionName,
					"description": t.FunctionDescription,
					"parameters":  json.RawMessage(t.FunctionParameters),
				},
			})
		}
		payload["tools"] = tools
	}
	if o.ToolChoice != "" {
		payload["tool_choice"] = o.ToolChoice
	}
	// DeepSeek 思考模式：宿主聊天页「深度思考」开关映射为 thinking.type=enabled。
	// 仅在显式开启时下发；关闭时不传（DeepSeek 新模型默认即思考）。
	if o.Thinking {
		payload["thinking"] = map[string]any{"type": "enabled"}
	}
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
