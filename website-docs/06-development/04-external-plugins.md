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

仓库提供了可运行的 [Local Files 示例](https://github.com/Tencent/WeKnora/tree/main/examples/local-files-plugin)：

```text
examples/local-files-plugin/
├── main.go                  # gRPC datasource 实现
├── plugin.yaml              # 进程模式 manifest
├── plugin.container.yaml    # 禁网容器模式 manifest
└── Dockerfile
```

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

启动时插件管理器会发现 manifest。每个插件 ID 必须唯一；同一个目录中不能同时安装同一 ID 的进程和容器 manifest。

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
    timeoutSeconds: 5
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
- `healthCheck.timeoutSeconds` 用于启动后的健康检查。
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

## 插件协议与生命周期

公开协议定义在 `sdk/plugin/proto/plugin.proto`。插件需实现 `PluginLifecycle`；datasource 插件还需实现 `DataSourcePlugin`。Go 插件可复用 `sdk/plugin/server` 提供的监听与默认 lifecycle 实现。

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
