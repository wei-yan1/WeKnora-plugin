# DingTalk 数据源插件（钉钉知识库）

独立于 WeKnora 主仓的数据源插件，把钉钉**知识库（Wiki）**里的文档同步进 WeKnora 做 RAG 知识源。

## 目录结构

```text
weknora-plugin-dingtalk/
├── plugin.yaml                 # Manifest：能力、权限、前端元数据与配置声明
├── main.go                     # Validate / ListResources / FetchAll / FetchIncremental
├── go.mod / go.sum
├── icon.png                    # 数据源类型卡片和编辑页左上角图标
├── weknora-plugin-dingtalk     # Linux / WSL 可执行文件
├── weknora-plugin-dingtalk.exe # Windows 可执行文件
└── README.md
```

## 构建

Windows 开发环境：

```powershell
go mod tidy
go build -o weknora-plugin-dingtalk.exe .
```

用于 Linux / WSL 部署时，`entrypoint` 使用同名无后缀文件：

```powershell
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -o weknora-plugin-dingtalk .
```

`go.mod` 里 `replace github.com/Tencent/WeKnora => D:/WeKnora-fork` 指向本机主仓，
仅用于引入 `pkg/pluginapi` SDK；运行时插件只通过 gRPC 与主仓通信，不链接宿主主程序。

## 前端展示元数据

`plugin.yaml` 中的 `metadata` 已完整声明插件类型在 WeKnora 前端所需的基础信息：

- `connector_type: weknora.dingtalk`：稳定的数据源类型标识；
- `icon: icon.png`：宿主代理读取插件目录中的图标，在类型选择卡片与编辑页标题显示；
- `description`：显示在类型选择和基本信息区域；
- `docs_url`：前端的“配置文档”入口，指向钉钉开放平台文档；

数据源的**实例名称**不是 Manifest 字段：新建时宿主会用插件名称作为默认值，用户修改后保存到数据源记录中。凭证字段只由下方的 `config_schema.properties.credentials` 声明，插件不应额外定义前端专用凭证表单。

## 配置项

全部位于 `config_schema.properties.credentials`（凭证区，前端显示为密码框/凭证表单）：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `app_key` | string | 是 | 钉钉应用 AppKey（Client ID） |
| `app_secret` | string | 是 | 钉钉应用 AppSecret（Client Secret，敏感） |
| `union_id` | string | 是 | 操作人 UnionID |

## 钉钉 API 链路（已用真实凭证验证）

| 步骤 | 接口 | 状态 |
| --- | --- | --- |
| 获取 accessToken | `POST /v1.0/oauth2/accessToken` | ✅ 已验证 |
| 知识库列表 | `GET /v2.0/wiki/workspaces?operatorId={unionId}` | ✅ 已验证 |
| 节点列表 | `GET /v2.0/wiki/nodes?parentNodeId={id}&operatorId={unionId}` | ✅ 已验证 |
| 文件下载 | `POST /v1.0/storage/spaces/{spaceId}/dentries/{dentryId}/downloadInfos/query?operatorId={unionId}` | ⚠️ 需应用开通权限 |

## 必须开通的钉钉应用权限

下载文件接口需要应用开通以下权限（钉钉开放平台后台 → 应用 → 权限管理）：

- `Storage.DownloadInfo.Read`（文件下载信息读权限）
- `Wiki.Workspace.Read`（知识库读权限）
- `Wiki.Node.Read`（知识库节点读权限）

缺少 `Storage.DownloadInfo.Read` 时会报：
`Forbidden.AccessDenied.AccessTokenPermissionDenied [Storage.DownloadInfo.Read]`

## 装载与验证

1. 保持整个 `weknora-plugin-dingtalk` 目录完整（Manifest、图标和与当前系统匹配的可执行文件必须同目录），并放入插件根目录，例如 `D:\weknora-plugins\datasource\weknora-plugin-dingtalk`；
2. 设置宿主插件目录。宿主按**扩展类型**读取五个独立变量，**没有 `WEKNORA_PLUGIN_DIRS` 这个变量**——写错名字不会报错，只会表现为「插件不被发现」，排查时极易误判为 Manifest 或二进制问题。

   本机直接运行宿主（指向插件仓库里对应类型的那一层目录）：

   ```powershell
   $env:WEKNORA_PLUGIN_DIR_DATASOURCE = "D:\weknora-plugins\datasource"
   ```

   Docker Compose 部署：变量填**容器内**路径，插件仓库根目录经 `WEKNORA_PLUGIN_HOST_DIR` 只读挂载，其顶层目录名（`datasource/`、`parser/`…）与下面默认值一一对应，因此挂上去后五个变量保持默认即可：

   ```env
   WEKNORA_PLUGIN_HOST_DIR=../WeKnora-plugin   # 宿主侧：插件仓库根目录（WSL 形如 /mnt/d/WeKnora-plugin）
   WEKNORA_PLUGIN_MOUNT_DIR=/app/plugins       # 容器内挂载点，须与 WEKNORA_PLUGIN_DIR_* 前缀一致
   WEKNORA_PLUGIN_DIR_DATASOURCE=/app/plugins/datasource
   ```

   五个变量与默认容器内路径：

   | 扩展类型 | 环境变量 | 默认容器内路径 |
   | --- | --- | --- |
   | datasource | `WEKNORA_PLUGIN_DIR_DATASOURCE` | `/app/plugins/datasource` |
   | parser | `WEKNORA_PLUGIN_DIR_PARSER` | `/app/plugins/parser` |
   | search | `WEKNORA_PLUGIN_DIR_SEARCH` | `/app/plugins/search` |
   | model | `WEKNORA_PLUGIN_DIR_MODEL` | `/app/plugins/model` |
   | retriever | `WEKNORA_PLUGIN_DIR_RETRIEVER` | `/app/plugins/retriever` |

   某类不需要插件时把对应变量置空即可（空值 = 不扫描该类型）；五个变量全为空时宿主视为「未安装插件」，正常启动。

3. 启动/刷新 WeKnora，宿主发现并启动插件；
4. 前端新建数据源，选择 `DingTalk Data Source`，填写 AppKey / AppSecret / UnionID；
5. 触发同步。

## 实现说明

- `Validate`：校验凭证（获取 token + 列知识库，验证 `Wiki.Workspace.Read`）；
- `ListResources`：懒加载知识库树（`workspace` → 递归 `folder` → `file`）；
- `FetchAll`：DFS 遍历所有知识库，下载每个 `FILE` 的原始字节交给 WeKnora 解析；
- `FetchIncremental`：使用插件私有 cursor，只下载新增或变更的文件；对一次完整列举中已消失的旧文件发送 `IsDeleted: true` 删除墓碑。

`FetchedItem.ExternalID` 使用源端稳定 ID（`dingtalk:file:{nodeId}`），
`UpdatedAt` 使用节点 `modifiedTime`（RFC3339）。cursor 的字段与 Diff 规则仅由插件解释；宿主只保存并在下一次同步时原样传回 cursor。插件返回 `IsDeleted: true` 后，宿主再按数据源的 `SyncDeletions` 设置决定是否应用删除。

## 已知限制

- **钉钉在线文档（ALIDOC / `.adoc`）**：当前统一走 `storage` 下载接口；若该接口对
  在线文档不支持，需要改用钉钉文档「导出」或「块元素」接口单独处理，待开通权限后实测。
- 文件下载失败的项会跳过（不阻塞整次同步）；正式使用建议按需把跳过项改为返回 warning。
