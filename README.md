# cpagw

通过 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 Go SDK 嵌入网关，主要产物为单个 `cpagw` binary。本仓库与其他 `cpa` 命令或仓库无关。

## 配置模型

```text
connection：独立上游连接（稳定 ID、协议、认证方式、API 根地址、专属凭证、模型清单）
profile：下游 Agent 的专属 key、三档 connection/实际模型绑定与展示
```

接入只需创建一个 connection，不需要先创建 provider。每个连接独立选择协议和凭证，名称不决定协议。每个 profile 的 Opus、Sonnet、Haiku 档位直接选择 connection 和实际模型 ID；多个档位可以使用同一连接。

**这是不兼容旧配置的结构调整。** 当前只支持 schema 3，不保留 `provider` 命令、旧 profile 的 `provider` 字段或旧状态迁移。schema 1，以及旧 provider 版和独立 API-key 版 schema 2，均明确拒绝且不会被覆盖。已有配置的用户应先用原版本执行 restore、停止旧服务，再用新的 `--state-dir` 重新接入；OAuth 连接需要重新登录。不要让旧、新版本共享状态目录。

下游公开模型 ID 保持为 Claude ID。不同 profile 可以使用相同公开 ID，但展示不同名称并路由到不同上游。

## 安装

```sh
curl -fsSL https://raw.githubusercontent.com/shichao-wang/cpagw/main/scripts/install.sh | bash
```

脚本识别 macOS/Linux 与 amd64/arm64，从 [Releases](https://github.com/shichao-wang/cpagw/releases) 下载对应产物并用 `checksums.txt` 校验后安装。默认装到 `/usr/local/bin`，该目录不可写时回退到 `~/.local/bin`。

可选环境变量：

| 变量 | 说明 |
|---|---|
| `CPAGW_VERSION` | 指定版本（如 `v0.1.0`），默认为最新 Release |
| `CPAGW_INSTALL_DIR` | 指定安装目录，优先级高于默认与回退规则 |

安装包校验是强制环节，没有跳过开关：脚本比对 `checksums.txt` 中的 SHA256，不匹配即中止。该检查防的是传输损坏与只改动产物一侧的篡改，属于完整性校验，不构成对发布来源的认证。

建议先下载脚本审阅再执行：`curl -fsSL <上述地址> -o install.sh`，检查无误后 `sh install.sh`。本工具暂不支持 Windows。

## 升级

```sh
cpagw upgrade
```

升级支持 macOS/Linux 的 amd64 与 arm64。命令只替换当前正在运行的 `cpagw` 实际可执行文件；通过符号链接启动时保留符号链接并更新其目标，不修改 cpagw 配置。它不会自动提权（`sudo`）、切换安装目录或重启网关进程；如果当前可执行文件所在目录不可写，升级会失败，请自行处理目录权限。网关正在运行时，升级后需手动重启。

当前版本与最新 Release tag 相同时跳过替换；从源码构建或版本未知时，将替换为最新 Release。升级复用安装脚本的 `checksums.txt` SHA256 完整性校验边界，不代表对发布来源的认证。

## 构建

首期支持 macOS/Linux（进程管理依赖 Unix 信号及 `ps`），需要 Go 1.27.1 或更新版本：

```sh
go build -o bin/cpagw ./cmd/cpagw
export PATH="$PWD/bin:$PATH"
cpagw --help
```

SDK 固定为 CLIProxyAPI v8.0.10。分发时请附上 `THIRD_PARTY_NOTICES`，并遵守传递依赖的许可证。

## 发布

PR、`main` push 和 merge queue 候选会运行相同的合并前门禁：Linux/macOS 全量测试、重复终端 PTY 测试、race、vet、build，以及只读 GoReleaser snapshot 预演。固定 required check 为 `ci / pre-merge`；任何前置 job 失败、取消或跳过都不会放行。Snapshot 会检查四平台归档、必备文件、SHA256 与当前 runner 可执行的原生 binary，但不执行正式 tag/git validate，也不访问 GitHub Release API；这不等于正式发布验证。

门禁通过后，仅 `main` push 会进入打 tag job；只有该提交由已合并 PR 引入时才生成 tag 并发布：

```text
合并 PR → ci / pre-merge 全部通过 → 打 tag（v2026.10.5-abc123）→ GoReleaser 发布 Release
```

tag 由日期与 commit 短 SHA 组成，不含补零（`2026.10.5` 而非 `2026.10.05`），否则不是合法 semver，GoReleaser 会拒绝。**直接 push 到 `main` 不会发布**，merge queue 只运行检查、不打 tag 或发布。

成功发布的 Release 不标记为预发布版，安装脚本默认通过 `/releases/latest` 获取最新正式产物。当前工作流不监听 tag push，**只推送 `v*` tag 不会触发发布**。本地可用同一配置预演（GoReleaser v2.18.2）：

```sh
goreleaser check
goreleaser release --snapshot --clean
python3 -I scripts/verify-release-artifacts.py dist
```

## 接入上游

交互接入只需一条命令：

```sh
cpagw connection add
# 也可以先指定名称：cpagw connection add example
```

向导依次收集连接名称、认证方式、协议、API 根地址、模型清单和凭证信息，确认后一次保存。API-key 连接使用隐藏输入；Codex OAuth 连接不询问 key，创建后单独登录。认证方式、协议、模型来源、继续添加模型和最终确认均使用 **↑/↓ 选择、Enter 确认**，不需要输入数字编号或 `y/n`；保存和删除确认默认选中“取消”。直接录入模型时，每条使用 `ID` 或 `ID=展示名`，之后选择“继续添加模型”或“完成录入”；也可以选择 YAML 文件，不必先创建文件。

在文本、选择菜单、密码和确认阶段都可按 **Control+C** 取消；命令提示“已取消”，以退出码 130 返回 shell 并恢复终端。保存前（包括等待状态锁时）取消不会保存连接、凭证或增加 revision；原子提交开始后不保证撤回，保存成功后的重载失败仍会明确报告已保存。添加默认不联网、不发模型请求。

脚本接入先准备模型清单（必须填写上游真实支持的模型 ID）：

```yaml
models:
  - id: upstream-large
    name: 主力模型
  - id: upstream-balanced
    name: 均衡模型
  - id: upstream-fast
    name: 快速模型
```

将 key 从标准输入传入，不要将其放进命令参数或 shell 历史：

```sh
# 从安全的凭证来源提供 stdin；不要在命令中硬编码 key。
cpagw connection add example \
  --protocol anthropic-messages \
  --base-url https://api.example.com \
  --models models.yaml \
  --api-key-stdin
cpagw connection show example
cpagw connection models example
cpagw connection check example
```

API-key 非交互接入必须提供完整参数和 `--api-key-stdin`；OAuth 非交互接入显式指定 `--auth-type codex-oauth`、`--protocol responses` 和模型清单，不提供 API key。缺参会报错而不会等待向导。每个连接持有自己的凭证，不存在默认 key 继承或隐藏的 provider。show/list 仅显示认证方式与凭证就绪状态，不回显 key、token、凭证引用或账户标识。

### Codex OAuth

Codex OAuth 目前只支持 `responses` 协议和 OpenAI 官方 Codex endpoint `https://chatgpt.com/backend-api/codex`。可省略 `--base-url`，也可显式指定这个官方地址；不支持自定义 OAuth endpoint。认证通过 Codex 的设备码流程完成：命令输出官方验证页和设备码，用户在浏览器中完成授权，CLI 等待授权结果。可用 `--no-browser` 禁止 CLI 尝试打开浏览器。

为 Codex OAuth 准备独立的 `codex-models.yaml`，不要复用上面 API key 示例的模型清单：

```yaml
models:
  - id: gpt-5.5
    name: Codex 示例模型
```

`gpt-5.5` 仅为示例，使用前请确认自己的账号实际支持该模型，并据此填写模型清单与 profile 绑定。

```sh
cpagw connection add codex \
  --auth-type codex-oauth \
  --protocol responses \
  --models codex-models.yaml
cpagw connection login codex --no-browser
# bindings.yaml 的 opus/sonnet/haiku 均设置 connection: codex、target_model: gpt-5.5
cpagw profile create codex --agent claude-code --file bindings.yaml
cpagw server start
cpagw profile apply codex
cpagw server stop
cpagw connection logout codex --yes
```

认证方式切换必须在 `connection update` 中显式传 `--auth-type`；切换为 API key 时还需在同一命令显式提供 `--api-key-stdin`，不能继承或回退。新增 OAuth 连接、`login`、`logout`、重登录、认证类型切换及删除 OAuth 连接，均要求先停止网关；API-key 连接仍支持热更新。连接保留，logout 只清除本地保存的 OAuth 凭证，不代表向上游撤销授权。OAuth 凭证属于本地敏感状态，可能以明文保存但文件权限限制为 0600；请勿提交或分享状态文件。OAuth 账号可用模型取决于账号实际授权及 SDK 注册目录；模型清单只是本地声明，不保证账号支持该模型，也不意味着任意 Responses 服务兼容。OAuth 检查不会携带 access token 请求通用 `/models` endpoint。

上游协议：

| protocol | 上游入口 | SDK 适配 |
|---|---|---|
| `anthropic-messages` | API 根地址 + `/v1/messages` | Claude |
| `chat-completions` | API 根地址 + `/chat/completions` | OpenAI compatibility |
| `responses` | API 根地址 + `/responses` | Codex HTTP |

地址应与 SDK 的路径拼接契约匹配。例如 OpenAI 常规 Chat Completions 根地址包含 `/v1`，DeepSeek 可使用 `https://api.deepseek.com`。Anthropic 根地址不要再以 `/v1` 结尾。Responses 不自动追加 `/v1`。

**Responses 连接已关闭 SDK 的 Codex 身份强制设置，但仍有请求规范化，不是任意 Responses API 的透明透传。** 支持指定协议不等于任何服务或模型都完全兼容 Claude Code 的 thinking、工具调用和上下文能力。三种协议不做自动故障切换。

add/create 默认不联网。check 仅尝试模型目录，不发送推理；目录缺失、404/405 表示无法通过这种方式验证，不能据此判断推理不可用。检查结果不会自动替换本地清单。

## Profile 与 Claude Code

绑定文件示例见 `examples/`。公开 model ID 需明确填写你希望 Claude Code 使用的官方 ID，不能填实际的非 Claude 上游 ID。三个档位 ID 必须唯一。

```sh
cpagw profile create daily --agent claude-code --file bindings.yaml
cpagw server start
cpagw server status
cpagw profile apply daily
# 新开 Claude Code 会话使用已应用的配置。
cpagw profile restore
cpagw server stop
```

模型选择器展示需要支持 `modelPicker` 的 Claude Code 版本（v2.1.242 或更新版本）。

apply 默认合并 `~/.claude/settings.json`，也可通过 `--settings` 指定文件。通过 env 和 modelPicker 设置网关认证、模型 ID、名称与描述，不修改订阅登录凭证。模型发现不会可靠地覆盖所有内置档位，因此展示配置也由 profile 应用。

restore **仅对有有效 cpagw 接管记录的配置执行**。它恢复首次接管前的受管字段，保留无关修改；受管字段发生冲突时拒绝整体恢复。没有记录、只有备份、路径不匹配或重复 restore 均不能恢复。当前 shell 环境变量及项目级设置可能覆盖用户配置，需自行检查这些优先级。

## 更新与删除

```sh
cpagw connection update example --api-key-stdin
cpagw connection update example --models models.yaml
cpagw connection update example --base-url https://api.example.com
cpagw connection remove example
cpagw profile delete daily
```

更新未指定 key 时保留连接的原凭证；地址、模型与 key 同时修改时按同一事务保存。轮换只影响目标连接。删除连接或移除模型前检查 profile 引用，存在引用时拒绝。被活动 apply 记录引用的 profile 需先 restore 或切换到其他 profile；解除 profile 对连接的引用后才能删除连接。非交互删除需提供 `--yes`。

## 本地状态与安全

- 状态目录默认为 `$XDG_CONFIG_HOME/cpagw`，未设置时使用 `~/.config/cpagw`；可用 `--state-dir` 隔离。
- 状态目录 0700、敏感文件 0600；配置、secret 引用及 secret 数据以同一原子事务保存。上游 key、Codex OAuth 凭证和下游 profile key 分开管理，SDK 配置是私有派生数据。
- 当前状态格式为 schema 3，仅接受显式认证方式、稳定连接 ID 与独立 OAuth 表的完整新格式；不存在 `state migrate` 或自动默认化。旧 schema 1/2 及 provider 结构会拒绝读取/覆盖，使用新状态目录重新配置，OAuth 连接重新登录。
- 本地状态和 Claude Code 应用后的 settings 都可能含明文 key；文件权限保护不是加密保险箱。不要提交或分享这些文件。
- 默认仅监听 loopback；后台和前台启动都会预检查监听地址，端口冲突时明确报告占用地址，不启动网关，也不停止原占用服务。预检查分两步：先探测同端口的 `127.0.0.1` 与 `::1` 是否已有服务监听，再验证地址可绑定。**只做绑定检查并不足够**——macOS 允许通配监听（如 `*:8317`）与具体 loopback 地址同时绑成功，绑定成功不能证明端口空闲。预检查会立即释放临时监听器，正式监听和实例就绪校验仍负责处理之后的端口竞争。
- 下游只开放模型列表、Messages 及支持的 count_tokens，其他执行协议及管理入口不向 profile key 开放。
- 模型目录隐藏不是唯一的访问控制：每次推理也严格校验 key 和公开模型绑定。
- 本地状态是配置与 OAuth 凭证的唯一事实源，SDK 配置和认证记录均为派生运行时数据。网关进程使用单个 SDK Service；结构变更在该 Service 上串行热更新，关闭请求 gate 直至新状态就绪后再报告成功。OAuth token 刷新仅做凭证条件保存，不触发结构重载；登录、退出和认证方式切换仍要求先停止网关。
- stop 不承诺等待全部在途 SSE 完整结束。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
```

测试只使用临时状态/settings 和 mock 上游，不改真实 `~/.claude`，不调用付费模型。真实模型的兼容性和费用需要另外授权验收。
