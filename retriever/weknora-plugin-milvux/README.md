# Milvux — Milvus 复刻检索引擎插件

这是为验证 WeKnora 外部插件框架而制作的 **检索引擎（Retriever）扩展点插件**。它复刻主仓内建 Milvus 引擎的核心检索能力（向量 HNSW + 关键词 BM25 稀疏向量），但作为**独立进程**通过 gRPC 与宿主通信。

- **别名 `milvux`**：因为宿主机上已经存在内建 `milvus` 引擎，外部插件不能声明同名引擎类型，故起别名 `milvux`。
- 对应文档：`WeKnora/docs/plugin-development-retriever.md`

## 能力

| 能力 | 实现方式 |
|---|---|
| `vector` | Milvus HNSW 索引，COSINE metric，分数归一到 `[0,1]`（`(score+1)/2`） |
| `keywords` | Milvus BM25 Function 稀疏向量检索（`content_sparse`） |
| `filter` | 元数据多值 IN / NOT IN 过滤（识别 `exclude_` 前缀），并强制 `is_enabled == true` |

分数语义：`similarity_higher_better`（通过 `Describe` RPC 声明，宿主懒加载读取）。

## 构建

宿主跑在 WSL（Linux）时，编译 Linux 二进制：

```bash
cd weknora-plugin-milvux
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o weknora-plugin-milvux .
```

> 需要 Go 1.26+。`go.mod` 里 `replace` 指向本地 `WeKnora-fork`（`../../../WeKnora-fork`），并 pin 了 `otelgrpc v0.59.0` 以兼容 etcd v3.5.5（Milvus client 的间接依赖）。
>
> 若宿主跑在 Windows，用 `go build -o weknora-plugin-milvux.exe .`。

## 配置

在 WeKnora「向量数据库引擎」页创建 VectorStore，选择 `milvux` 引擎，填写：

| 字段 | 必填 | 说明 |
|---|---|---|
| `addr` | ✅ | Milvus 地址，如 `localhost:19530` |
| `database` | | Milvus 数据库名（空用默认库） |
| `username` / `password` | | 鉴权（空则不鉴权） |
| `collection_name` | | collection 前缀（默认 `weknora_embeddings`） |

## 启动 Milvus 依赖服务

本插件是 Milvus 的客户端，本身不存储数据，必须连接一个正在运行的 Milvus 服务（默认 `localhost:19530`）。

开发环境（docker 基础设施，Milvus 在 `docker-compose.dev.yml` 里用 `milvus` profile 隔离，默认 `make dev-start` 不会启动它）：

```powershell
docker compose -f docker-compose.dev.yml --profile milvus up -d milvus
```

生产环境（完整 compose，同样 19530 端口）：

```powershell
docker compose --profile milvus up -d milvus
```

启动后确认容器在跑、端口可达：

```powershell
docker ps | findstr milvus
Test-NetConnection localhost -Port 19530
```

> 注意 `addr` 的取值取决于宿主运行位置：本地/WSL 直接跑宿主填 `localhost:19530`（端口已映射到宿主机）；宿主跑在 docker 容器里时，要填容器网络可达的地址（如 `milvus:19530` 或 `host.docker.internal:19530`）。

## 验证

1. 确保本地 Milvus 已启动（`localhost:19530`）。
2. 把插件目录加入宿主扫描路径（`WEKNORA_PLUGIN_DIR_RETRIEVER`）。
3. 重启宿主，确认日志出现 `milvux` 插件握手/注册。
4. 在「向量数据库引擎」页创建 `milvux` 类型的 VectorStore，用「测试连接」验证连通。
5. 建知识库绑定该 VectorStore，入库文档后检索，确认向量 + 关键词两路都能召回。

## 目录结构

```
weknora-plugin-milvux/
├── plugin.yaml   # manifest：别名 milvux + config_schema + icon
├── main.go       # 插件主程序（实现 RetrieverBackend）
├── Milvus.png    # 插件图标（manifest metadata.icon 引用）
├── go.mod / go.sum
└── README.md
```
