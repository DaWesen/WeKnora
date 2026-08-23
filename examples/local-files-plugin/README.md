# Local Files 外部插件示例

`local-files-plugin` 是一个可运行的 datasource 外部插件。它通过 gRPC 把指定目录中的 Markdown（`.md`）和纯文本（`.txt`）同步到 WeKnora，并使用 cursor 实现增量同步。

该示例用于演示外部插件框架的完整交互：Manifest 发现、进程或容器运行时、健康检查、插件身份校验、服务端流同步、checkpoint/cursor，以及容器权限隔离。

## 能力与边界

- 首次同步：为目录中全部 `.md`、`.txt` 文件发送 `UpsertDocument`。
- 增量同步：只为内容哈希变化的文件发送 `UpsertDocument`。
- 删除同步：cursor 中存在但本轮扫描不到的文件发送 `DeleteDocument`。
- 资源排序：按相对路径稳定排序，便于测试和排障。
- 文件范围：递归扫描配置目录；其他扩展名会被忽略。
- 资源选择：实现平铺文件资源枚举、按资源选择的 `FetchAll`，以及流式同步中的资源过滤。

文件的相对路径是稳定的 `sourceId`。例如 `docs/guide.md` 被同步为 `sourceId=docs/guide.md`。

## 前置条件

- 使用进程模式：Go 1.24+，并在 WeKnora 仓库根目录执行构建命令。
- 使用容器模式：Linux Docker Engine，并能构建或拉取插件镜像。
- WeKnora 主程序已启用外部插件框架，且插件目录由 `WEKNORA_PLUGIN_DIR` 指向。

> 此 Dockerfile 会复制仓库内的 `internal/plugin/proto`，因此本示例当前是 WeKnora 仓库内示例，而不是独立发布的 SDK 模板。

## 运行模式

| 模式 | Manifest | 通讯 | 网络限制 | 推荐用途 |
| --- | --- | --- | --- | --- |
| 进程 | `plugin.yaml` | TCP `127.0.0.1:50071` | 只能声明允许网络 | Windows 本地开发、可信部署 |
| 容器 | `plugin.container.yaml` | Unix Socket | `network.enabled: false`，Docker `--network none` | Linux 生产隔离部署 |

不要同时把两个 manifest 放进同一个插件目录：二者的 `metadata.id` 相同，会造成发现冲突。每次只安装一个。

## 进程模式：本地开发

进程型插件运行在宿主环境，宿主无法可靠地把它的网络权限限制为关闭。因此 `plugin.yaml` 必须声明：

```yaml
permissions:
  network:
    enabled: true
```

### 1. 构建二进制

在仓库根目录执行：

```powershell
go build -o examples/local-files-plugin/local-files-plugin.exe ./examples/local-files-plugin
```

Linux/macOS 使用：

```bash
go build -o examples/local-files-plugin/local-files-plugin ./examples/local-files-plugin
chmod +x examples/local-files-plugin/local-files-plugin
```

### 2. 安装 manifest 和程序

插件目录中每个插件使用独立子目录。下面以仓库根目录下的 `plugins/local-files` 为例：

```text
plugins/
└── local-files/
    ├── plugin.yaml
    └── local-files-plugin[.exe]
```

Windows PowerShell：

```powershell
New-Item -ItemType Directory -Force plugins/local-files | Out-Null
Copy-Item examples/local-files-plugin/plugin.yaml plugins/local-files/plugin.yaml
Copy-Item examples/local-files-plugin/local-files-plugin.exe plugins/local-files/local-files-plugin.exe
$env:WEKNORA_PLUGIN_DIR = (Resolve-Path plugins).Path
```

Linux/macOS：

```bash
mkdir -p plugins/local-files
cp examples/local-files-plugin/plugin.yaml plugins/local-files/plugin.yaml
cp examples/local-files-plugin/local-files-plugin plugins/local-files/local-files-plugin
export WEKNORA_PLUGIN_DIR="$PWD/plugins"
```

`command: ["./local-files-plugin"]` 相对于 manifest 所在目录执行。Windows 若使用 `.exe`，请把已安装 manifest 中的 command 改为 `./local-files-plugin.exe`。

### 3. 配置数据源

创建外部 datasource 时，为插件配置传入实际目录：

```json
{
  "rootPath": "C:\\Users\\you\\Documents\\knowledge"
}
```

Linux 示例：

```json
{
  "rootPath": "/srv/knowledge"
}
```

`rootPath` 必填，且必须是插件进程可访问的已存在目录。插件的 `ValidateConfig` 会检查该目录。首次同步会导入 `.md` 和 `.txt`；后续同步使用 WeKnora 持久化的 cursor，仅重传变更和删除事件。

### 4. 验证同步

1. 在 `rootPath` 下创建 `hello.md` 并发起同步，应看到一条 upsert；
2. 修改该文件并再次同步，应只更新该文件；
3. 删除该文件并再次同步，应收到 delete；
4. 确认 datasource 的 cursor 已更新，下一次重试将从持久化 cursor 继续。

## 容器模式：Linux 隔离部署

容器 manifest 使用 Docker 隔离并声明禁网：

```yaml
entrypoint:
  type: container
  grpcAddress: unix:///var/lib/weknora/plugins/local-files/plugin.sock
  containerGrpcAddress: unix:///run/weknora/plugin.sock
permissions:
  network:
    enabled: false
  filesystem:
    readOnly:
      - "${config.rootPath}"
```

运行时会将 datasource 的 `rootPath` 作为只读目录挂入容器，并使用共享 Unix Socket 与主程序通讯。禁网容器不使用 TCP gRPC；框架会为其设置网络隔离和受限运行参数。

### 1. 构建镜像

必须从仓库根目录构建，因为 Dockerfile 需要读取插件 proto：

```bash
docker build -f examples/local-files-plugin/Dockerfile \
  -t weknora/local-files-plugin:0.1.0 .
```

### 2. 安装 container manifest

将 `plugin.container.yaml` 复制为插件目录中的 `plugin.yaml`：

```bash
mkdir -p plugins/local-files
cp examples/local-files-plugin/plugin.container.yaml plugins/local-files/plugin.yaml
export WEKNORA_PLUGIN_DIR="$PWD/plugins"
```

在 Compose 部署中，使用 `WEKNORA_PLUGIN_HOST_DIR` 把宿主插件目录只读挂载给 app；详细配置见外部插件开发文档。

### 3. 选择安全的文件目录

容器模式的 `rootPath` 必须是 Docker daemon 所在宿主机上可解析的目录。插件 manifest 只会把该路径以只读方式授权给插件容器。不要把仓库根目录、用户主目录或包含凭据的上级目录作为 `rootPath`。

示例：

```json
{
  "rootPath": "/srv/weknora-import"
}
```

## `networkProbeTarget` 测试项

`networkProbeTarget` 仅用于演示网络策略的可观测性；通常不要在真实数据源配置中设置它。

```json
{
  "rootPath": "/srv/weknora-import",
  "networkProbeTarget": "example.com:443"
}
```

插件会尝试建立 TCP 连接。失败时会发送 `SECURITY_POLICY_DENIED` 同步事件。对于禁网容器，这可用于验证插件无法进行出站网络访问。不要把这个字段当作网络白名单：真正的网络隔离由 manifest 和 Docker runtime 实施。

## Windows 限制

- Windows 可使用进程模式和 TCP gRPC。
- 容器示例依赖 Unix Socket，不能直接作为 Windows 宿主上的本地插件端点运行。
- 若在 Windows 上运行 Docker Desktop，请在 Linux 容器/WSL2 环境内按 Linux 容器模式部署，并确保 `rootPath` 对 Docker daemon 可见。

## 安全提示

- 进程模式适用于开发或可信插件；它不具备可强制的禁网能力。
- 容器模式应优先使用最小的只读目录授权，不要授予父目录。
- 不要在 manifest、路径、错误文本或数据源配置中写入 token；运行时审计会持久化受控事件字段，但不会持久化插件审计事件的目标和错误消息。
- 容器型插件需要主程序访问 Docker runtime。Docker socket 通常等价于较高宿主权限，不应在默认部署中无条件挂载；应使用受控 runtime 或 Docker socket proxy，并仅在明确启用容器插件时授予。

## 测试

在仓库根目录执行：

```bash
go test ./examples/local-files-plugin
```

测试覆盖目录校验、网络探针失败事件和基于 cursor 的增量 upsert 行为。
