# WeKnora-plugin

WeKnora **主仓之外**的独立插件仓库，存放五个扩展点的实例插件：数据源、解析、网络搜索、模型、检索引擎各一个目录。

插件不复制进主仓。主仓只提供一个可选的插件挂载点，部署时通过环境变量把本仓库挂进去。

## 目录结构

按扩展类型分目录，每个插件是独立子目录（含自己的 `plugin.yaml`、`go.mod` 与构建入口）：

```text
WeKnora-plugin/
├── datasource/
│   ├── weknora-plugin-dingtalk/    # 钉钉知识库（Wiki）数据源
│   ├── weknora-plugin-github/
│   └── weknora-plugin-localdir/
├── parser/
│   └── weknora-plugin-builtinb/
├── search/
│   ├── weknora-plugin-tarily/
│   └── weknora-plugin-tarily123/
├── model/
│   └── weknora-plugin-DS/
└── retriever/
    └── weknora-plugin-milvux/
```

顶层目录名（`datasource/`、`parser/` …）刻意与宿主容器内的默认插件路径 `/app/plugins/<type>` 一致，因此**直接挂载本仓库根目录即可**，五个路径变量保持默认。

### 命名说明

插件名刻意不与宿主内置实现重名，以便外部插件与内置实现同时存在、互不覆盖：

- `milvux`（`weknora.milvux`）、`builtinb`（`example.builtin-b-parser`）：分别是内置 `milvus` 检索引擎、内置解析引擎的等价外部实现，用别名避开重名，同时验证外部插件链路真实生效。
- `tarily` / `tarily123`：同一能力（Tavily 搜索，`provider_type: TARily` 同样为避开内置 Tavily provider）的两份副本，用来验证两条运行时路径——`tarily` 的 `entrypoint` 是 `./weknora-plugin-tarily`，走 **offline / trusted 进程态**（ProcessRuntime）；`tarily123` 的 `entrypoint` 是 `docker://weknora-plugin-tarily:latest`，走 **isolated OCI 容器**（DockerRuntime），用于验证 `--network none`、egress 代理、control socket 权限移交等强隔离控制点是否有效。两者 id 不同，可同时装载。

## 宿主如何发现插件

宿主启动时按扩展类型扫描五个环境变量指向的目录，每个目录下平铺放置独立插件包，插件的扩展类型以 Manifest 的 `extension_type` 为准：

| 扩展类型 | 环境变量 | 默认容器内路径 |
| --- | --- | --- |
| datasource | `WEKNORA_PLUGIN_DIR_DATASOURCE` | `/app/plugins/datasource` |
| parser | `WEKNORA_PLUGIN_DIR_PARSER` | `/app/plugins/parser` |
| search | `WEKNORA_PLUGIN_DIR_SEARCH` | `/app/plugins/search` |
| model | `WEKNORA_PLUGIN_DIR_MODEL` | `/app/plugins/model` |
| retriever | `WEKNORA_PLUGIN_DIR_RETRIEVER` | `/app/plugins/retriever` |

注意：**没有 `WEKNORA_PLUGIN_DIRS` 这个变量**。宿主只读上表五个名字，写了别的名字不会报错，只会静默表现为「插件不被发现」。某类不需要插件时把对应变量置空（空值 = 不扫描该类型）；五个全空 = 未安装插件，宿主正常启动。

### 方式一：挂载本仓库（推荐）

在 WeKnora 主仓的 `.env` 中：

```env
WEKNORA_PLUGIN_HOST_DIR=../WeKnora-plugin   # 宿主侧：本仓库根目录（WSL 形如 /mnt/d/WeKnora-plugin）
WEKNORA_PLUGIN_MOUNT_DIR=/app/plugins       # 容器内挂载点，须与 WEKNORA_PLUGIN_DIR_* 前缀一致
```

Compose 以只读方式挂载（`${WEKNORA_PLUGIN_HOST_DIR:-./plugins}:${WEKNORA_PLUGIN_MOUNT_DIR:-/app/plugins}:ro`），容器内 `/app/plugins/<type>` 就是本仓库的同类目录。

isolated 形态还需要设置 `WEKNORA_PLUGIN_RUNTIME_AGENT_TOKEN`（app 与 plugin-runtime agent 共享的认证令牌，未设置时 Compose 直接拒绝启动）；改动 token 后须同时重建 app 与 plugin-runtime 两个容器。

### 方式二：放进主仓 `plugins/<type>/`

主仓的 `plugins/<type>/` 是另一个合法挂载点（同样只读挂到 `/app/plugins`），该目录随主仓保留了占位说明文件，未安装任何插件时也能正常启动。插件放进这里同样有效，但会造成主仓与插件仓库两份副本，一般不建议。

## 构建要求

**每个插件都必须编译出 Linux/amd64 二进制，这是它被装载的唯一形态。** 宿主跑在 WSL 或容器里，两者都是 Linux，`plugin.yaml` 的 `entrypoint` 指向的文件必须能在 Linux 上执行：

```yaml
entrypoint: ./weknora-plugin-dingtalk    # 无扩展名的 Linux 二进制
```

Windows 的 `.exe` 只是本机调试产物，部署时不会被采用（可以留在目录里，不冲突）。因此每个插件目录的部署形态是「`plugin.yaml` + 图标 + **无扩展名的 Linux 二进制**」：

```text
weknora-plugin-dingtalk/
├── plugin.yaml
├── weknora-plugin-dingtalk    # Linux/amd64 可执行文件，entrypoint 指向它
└── icon.png
```

构建方式（各插件统一，进入插件目录执行）：

```bash
cd <插件目录>          # 如 datasource/weknora-plugin-dingtalk
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o <二进制名> .
```

`CGO_ENABLED=0` 产出静态链接二进制，避免宿主容器内缺少 glibc 依赖；输出名要与 `plugin.yaml` 的 `entrypoint` 完全一致。各插件 README 里有针对自己的完整命令。

**容器不会替插件编译。** 插件目录内必须已有可执行文件，否则宿主能发现 `plugin.yaml` 但装载失败（日志中可见）。

- **ProcessRuntime（offline / trusted）**：直接用上面那个 Linux 二进制，作为 app 容器的子进程运行。
- **isolated（DockerRuntime）**：用插件目录内的 `Dockerfile` 构建镜像（镜像内同样是 Linux 二进制），并把镜像引用写入 `plugin.yaml` 的 `entrypoint`，形如 `docker://<image>:<tag>`。

本机（Windows）调试时才构建当前平台的产物，例如 `go build -o weknora-plugin-dingtalk.exe .`。
