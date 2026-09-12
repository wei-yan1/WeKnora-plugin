# GitHub 数据源插件

独立于 WeKnora 主仓的数据源插件，把 GitHub 仓库的文档/代码文件同步进 WeKnora 做 RAG 知识源。

## 构建

部署到 WSL / Docker 时要的是**无扩展名的 Linux 二进制**（`plugin.yaml` 的 `entrypoint: ./weknora-plugin-github` 指向它）；`.exe` 只用于本机调试。

```bash
cd datasource/weknora-plugin-github
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o weknora-plugin-github .
```

本机（Windows）调试：

```powershell
cd datasource/weknora-plugin-github
go build -o weknora-plugin-github.exe .
```

`go.mod` 里 `replace github.com/Tencent/WeKnora => ../../../WeKnora-fork` 指向与本仓库**同级**的主仓检出。
用相对路径而不是绝对路径（如 `D:/WeKnora-fork`），是为了在别的机器、WSL 和容器里都能直接构建——
绝对路径一旦换环境就会报 `replacement directory ... does not exist`。

`replace` 仅用于引入 `pkg/pluginapi` SDK；运行时插件只通过 gRPC 与主仓通信。

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
- `contents` 接口对 1 MiB 以上的文件不下发内联内容（`encoding: none`），只给 `download_url`。这类文件**不会被跳过**：插件把它们作为「只带 URL 的条目」交给宿主，由 WeKnora 自行下载解析（见 `item()` 与数据源指南的「URL 无内容」约定）。既不会静默丢文件，也不会因为一个超大文件让整个仓库同步失败；
- 单个文件拉取失败（网络错误、base64 解码失败、API 未给出 `download_url` 等）**不会中断整批**：插件把它作为「失败占位项」上报（无内容 + `Metadata["error"]`），宿主计入失败数并保留上一轮 cursor，下一轮增量会重试该文件；
- 文件类型白名单与内置 GitLab connector 一致（md/pdf/docx/xlsx/图片/音频等，见 `supportedFileExtensions`）。
