# cpagw

通过 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 Go SDK 嵌入网关，主要产物为单个 `cpagw` binary。本仓库与其他 `cpa` 命令或仓库无关。

## 配置模型

```text
provider：服务提供商配置（默认上游 API key）
  └── connection：协议、API 根地址、可选 key 覆盖、模型清单
profile：下游 Agent 的专属 key、三档模型绑定与展示
```

提供商名称不决定协议。例如一个 `deepseek` provider 可以配置 Chat Completions、Anthropic Messages 和 Responses 三个 connection。每个 profile 的 Opus、Sonnet、Haiku 档位明确选择 provider、connection 和实际模型 ID。

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

合并 PR 到 `main` 后自动打 tag 并发布 Release：

```text
合并 PR → ci 通过 → 打 tag（v2026.10.5-abc123）→ GoReleaser 发布 Release
```

tag 由日期与 commit 短 SHA 组成，不含补零（`2026.10.5` 而非 `2026.10.05`），否则不是合法 semver，GoReleaser 会拒绝。**直接 push 到 `main` 不会发布**，只有经 PR 合并的提交才触发。

Release 不是预发布版，因此 `/releases/latest` 始终指向最新一次发布，安装脚本无需指定版本即可取用。手动发布可在目标 commit 上推一个 `v*` tag：

```sh
git tag v2026.10.5-abc123 && git push origin refs/tags/v2026.10.5-abc123
```

## 接入提供商

准备模型清单（必须填写上游真实支持的模型 ID）：

```yaml
models:
  - id: upstream-large
    name: 主力模型
  - id: upstream-balanced
    name: 均衡模型
  - id: upstream-fast
    name: 快速模型
```

```sh
cpagw provider add deepseek
cpagw provider connection add deepseek anthropic \
  --protocol anthropic-messages \
  --base-url https://api.deepseek.com/anthropic \
  --models models.yaml
cpagw provider show deepseek
cpagw provider models deepseek --connection anthropic
cpagw provider check deepseek --connection anthropic
```

provider add 通过隐藏输入读取默认 key。非交互场景使用 `--api-key-stdin`，不要将 key 放进命令参数。connection 默认继承 provider key，使用同名选项可以独立覆盖；update 的 `--inherit-api-key` 清除覆盖。show/list 脱敏，不回显 key。

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
cpagw provider update deepseek --api-key-stdin
cpagw provider connection update deepseek anthropic --models models.yaml
cpagw provider connection update deepseek anthropic --inherit-api-key
cpagw provider connection remove deepseek anthropic
cpagw provider remove deepseek
cpagw profile delete daily
```

更新默认 key 只影响继承连接，不覆盖连接自己的 key。删除 provider/connection 或移除模型前检查 profile 引用，存在引用时拒绝。被活动 apply 记录引用的 profile 需先 restore 或切换到其他 profile。

## 本地状态与安全

- 状态目录默认为 `$XDG_CONFIG_HOME/cpagw`，未设置时使用 `~/.config/cpagw`；可用 `--state-dir` 隔离。
- 状态目录 0700、敏感文件 0600；配置、secret 引用及 secret 数据以同一原子事务保存。上游 key 和下游 profile key 分开引用，SDK 配置是私有派生数据。
- 本地状态和 Claude Code 应用后的 settings 都可能含明文 key；文件权限保护不是加密保险箱。不要提交或分享这些文件。
- 默认仅监听 loopback；后台和前台启动都会预检查监听地址，端口冲突时明确报告占用地址，不启动网关，也不停止原占用服务。预检查分两步：先探测同端口的 `127.0.0.1` 与 `::1` 是否已有服务监听，再验证地址可绑定。**只做绑定检查并不足够**——macOS 允许通配监听（如 `*:8317`）与具体 loopback 地址同时绑成功，绑定成功不能证明端口空闲。预检查会立即释放临时监听器，正式监听和实例就绪校验仍负责处理之后的端口竞争。
- 下游只开放模型列表、Messages 及支持的 count_tokens，其他执行协议及管理入口不向 profile key 开放。
- 模型目录隐藏不是唯一的访问控制：每次推理也严格校验 key 和公开模型绑定。
- 当前配置更新通过网关进程内重建 SDK 服务生效，可能短暂不可用，并中断在途请求；CLI 会确认新 revision 就绪后才报告成功。SDK watcher 无中断重载是后续优化项。
- stop 不承诺等待全部在途 SSE 完整结束。

## 验证

```sh
go test ./...
go test -race ./...
go vet ./...
```

测试只使用临时状态/settings 和 mock 上游，不改真实 `~/.claude`，不调用付费模型。真实模型的兼容性和费用需要另外授权验收。
