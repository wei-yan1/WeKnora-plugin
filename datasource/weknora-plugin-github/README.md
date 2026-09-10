# GitHub 数据源插件

独立于 WeKnora 主仓的数据源插件，把 GitHub 仓库的文档/代码文件同步进 WeKnora 做 RAG 知识源。

## 构建

```powershell
go mod tidy
go build -o weknora-plugin-github.exe .
```

`go.mod` 里 `replace github.com/Tencent/WeKnora => D:/WeKnora-fork` 指向本机主仓，
仅用于引入 `pkg/pluginapi` SDK；运行时插件只通过 gRPC 与主仓通信。

## 配置

按 `config_schema` 分区：

- `settings.repositories`（string[]）：逗号分隔的仓库，格式 `owner/repo` 或 `owner/repo:branch`（branch 默认 HEAD）
- `credentials.token`（string，secret）：GitHub Personal Access Token（公开仓库可留空匿名访问，限流 60/h；私有仓库必填，限流 5000/h）

```yaml
settings:
  repositories: ["Tencent/WeKnora", "microsoft/vscode:main"]
credentials:
  token: "ghp_xxxxx"   # 可选
```

## GitHub API 链路（已用真实公开仓库验证）

| 用途 | 接口 | 状态 |
| --- | --- | --- |
| 校验凭证/仓库可达 | `GET /repos/{owner}/{repo}/commits/{branch}` | ✅ 已验证 |
| 列文件树 | `GET /repos/{owner}/{repo}/git/trees/{branch}?recursive=1` | ✅ 已验证 |
| 取文件内容 | `GET /repos/{owner}/{repo}/contents/{path}?ref={branch}` | ✅ 已验证 |
| 增量对比 | `GET /repos/{owner}/{repo}/compare/{base}...{head}` | 实现，逻辑同 GitLab |

认证：请求头 `Authorization: Bearer <token>`（token 为空则匿名）。

## 实现说明

- `Validate`：验证 credentials 里的 token（调 `GET /user`），token 为空则直接通过（公开仓库匿名可用）；不依赖 settings，保证前端 Test Connection（只发 credentials）可用；
- `ListResources`：懒加载仓库树（仓库 → 目录 → 文件，用 contents API 列目录）；
- `FetchAll`：`git/trees?recursive=1` 全量列出文件，过滤支持的扩展名，逐个 `contents` 拉内容；
- `FetchIncremental`：按仓库记录上次同步的 head commit SHA 作为 cursor；head 变化时用 `compare` 接口只拉变更文件（`removed`→`IsDeleted`，`renamed`→删旧加新），compare 失败或文件数 ≥300（GitHub 截断限制）时降级全量重扫。

## 已知限制

- `git/trees?recursive=1` 返回 `truncated=true` 时（超大仓库）会报错，建议改用较小的分支/仓库；
- `contents` 接口有 1MB 单文件上限，超大文件会拉取失败（该文件跳过）；
- 文件类型白名单与内置 GitLab connector 一致（md/pdf/docx/xlsx/图片/音频等，见 `supportedFileExtensions`）。
