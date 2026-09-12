# 外部插件开发与部署

WeKnora 外部插件用于在**不修改主仓、不中断主程序**的前提下扩展数据源、文档解析、联网搜索、模型提供者和检索器。框架采用 **Manifest + 进程间 gRPC + 可选 Docker 隔离运行时**。

本页描述运行时插件。若你需要把连接器编译进 WeKnora，可继续使用[扩展点指南](./03-extension-points)中的内建 connector 注册方式。

## 选择外部插件还是内建扩展

| 需求 | 推荐方式 | 原因 |
| --- | --- | --- |
| 独立交付、跨语言开发、隔离第三方代码 | 外部插件 | Manifest 发现、gRPC 边界、可选容器隔离 |
| 需要直接调用大量内部 Go API | 内建扩展 | 编译期类型集成更直接 |
| 大批量数据源同步 | 外部 datasource 插件 | 支持 gRPC 服务端流和 cursor |
| 独立交付且需要资源选择 | 外部 datasource 插件 | 支持资源枚举、祖先解析与按选择同步 |

外部插件不是 Go `plugin` 动态库：插件崩溃不会直接拖垮主程序，协议可以由任意支持 gRPC/Protobuf 的语言实现。

## 快速开始

仓库提供了两个可运行示例：

- [Local Files 示例](https://github.com/Tencent/WeKnora/tree/main/examples/local-files-plugin)（datasource 扩展）：

```text
examples/local-files-plugin/
├── main.go                  # gRPC datasource 实现
├── plugin.yaml              # 进程模式 manifest
├── plugin.container.yaml    # 禁网容器模式 manifest
└── Dockerfile
```

- [DuckDuckGo Search 示例](https://github.com/Tencent/WeKnora/tree/main/examples/web-search-plugin)（web_search 扩展）：基于 DuckDuckGo Instant Answer API 的真实搜索插件，无需 API key。实现 `Describe` + `Search` 两个 RPC，支持 `base_url` / `proxy_url` 配置，演示 web search 扩展的完整开发流程。
- [Deterministic Models 示例](https://github.com/Tencent/WeKnora/tree/main/examples/model-provider-plugin)（model_provider 扩展）：完全离线的确定性模型 provider——echo 流式 Chat、hashing trick 向量 Embed、词重叠评分 Rerank，无需网络与 API key。演示 model provider 三个推理 RPC（`Chat`/`Embed`/`Rerank`）的完整实现与流式协议形状。
- [Memory Vector Retriever 示例](https://github.com/Tencent/WeKnora/tree/main/examples/retriever-plugin)（retriever 扩展）：进程内内存向量检索引擎，实现完整索引生命周期全部 9 个 RPC（写入/删除/拷贝/状态更新/查询）。接收宿主计算的 embedding 并按余弦相似度召回，演示声明 `index` capability 的检索插件如何注册为完整索引后端。
- [Markdown & Plain Text Parser 示例](https://github.com/Tencent/WeKnora/tree/main/examples/document-parser-plugin)（document_parser 扩展）：Markdown/纯文本解析引擎，零第三方依赖——换行归一、YAML front matter 提取为 metadata、标题提升、纯文本分段转 Markdown。演示 document parser 扩展的完整开发流程，含 `ParseStream` 分块上传实现（声明 `stream` capability 后大文件不再受 unary 4MB 限制）。
- [Standalone Repo 示例](https://github.com/Tencent/WeKnora/tree/main/examples/standalone-repo/local-files)（独立仓库形态）：以独立 Go module 的形式实现 local-files 数据源插件，不 import 主仓任何包、仅依赖 `sdk/plugin`，并附增量同步测试。该插件同时托管为独立仓库 [weknora-plugin-local-files](https://github.com/DaWesen/weknora-plugin-local-files)（独立构建、独立测试、内嵌 SDK，不依赖主仓）。配合下方"从零构建一个插件"教程使用。

按照示例 README 构建程序或镜像，将单个插件放入插件根目录的子目录，并设置：

```bash
export WEKNORA_PLUGIN_DIR=/var/lib/weknora/plugins
```

目录布局：

```text
/var/lib/weknora/plugins/
└── local-files/
    ├── plugin.yaml
    └── local-files-plugin       # 仅进程模式需要
```

启动时插件管理器会发现 manifest。每个插件 ID 必须唯一；同一个目录中不能同时安装同一 ID 的进程和容器 manifest。容器示例的 `healthCheck` 已包含必填的 `intervalSeconds`，可以直接作为有效 Manifest 使用。

## Manifest

所有插件通过 `plugin.yaml` 声明元数据、扩展类型、运行入口、配置 schema 和权限。

```yaml
apiVersion: weknora.plugin/v1
kind: Plugin
metadata:
  id: com.example.local-files
  name: Local Files
  version: 0.1.0
  description: Sync markdown and text files.
spec:
  extensionType: datasource
  weknoraVersion: ">=0.1.0"
  entrypoint:
    type: process
    command: ["./local-files-plugin"]
    grpcAddress: "127.0.0.1:50071"
  configSchema:
    type: object
    required: [rootPath]
    properties:
      rootPath:
        type: string
  permissions:
    network:
      enabled: true
    filesystem:
      readOnly: []
  healthCheck:
    intervalSeconds: 30
    timeoutSeconds: 5
    failureThreshold: 3
  restartPolicy:
    enabled: true
    maxAttempts: 3
    windowSeconds: 300
    backoffMillis: 1000
```

### 字段约束

- `apiVersion` 当前必须为 `weknora.plugin/v1`，`kind` 必须为 `Plugin`。
- `metadata.id`、`name`、`version` 必填；ID 是运行时、审计和状态关联键。
- `extensionType` 支持 `datasource`、`document_parser`、`web_search`、`model_provider`、`retriever`。
- `weknoraVersion` 必填，用于声明兼容版本范围。
- `configSchema` 当前支持字符串字段和 `required` 的轻量 schema 校验；插件仍须在 gRPC `ValidateConfig` 中校验业务规则。
- `healthCheck` 启用运行期 gRPC 健康监测：`intervalSeconds` 必填，范围为 1–3600；`timeoutSeconds` 范围为 1–60，且不得大于检查间隔；`failureThreshold` 可选，范围为 0–10，未设置或为 0 时按 1 次连续失败处理。
- 插件启动完成后按 `intervalSeconds` 周期探测。连续失败达到阈值后，运行时将被停止、插件标记为失败，并在 `restartPolicy` 允许时进入受预算和退避约束的自动恢复；一次成功探测会清零连续失败计数。
- restart policy 启用时，`maxAttempts` 为 1–10，`windowSeconds` 为 1–3600，`backoffMillis` 为 0–60000。

## 进程与容器运行时

### 进程模式

进程入口在主机或 app 容器内部执行：

```yaml
entrypoint:
  type: process
  command: ["./my-plugin"]
  grpcAddress: "127.0.0.1:50071"
```

`command` 相对于 manifest 所在目录执行。若主程序本身运行在 Docker 中，该可执行文件必须存在于 **app 容器内**；仅把宿主文件放在插件目录并不足以保证二进制可运行。

进程插件无法被框架可靠地禁网，因此必须声明：

```yaml
permissions:
  network:
    enabled: true
```

进程模式适合本地开发或经过信任审核的插件。

### 容器模式

容器入口可应用网络、文件系统与资源限制：

```yaml
entrypoint:
  type: container
  image: registry.example.com/weknora/my-plugin:1.0.0
  grpcAddress: unix:///var/lib/weknora/plugins/my-plugin/plugin.sock
  containerGrpcAddress: unix:///run/weknora/plugin.sock
permissions:
  network:
    enabled: false
  filesystem:
    readOnly:
      - "${config.rootPath}"
```

禁网容器必须同时满足：

1. `entrypoint.type: container`；
2. `network.enabled: false`；
3. host 与 container gRPC 地址均为 `unix://`；
4. 通过共享 Unix Socket 通讯，不暴露 TCP 端口。

运行时使用 Docker 的只读文件系统、能力收缩、禁止权限提升、PID/内存/CPU 限额和受限 `/tmp`。只读目录会在真实路径解析后挂载，以减少符号链接绕过授权边界的风险。

### Wasm 运行时

wasm 入口把插件编译为 `.wasm` 模块，由宿主用 [wazero](https://wazero.io)（纯 Go、无 CGO）嵌入执行。它面向**轻量纯计算插件**——为一个 50 行的 markdown 归一化起一个进程或容器开销过重。wasm 模块与进程/容器插件共用同一份 lifecycle、身份核验、健康监测与审计链路，但**没有进程派生、没有容器开销、没有网络面**。

```yaml
entrypoint:
  type: wasm
  wasmModule: parser.wasm          # 相对插件目录的 .wasm 路径
  grpcAddress: "127.0.0.1:50081"
permissions:
  network:
    enabled: false                  # wasm 模块天然无网络，声明 true 会被 manifest 校验拒绝
```

宿主启动 wasm 插件时：用 wazero 编译 `wasmModule` 指向的 `.wasm`，实例化后用一个进程内 gRPC facade 暴露其导出函数。后续调用链与进程插件完全一致——`GetInfo` 身份核验、周期 `HealthCheck`、`Describe`/`Parse` 业务 RPC、审计事件。Loader 不感知入口类型差异。

#### 三种入口的取舍

| 插件形态 | 推荐入口 | 原因 |
| --- | --- | --- |
| 重计算、联网 I/O、模型推理 | `process` / `container` | 真隔离、完整语言运行时 |
| 需要容器级隔离（网络/能力限制） | `container` | Docker `--cap-drop ALL`、`--network none`、资源限额 |
| 纯数据变换、<1000 行、无 I/O | `wasm` | 嵌入式、无派生开销、默认禁网 |

#### 模块 ABI

模块导出四个函数，线性内存由 TinyGo wasm target 自动导出为 `memory`。所有字符串以 NUL 结尾、存活于模块内存中，宿主不释放。

| 导出 | 签名 | 返回 |
| --- | --- | --- |
| `describe` | `() -> ptr` | NUL 结尾 JSON：`{"engine_name","description","file_types","capabilities"}` |
| `input_buffer` | `() -> ptr` | 宿主写入输入字节的缓冲区地址（调用 `parse` 前） |
| `parse` | `(len) -> ptr` | 从 `input_buffer` 读取 `len` 字节解析后，返回 NUL 结尾 JSON `{"markdown_content","metadata"}` |
| `health_check` | `() -> ptr` | 状态令牌；`"serving"` 视为健康，其他映射为 NOT_SERVING |

`health_check` **可选**：未导出该函数的模块由 facade 代答 `SERVING`，保证旧模块向前兼容。

完整 wasm 插件示例见 [`examples/wasm-parser-plugin`](https://github.com/Tencent/WeKnora/tree/main/examples/wasm-parser-plugin)：用 TinyGo 编译的 markdown 换行归一化器，与 [`examples/document-parser-plugin`](https://github.com/Tencent/WeKnora/tree/main/examples/document-parser-plugin)（进程版完整 parser）形成对照。

#### 能力边界

- **默认禁网、禁文件系统**：wasm 模块只有线性内存，本迭代不提供网络或文件 host function。manifest 声明 `network.enabled: true` 会被校验拒绝——声明与实际能力一致。
- **首批只支持无状态扩展**：当前 facade 实现 `DocumentParserPlugin`；`web_search` 纯计算路径可后续补齐。datasource 的文件 I/O 与 model_provider 的长连接暂不支持 wasm 形态。
- **串行执行**：wazero 单实例串行，适合低 QPS 场景。并发请求需在 facade 层排队。

## 插件协议与生命周期

公开协议定义在 `sdk/plugin/proto/plugin.proto`。`sdk/plugin` 是独立版本化的 Go module，插件仓库通过 `go get github.com/Tencent/WeKnora/sdk/plugin` 引用，不依赖宿主模块。插件需实现 `PluginLifecycle`；datasource 插件还需实现 `DataSourcePlugin`。Go 插件可复用 `sdk/plugin/server` 提供的监听与默认 lifecycle 实现。

```text
Discover
  → Validate manifest
  → Start runtime
  → HealthCheck
  → GetInfo（ID、版本、扩展类型校验）
  → ValidateConfig / ValidateCredentials
  → Sync
  → Stop
```

`GetInfo` 返回的 ID、版本和扩展类型必须与 manifest 一致；不一致会被拒绝并记录审计事件。

### datasource 流式同步

`DataSourcePlugin.Sync` 是服务端流：

```text
SyncRequest(datasource_id, config, cursor)
  → UpsertDocument
  → DeleteDocument
  → Progress
  → Checkpoint
  → SyncError
  → Completed(cursor)
```

WeKnora 将 checkpoint/cursor 持久化。同步任务重试时从上一次成功持久化的 cursor 继续，不应在同一条流中自动重启插件并回放事件。插件应把 cursor 视为不透明状态，并保证它能稳定标识已同步内容。

`SyncError` 是业务同步错误，和 gRPC transport 失败不同。对于安全策略拒绝，使用 `SECURITY_POLICY_DENIED`，不要把凭据、token 或完整下游响应放进错误文本。

### 资源选择与全量抓取

Datasource 插件可实现以下 RPC，使现有数据源资源选择 API 无需改动即可使用：

```text
ListResources(config, parent_id) → Resource[]
ResolveResourceAncestors(config, resource_ids) → ancestor_ids[]
FetchAll(datasource_id, config, resource_ids) → Document[]
```

`Resource.external_id` 必须稳定，并与保存的 `resource_ids` 完全一致。分层资源应在根请求中返回顶层节点、在非空 `parent_id` 请求中返回直接子节点；`ResolveResourceAncestors` 返回已选节点路径上的父节点。扁平资源可在祖先解析中返回空数组。

`Document.source_resource_id` 应标识所属选择资源。流式 `SyncRequest` 同样携带 `resource_ids`，因此采用流式同步的插件必须按选择过滤 upsert 与 delete，而不能只在 `FetchAll` 中处理选择范围。

### retriever 索引写入与向量

声明 `index` capability 的 retriever 插件会注册为完整索引后端，接收全部索引生命周期 RPC（`SaveIndex`、`BatchSaveIndex`、删除、复制、状态更新）。

向量写入：知识库以 vector 检索类型建索引时，宿主会在 `IndexRecord.embedding` 中携带由宿主 embedder 计算好的向量。插件必须满足：

- Manifest 与 `Describe` 的 capabilities 都声明 `embedding`；
- 索引后端将向量与记录一起存储，供 `Retrieve` 按向量召回；
- `embedding` 缺失时不视为错误（keywords-only 索引不带向量）。

Describe 未声明 `embedding` 的插件无法注册为 vector 索引后端——这是防止旧版 SDK 插件静默写入纯文本记录的加载期校验。

### model provider 推理

声明 `chat`、`embedding` 或 `rerank` model type 的 model provider 插件会注册对应的推理能力工厂。当宿主需要创建该 provider 的模型客户端时，会优先查找 `CapabilityRegistry`，命中则返回调用插件 gRPC RPC 的适配器，而非内置 OpenAI-compatible 适配器。

三个推理 RPC：

- `Chat`（server-streaming）：宿主始终以流式调用，非流式请求由适配器内部收集全部 chunk 后聚合返回。`ChatMessage` 支持 `images` 字段（URL 或 base64 data URI），用于 VLM 多模态输入。`ChatChunk` 携带 `content`/`reasoning_content` 增量、`finish_reason` 和 usage。
- `Embed`（unary）：批量输入，返回 `Embedding[]` 和 `dimensions`。宿主在配置未指定维度时缓存响应中的 `dimensions`。
- `Rerank`（unary）：输入 query + documents，返回按 `score` 降序的 `RerankResult[]`（含原始 `index`）。

每个 RPC 携带 `map<string,string> config`，由宿主从模型配置（`api_key`/`base_url`/`model_name`/`model_id` + `ExtraConfig` + `CustomHeaders`）构建。VLM 复用 `Chat` RPC（images 在 `ChatMessage` 中传递）；ASR 目前无推理 RPC。

当前限制：`ChatRequest` 不携带 tools/tool_choice，插件无法执行 function calling；`ListModels` 协议已定义但宿主模型列表 UI 仍从 DB 读取。

### document parser 流式上传

unary `Parse` 受 gRPC 默认单条消息大小限制（4MB）。声明 `stream` capability 的插件可同时实现 `ParseStream`（双向流）：

- **上传方向**（client-streaming）：首条消息携带 `header`（完整 `DocumentParserParseRequest` 元数据），后续消息携带 `data` 分块，`seq` 从 0 递增，`last = true` 结束。宿主以 1 MiB 分块发送。
- **响应方向**（server-streaming）：任意数量的 `progress` 事件（`received_bytes`/`received_chunks`，信息性），随后**恰好一条** `result` 事件（完整 `DocumentParserParseResponse`）终止流。

宿主在加载时记录插件的 `stream` 能力位：声明则 `Read` 走 `ParseStream`，否则走 unary `Parse`——旧插件无需改动。实现要点：分块重组后与 unary 共用同一解析管线；缺 header 的首条消息会被拒绝。

## 脚手架与契约测试（weknora-plugin CLI）

`weknora-plugin` 是插件开发者工具，提供三个子命令，把"从零建插件"压缩到一条命令：

```bash
go install github.com/Tencent/WeKnora/cmd/weknora-plugin@latest
```

### init——生成插件骨架

```bash
weknora-plugin init --type document_parser --id com.example.my-parser --out ./my-parser
```

在 `--out` 目录生成四个文件：`plugin.yaml`（可通过宿主校验的最小 manifest）、`main.go`（含 `Describe` 元数据与返回 `Unimplemented` 的业务 RPC 桩，开箱即可启动）、`go.mod`、`README.md`。`--type` 支持全部五类扩展。

本地开发期可加 `--sdk-path <WeKnora 检出路径>`，生成的 `go.mod` 会带指向本地 `sdk/plugin` 的 replace（路径含空格会自动加引号）；默认生成依赖已发布模块的版本，发布插件前删除 replace 即可。其他 flag：`--module`（Go module 路径）、`--bin`（二进制名）、`--grpc-addr`（监听地址）。

### lint——校验 manifest

```bash
weknora-plugin lint plugin.yaml
```

复用宿主的 `ParseManifest` 校验（apiVersion/kind/metadata/extensionType/capability 白名单/semver 范围/entrypoint/权限/健康检查/重启策略），通过 lint 的 manifest 保证被宿主装载器接受。

注意宿主规则：**进程模式插件不能声明 `network.enabled: false`**——隔离只在容器运行时下可强制。离线插件应提供 `plugin.container.yaml`（见 document-parser-plugin 示例）。

### test-contract——启动插件并比对运行时与 manifest

```bash
weknora-plugin test-contract .
```

按 `<dir>/plugin.yaml` 启动插件进程，拨号其 gRPC 地址，然后检查：

- `GetInfo().id == manifest metadata.id`、`GetInfo().version == manifest metadata.version`；
- `GetInfo().extensionTypes` 包含 `spec.extensionType`；
- `HealthCheck` 返回 `STATUS_SERVING`；
- 有 `Describe` RPC 的四类扩展：Describe 成功，且 **Describe capabilities 必须是 manifest capabilities 的子集**（与宿主 loader 注册期同一规则）；datasource 无 Describe RPC，改测 `ValidateCredentials` 应答。

发现漂移时逐项打印 `field: manifest=... runtime=...` 并以非零码退出。Windows 上 unix socket 入口不受支持，契约测试请用 TCP `grpcAddress`。

### 修复记录（2026-09-07）

引入 lint 后发现并修复了 8 个示例 manifest 的三类问题：缺 `kind: Plugin`、缺 `metadata.version`、`healthCheck` 使用了不存在的 `interval/timeout` 字符串字段（正确为 `intervalSeconds`/`timeoutSeconds` 整数）；另修复 4 个进程模式 manifest 声明 `network.enabled: false` 与宿主"进程禁网必须容器运行时"规则冲突的问题（进程版改为 enabled: true 并注释说明，容器版保持禁网）。全部 11 个示例 manifest 现已通过 lint。

## 插件签名与信任根

宿主可以要求插件 manifest 带有发布者签名：`WEKNORA_PLUGIN_TRUSTED_KEYS` 指向一个包含 `*.pub` 公钥文件的目录（文件名即 keyId），启用后每个 `plugin.yaml` 必须带有效 `signature` 块才会被装载；**未配置该变量时签名校验完全关闭**（开发模式默认），不影响本地开发。

### 签名流程

```bash
# 1. 生成密钥对并签名（一次性引导信任根）
weknora-plugin sign --gen-key ./keys --key-id release-2026 ./my-parser/plugin.yaml
#    ./keys/release-2026.pub → 分发到宿主信任目录；.key 私钥妥善保管

# 2. 之后更新 manifest 内容后重新签名（复用私钥）
weknora-plugin sign --key ./keys/release-2026.key --key-id release-2026 ./my-parser/plugin.yaml

# 3. 宿主启用校验
export WEKNORA_PLUGIN_TRUSTED_KEYS=/etc/weknora/plugin-keys
#    将 release-2026.pub 拷入该目录即可
```

manifest 中生成的签名块：

```yaml
signature:
  algorithm: ed25519
  keyId: release-2026
  sig: <base64 的 64 字节 ed25519 签名>
```

签名覆盖 manifest 全部内容（剔除 `signature` 块本身后重新序列化再哈希），因此**任何字段被篡改都会导致验签失败**。`--key` 模式接受 32 字节原始私钥文件；`.pub` 公钥文件支持 raw 32 字节或 base64（允许一个尾换行）两种格式。

### 宿主行为

- 信任目录配置后，**未签名 / 签名不匹配 / keyId 未知 / 算法不支持**的插件在 Discover 阶段被跳过（不阻断其他插件装载），并落 `plugin.signature_invalid` 审计事件（含 manifest 路径与失败原因）；
- 信任目录不存在或为空 → 校验关闭；
- 信任目录中有格式非法的公钥文件 → 宿主启动报错（fail-fast，防止误配信任根静默放行一切）。

注意：签名保证 manifest 的**来源与完整性**，不保证插件二进制本身——进程插件仍依赖操作系统权限保护插件目录；容器镜像的完整性应由镜像签名（cosign 等）另行覆盖，这是刻意不越界的职责划分。

## 配置与权限

配置分两层校验：

1. manifest 的 `configSchema` 在启动前检查必填字段与当前支持的字段类型；
2. 插件的 `ValidateConfig` 与 `ValidateCredentials` 做目录存在性、凭据和服务可用性等领域校验。

对文件访问，使用精确的只读目录授权：

```yaml
filesystem:
  readOnly:
    - "${config.rootPath}"
```

不要用根目录、用户主目录或含凭据的父目录替代实际数据目录。容器插件的路径必须对 Docker daemon 所在主机可访问。

## 故障恢复与审计

主程序会把 gRPC `Unavailable`、`Unknown`、`Internal`、`DataLoss`，以及进程/容器异常退出视为运行时故障。`context.Canceled`、deadline、插件业务 `SyncError`、配置错误和安全策略拒绝不会被误判为 runtime crash。

后续 datasource 调用通过受控的启动/重启路径恢复，统一遵守 restart policy 的次数预算、时间窗口、退避和并发保护。配置错误不会消耗重启预算。

生命周期、安全拒绝、运行时失败与重启事件会同时写入：

- 进程内审计 ring buffer，方便即时排障；
- 应用既有 `audit_logs`，方便长期查询。

持久化审计只保留受控的插件 ID、动作、结果和 details；潜在敏感的目标地址与错误消息不会写入 durable audit。审计库短暂不可用也不会阻塞插件启动、停止、同步或恢复。

## 指标上报

插件可通过 SDK 上报运行期指标（counter / gauge / histogram），宿主在每次健康检查成功后拉取并缓存，便于运维查询"一次同步处理了多少文档、失败原因、耗时分布"等运行期数据。

**SDK 用法**（`sdk/plugin/server/metrics.go`）：

```go
registry := pluginsdk.NewMetricsRegistry()
registry.Counter("documents_parsed", map[string]string{"type": "md"}).Add(3)
registry.Gauge("queue_depth", nil).Set(7)
registry.Histogram("parse_seconds", nil).Observe(0.25)

lifecycle := &pluginsdk.Lifecycle{
    Metadata: pluginsdk.Metadata{ID: "my-parser", Version: "0.1.0", ExtensionTypes: []string{"document_parser"}},
    Metrics:  registry,  // 不设置时 GetMetrics RPC 返回 Unimplemented
}
```

**宿主行为**：

- 健康检查成功后调用 `GetMetrics` RPC，结果缓存到 `MetricsSnapshot`（最近一次采样）
- 插件返回 `codes.Unimplemented`（旧 SDK 或未设置 `Metrics`）时标记 `unavailable: true`，不视为故障
- 拉取失败只记 debug 日志，不影响插件状态——指标是 advisory
- 管理 API `GET /api/plugins/{id}/metrics` 返回缓存的快照

旧插件无需改动：不实现 `GetMetrics` 的插件由 SDK 基类返回 `Unimplemented`，宿主标记"无指标"而非报错。

## 升级与回滚

插件升级靠"停旧进程、换二进制、启新进程"，框架为此提供 last-known-good 快照与自动回滚，避免升级失败后插件不可用且无回滚路径。

**快照建立**：插件首次成功启动时，宿主拷贝当前 manifest 与 entrypoint 到插件目录下的 `.weknora/rollback/<id>/<version>/`。后续启动保留原始快照（不覆盖），确保升级链路上始终能回到 last-known-good。

**自动回滚触发**：当插件进入 `failed` 状态且重启预算（`restartPolicy.maxAttempts`）在滑动窗口内耗尽时，`scheduleAutomaticRecovery` 调用 `maybeAutoRollback`：

1. 检查是否存在快照（`HasRollbackSnapshot`）——无则放弃，保持 failed
2. 恢复备份 manifest 到插件目录
3. 停止当前（新版）进程/容器/wasm
4. 用旧 manifest 重启
5. 落 `plugin.rolled_back`（成功）或 `plugin.rollback_failed`（失败）审计

**手动触发**：`weknora plugin rollback <id>` CLI 命令规划中，当前由自动路径覆盖核心场景。

**信任边界**：签名（见"插件签名与信任根"）保证 manifest 来源与完整性，但**不覆盖插件二进制/容器镜像**——后者由发布管道保证。回滚恢复的是快照时的 manifest 与 entrypoint 路径，不验证二进制是否被篡改。

## Compose 部署

生产 Compose 会使用以下变量提供只读的 manifest 根目录：

```dotenv
WEKNORA_PLUGIN_DIR=/var/lib/weknora/plugins
WEKNORA_PLUGIN_HOST_DIR=./plugins
```

`WEKNORA_PLUGIN_HOST_DIR` 是宿主目录；Compose 将其只读挂载到容器内的 `WEKNORA_PLUGIN_DIR`。创建插件目录后重启 app，主程序会重新执行发现流程。

```bash
mkdir -p plugins/local-files
cp examples/local-files-plugin/plugin.yaml plugins/local-files/plugin.yaml
docker compose up -d app
```

默认 Compose **不会**挂载 `/var/run/docker.sock`。这是刻意的安全选择：Docker socket 常常意味着高等级宿主控制权限。容器插件部署应显式选择受控 Docker runtime、socket proxy 或独立插件执行节点，并先完成最小权限评估；不要为了启用插件而直接对公网或多租户 app 容器开放 Docker daemon。

## 从零构建一个插件（逐步教程）

本节演示如何在一个**全新目录**中从零写出一个最小可运行的 WeKnora datasource 插件，不 clone 主仓、不修改主仓代码。完整代码见 [standalone-repo 示例](https://github.com/Tencent/WeKnora/tree/main/examples/standalone-repo/local-files)，也可直接克隆独立仓库 [weknora-plugin-local-files](https://github.com/DaWesen/weknora-plugin-local-files) 作为起点。

### 第 1 步：创建模块

```bash
mkdir weknora-plugin-demo && cd weknora-plugin-demo
go mod init github.com/yourname/weknora-plugin-demo
```

在 SDK 子模块 tag 发布前，添加临时 replace 指向本地 SDK 检出；发布后删除此块即可直接 `go get`：

```bash
go mod edit -require=github.com/Tencent/WeKnora/sdk/plugin@v0.1.0 \
           -replace=github.com/Tencent/WeKnora/sdk/plugin=/path/to/WeKnora/sdk/plugin
```

### 第 2 步：实现插件

创建 `main.go`，嵌入 SDK 提供的 `Lifecycle`（自动实现 GetInfo / HealthCheck / Shutdown），并实现扩展服务的 RPC。以 datasource 为例：

```go
package main

import (
    "context"
    "os"
    "time"

    pluginpb "github.com/Tencent/WeKnora/sdk/plugin/proto"
    pluginsdk "github.com/Tencent/WeKnora/sdk/plugin/server"
)

type server struct {
    pluginsdk.Lifecycle                              // 生命周期四件套
    pluginpb.UnimplementedDataSourcePluginServer     // 只覆写需要的 RPC
}

// ValidateConfig 校验宿主传入的配置，拒绝则启动失败
func (s *server) ValidateConfig(_ context.Context, req *pluginpb.ValidateConfigRequest) (*pluginpb.ValidateConfigResponse, error) {
    if _, err := os.Stat(req.Config["rootPath"]); err != nil {
        return &pluginpb.ValidateConfigResponse{Valid: false, Errors: []*pluginpb.FieldError{
            {Field: "rootPath", Message: "must exist"},
        }}, nil
    }
    return &pluginpb.ValidateConfigResponse{Valid: true}, nil
}

// Sync 增量同步：对比 cursor 中记录的内容哈希，仅输出变更文件
func (s *server) Sync(req *pluginpb.SyncRequest, stream pluginpb.DataSourcePlugin_SyncServer) error {
    // ... 扫描目录，逐个 stream.Send(&pluginpb.SyncEvent{...}) ...
    return stream.Send(&pluginpb.SyncEvent{Payload: &pluginpb.SyncEvent_Completed{
        Completed: &pluginpb.Completed{Cursor: newCursor},
    }})
}

func main() {
    impl := &server{Lifecycle: pluginsdk.Lifecycle{
        Metadata: pluginsdk.Metadata{
            ID: "com.yourname.demo", Version: "0.1.0",
            ExtensionTypes: []string{"datasource"},   // 必须与 plugin.yaml 一致
        },
    }}
    ctx, stop := pluginsdk.ContextWithSignals(context.Background())
    defer stop()
    panic(pluginsdk.ServeContext(ctx, impl, pluginsdk.Options{
        Address: pluginsdk.Address(),               // 读 WEKNORA_PLUGIN_GRPC_ADDRESS
    }, pluginsdk.DataSourceService(impl)))
}
```

其他扩展类型同理：换嵌入 `UnimplementedWebSearchPluginServer` / `UnimplementedModelProviderPluginServer` / `UnimplementedRetrieverPluginServer` / `UnimplementedDocumentParserPluginServer`，并在 `main()` 换用对应的 `WebSearchService(...)` 等注册器。

### 第 3 步：编写 manifest

创建 `plugin.yaml`（字段含义见上文 Manifest 一节）：

```yaml
apiVersion: weknora.plugin/v1
kind: Plugin
metadata:
  id: com.yourname.demo            # 与 GetInfo 返回的 ID 一致
  name: Demo Datasource
  version: 0.1.0
spec:
  extensionType: datasource
  weknoraVersion: ">=0.1.0"
  capabilities: [sync]
  entrypoint:
    type: process
    command: ["./weknora-plugin-demo"]
    grpcAddress: "unix:///tmp/weknora-demo.sock"
  configSchema:
    type: object
    required: [rootPath]
    properties:
      rootPath: { type: string }
  permissions:
    # 进程模式不能声明禁网：宿主只在容器运行时（container）下强制网络隔离，
    # 进程插件声明 network.enabled: false 会被 lint / 装载器拒绝。
    # 需要离线且强制隔离时改用容器形态，参考 plugin.container.yaml 示例。
    network: { enabled: true }
    filesystem: { readOnly: [] }
```

### 第 4 步：构建并安装

```bash
go build -o weknora-plugin-demo .

# 安装到插件目录（目录名任意，一个子目录一个插件）
mkdir -p /var/lib/weknora/plugins/demo
cp plugin.yaml weknora-plugin-demo /var/lib/weknora/plugins/demo/

# 启动宿主
export WEKNORA_PLUGIN_DIR=/var/lib/weknora/plugins
./weknora
```

宿主日志出现 `discovered 1 external plugins` 即装载成功。在管理界面创建数据源时会出现你的插件类型。

### 第 5 步：验证增量语义

课题验收要求"源端仅变更一个文件时，只有该文件被重新处理"。在 `Sync` 中实现内容哈希对比并把全量状态序列化进 cursor，即可满足：

```go
type fileState struct{ Hash string }
type syncCursor struct{ Files map[string]fileState }
// 下次同步时 parseCursor(req.Cursor) 取回上次状态，
// 哈希未变的文件跳过 upsert，消失的文件发 DeleteDocument。
```

standalone-repo 示例的 `main_test.go` 展示了完整断言写法。

## 开发检查清单

- [ ] manifest 的 ID、版本、扩展类型与 `GetInfo` 一致；
- [ ] 启动后 `HealthCheck` 返回 serving；
- [ ] 对输入配置实现领域校验；
- [ ] 同步支持幂等 upsert、删除和稳定 cursor；
- [ ] 错误不会泄漏 token、凭据或未脱敏的用户内容；
- [ ] 容器插件仅声明实际需要的网络和只读目录；
- [ ] 禁网容器使用 Unix Socket，不暴露 TCP listener；
- [ ] 覆盖首次同步、增量、删除、故障恢复和权限拒绝测试；
- [ ] 在目标部署环境验证 Docker runtime 权限，而不是依赖开发机配置。
