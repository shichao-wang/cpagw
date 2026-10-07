# cpagw 开发约定

## 定位

本仓库产物是单个 `cpagw` binary，通过 CLIProxyAPI 的公开 Go SDK 嵌入网关。与同工作空间中的 `cpa` 仓库无关，不修改或导入其实现。

## 领域边界

- `connection` 是独立上游连接，保存稳定 ID、明确的协议/认证方式、地址、自己的凭证引用和模型清单；名称在状态内唯一。
- `profile` 表示下游 Agent 的专属 key、公开模型 ID 与 connection/上游模型绑定。
- 不引入 provider、vendor 或默认凭证继承；不要从连接名称推断协议能力。
- cpagw 状态是唯一事实源，SDK 配置是私有派生数据。
- 仅支持 schema 3，不保留 provider、旧绑定或 schema 1/2 的兼容与迁移；遇到旧状态拒绝读取和覆盖，使用新状态目录重新配置。
- Codex OAuth 仅允许 responses 协议及官方 Codex endpoint，不回退 API key。结构状态与 OAuth 凭证分离，刷新以连接 ID/ref/generation 条件保存，不增加结构 revision；失败须撤销目标 SDK 认证并停止在途输出。
- OAuth 创建、登录/退出、认证切换及删除要求网关停止；网络登录只持运行锁，不持状态锁。静态热更新不能重新加载 OAuth 或复活失效凭证。

## 实现约束

- 使用锁定版本的公开 SDK；不导入其 internal，不擅自 fork。
- 同协议同模型的不同连接必须隔离 endpoint/key，不能只按协议类型路由到共享池。
- 仅修改结构化模型标识，不替换正文中的模型字符串；SSE 不整段缓冲。
- 下游鉴权与模型授权必须 fail-closed，管理凭证与 profile key 分离。
- 端口预检查必须包含同端口连通性探测，不能只依赖绑定是否成功：macOS 允许通配监听（`*:port`）与具体 loopback 地址同时绑成功，仅检查绑定会漏判并顶掉原服务。
- restore 只允许针对本工具成功 apply 且仍受管理的配置，保留用户无关修改；受管字段冲突时拒绝整体恢复。

## 测试与安全

合并前验证应与 `.github/workflows/release.yml` 保持一致：Linux/macOS 均运行 `go test -count=1 ./...`、`go test -count=3 ./internal/integration -run '^TestWizardTerminal$'`、`go test -race -count=1 ./...`、`go vet ./...` 和 build；发布预演固定 GoReleaser v2.18.2，并验证 snapshot 四平台归档、checksums 与原生 binary smoke。修改门禁脚本或 workflow 时同时运行 `python3 -I scripts/verify_release_artifacts_test.py`、`python3 -I scripts/verify_ci_gate_test.py` 和 YAML/config sanity 检查。Snapshot 不等价于真实 tag 或 GitHub Release API 验证。

```sh
go test -count=1 ./...
go test -count=3 ./internal/integration -run '^TestWizardTerminal$'
go test -race -count=1 ./...
go vet ./...
go build -o bin/cpagw ./cmd/cpagw
```

测试使用临时状态目录、临时 settings 和 mock 上游。不修改真实 `~/.claude`，不执行真实付费模型调用或账户登录，除非用户明确授权。

API key 不通过 argv 接收、不写入日志、错误、README 或测试快照。状态和备份以 0700/0600 权限保存，不提交运行配置、凭证或二进制。默认仅监听 loopback。说明、错误及必要注释使用中文。
