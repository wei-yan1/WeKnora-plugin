# DeepSeek (DS) 模型插件

一个验证 WeKnora Model 扩展点插件框架的最小可运行插件，接入 DeepSeek 官方
OpenAI 兼容 API。为避免与主仓内置的 `deepseek` provider 冲突，本插件使用
别名 **DS**（`provider: ds`）。

## 能力

- `extension_type: model`
- `capabilities: [chat]`（DeepSeek 仅提供对话能力）
- 支持非流式 `Chat` 与流式 `ChatStream`
- 保留 `reasoning_content` 回传（非流式 + 流式），兼容 DeepSeek thinking 模型（deepseek-reasoner）

## 构建

```bash
cd D:\weknora-plugins\model
go build -o weknora-plugin-ds .
```

## 测试

```bash
go test ./...
```

## 部署

把编译产物与 `plugin.yaml` 放在同一目录，并设置环境变量让宿主装载：

```bash
export WEKNORA_PLUGIN_DIR_MODEL=/path/to/model
```

## 配置

在 WeKnora 中新建模型时：

| 字段 | 值 |
|---|---|
| Source | plugin |
| Provider | `ds` |
| API Key | 你的 DeepSeek API Key |
| Base URL | `https://api.deepseek.com/v1`（可留空，默认此值） |
| 模型名 | `deepseek-chat` 或 `deepseek-reasoner` |

## 说明

- 宿主在保存模型时通过 `ValidateConfig` RPC 把 `api_key` / `base_url` 传一次，
  插件缓存后供后续 `Chat` / `ChatStream` 调用使用；`ModelID` / `ModelName` 经
  gRPC metadata 注入，用于取回对应配置。
- 插件无状态，可承受宿主重启：重启后下次保存/校验即重建缓存。
