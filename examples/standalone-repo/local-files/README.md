# WeKnora Local Files Plugin (Standalone Repository)

这是一个**独立仓库**形态的 WeKnora datasource 插件，验证课题核心验收第一条：
**不修改主仓任何代码，插件即可被发现、启动、健康检查并完成数据同步。**

本目录模拟“把插件迁出主仓”后的独立仓库结构——它不 import 主仓任何包，
只依赖 `github.com/Tencent/WeKnora/sdk/plugin`（插件 SDK module）。

## 仓库结构

```text
weknora-plugin-local-files/
├── main.go          # 插件实现（datasource：目录扫描 + 增量同步）
├── main_test.go     # 增量同步测试（只处理变更文件）
├── plugin.yaml      # WeKnora 插件 manifest
├── go.mod           # 仅依赖 sdk/plugin，无主仓依赖
└── README.md
```

## 与主仓的关系

`go.mod` 只有一处对 WeKnora 的引用：

```text
require github.com/Tencent/WeKnora/sdk/plugin v0.1.0
```

开发期用 `replace` 指向本地 SDK 检出（tag 发布后删除该块即可）：

```text
replace github.com/Tencent/WeKnora/sdk/plugin => ../../../sdk/plugin
```

## 构建

```bash
go build -o weknora-plugin-local-files .
```

## 安装到 WeKnora

```bash
# 1. 创建插件目录
mkdir -p /var/lib/weknora/plugins/local-files

# 2. 拷贝 manifest 与二进制
cp plugin.yaml /var/lib/weknora/plugins/local-files/
cp weknora-plugin-local-files /var/lib/weknora/plugins/local-files/

# 3. 指向插件根目录并启动宿主
export WEKNORA_PLUGIN_DIR=/var/lib/weknora/plugins
./weknora
```

宿主启动时会：

1. 扫描 `WEKNORA_PLUGIN_DIR` 下每个子目录的 `plugin.yaml`
2. 校验 manifest（extensionType、semver 范围、capability 白名单）
3. 启动插件进程并通过 `GetInfo` RPC 核验身份（ID/版本/类型须与 manifest 一致）
4. 调用 `Describe` 注册为 datasource connector
5. 周期性健康检查（`HealthCheck` RPC）

## 使用

在 WeKnora 中新建数据源时选择 `Local Files (Standalone Repo)` 类型，
配置 `rootPath`（要同步的目录），然后：

- **全量同步**：`FetchAll` 扫描目录内全部 `.md` / `.txt` 文件
- **增量同步**：`Sync` 基于内容哈希的 cursor——仅变更文件被重新处理，
  被删除的文件产生 delete 事件

## 测试

```bash
go test ./...
```

覆盖：配置校验拒绝不存在的 rootPath、首次同步全量 upsert、
第二次同步只处理变更文件、文件删除产生 delete 事件、resourceIds 资源选择。
