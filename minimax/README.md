# minimax-acp

用 **Go 1.22+、零第三方依赖**实现的 MiniMax Code ACP 工具集：驱动、代理、诊断 **MiniMax Code 原生 ACP v1 stdio server**，并提供一个确定性的 Go ACP agent 作为协议夹具。

MiniMax Code（npm `@minimax-ai/code`，CLI `mcode`）通过 **`mcode acp` 原生提供 ACP server**。所以本项目**不重复实现模型、工具或权限链路**，而是补齐 Go 侧需要的东西：

当前版本 `00.00.02`：修复 bridge 在建会话、加载／恢复和 prompt 响应前未等待通知转发的问题，保证最终文本与历史重放先于对应响应；阻塞通知 worker 的回归用例覆盖此顺序。该修复不扩大真实 MiniMax 证据边界。

| 形态 | 作用 |
| :--- | :--- |
| `doctor` | 只读体检：CLI 版本、原生 ACP 命令、登录数据目录（仅目录名）、桌面沙箱环境干扰 + `initialize` 握手 |
| `chat` | 作为 ACP Client 驱动 `mcode acp`：流式输出、权限策略、模式与模型配置选项 |
| `bridge` | 透明 ACP stdio 代理，前置 `mcode acp`，转发双向帧并做 `$/cancel_request` 的 id 重映射 |
| `serve` | Go 实现的确定性 ACP agent（echo fixture），给 Zed / JetBrains / 协议对拍用 |

```
ACP 宿主 (Zed / JetBrains / auto-run / ...)
   │  stdin/stdout  ACP v1 ndjson
   ▼
minimax-acp bridge ──────────────► mcode acp  (MiniMax Code 原生 ACP server)
                                    │
minimax-acp doctor / chat ─────────┘   (直接作为 Client 连接)
```

## 事实来源

| 来源 | 采用的内容 |
| :--- | :--- |
| [MiniMax Code CLI Quick Start](https://agent.minimax.io/docs/cli/quick-start) | `mcode acp` 是面向编辑器/客户端的 ACP 入口；安装 `npm install -g @minimax-ai/code`；`mcode login` / `--region global` |
| [npm `@minimax-ai/code`](https://www.npmjs.com/package/@minimax-ai/code) | `mcode acp` 通过 stdin/stdout 运行 ACP server，可创建/加载/恢复/关闭会话；stdout 全归协议，不能当交互命令用 |
| 第三方集成文档（Kandev `agents-and-profiles.md`） | 原生 ACP 支持模型选择、权限请求、MCP 配置、取消与会话加载；当前 ACP 接口仅接受文本 prompt，不声明 image/audio/video 输入 |
| 本机实测（2026-11-08，mcode 0.6.3 / macOS） | `mcode --version`、`mcode acp --help` 成功；`mcode acp` 在桌面沙箱内 **无任何 stdio 响应**。见 [docs/probes/index.json](docs/probes/index.json) |

用户提供的资料中 `npm install -g @//minimax-ai/code` 有笔误，正确包名是 `@minimax-ai/code`。本项目固定以 `mcode acp` 为上游，不调用 `mcode exec`，也不包装任何第三方 ACP 适配层。

## 构建

```bash
make build            # -> bin/minimax-acp（Go 二进制，.gitignore 排除）
make test             # go test ./... -count=1
make vet              # go vet ./...
make race             # go test ./... -race -count=1
bin/minimax-acp version
```

编译产物不进 Git。换机器或清理 `bin/` 后重新 `make build`。

## 命令

### `doctor` — 只读体检（默认无副作用）

```bash
bin/minimax-acp doctor --json
bin/minimax-acp doctor --json=false --region global
bin/minimax-acp doctor --handshake-timeout 30s --trace docs/probes/my-trace.ndjson
```

默认只做「CLI/环境检查 → `initialize` →（agent 声明支持时）`session/list`」，**绝不发送 `session/prompt`、绝不创建会话**。报告字段含 `status`、`promptSent`、`sessionCreated`、`cli`、`agentCapabilities`、`authMethods`、`diagnostics`。

- `--check-auth` 会调用 `session/new`（**有副作用**，默认关闭，报告里 `sessionCreated=true`）。
- `--include-titles` 才输出会话标题，默认打码 `[redacted]`。
- `--handshake-timeout`（默认 30s）给握手单独设上限：某些桌面沙箱下 `mcode acp` 完全不吐帧，此时返回 `status=no_response` 并给出环境诊断，而不是挂死。
- `cli` 只 `stat` `~/.minimax` 与 `auth/` 的**目录名**，从不打开 `auth.json`、`config.yaml` 或 `sessions/*`，报告不含凭证。

### `chat` — 驱动真实会话

```bash
bin/minimax-acp chat --prompt "解释这个仓库的 ACP 接入"
bin/minimax-acp chat --session <sessionId> --resume
bin/minimax-acp chat --show-state --mode <modeId> --set-config model=<value> -p "你好"
bin/minimax-acp chat --permission-mode prompt --allow-write --allow-terminal
```

- `--permission-mode deny|allow|prompt`：默认 `deny`（最小权限）；`prompt` 在终端逐项询问。
- `--allow-write` / `--allow-terminal` 才会向 agent 声明 `fs/write_text_file` / `terminal/*`，默认都不开。
- `--show-state` 打印 agent **实际返回**的 `modes` 与 `configOptions`；`--mode` / `--set-config` 把它们交给 `session/set_mode` / `session/set_config_option`。本项目不会猜测 MiniMax 的 modeId 或 configId。
- `--image` 附加图片块（MiniMax 的 ACP 是否接受非文本输入以其 `promptCapabilities` 为准，本项目不假定）。
- `--trace` 把每一帧写成 ndjson 证据；stdout 只给人看，协议永远走子进程的 stdio。

### `serve` — Go ACP agent 夹具

```bash
bin/minimax-acp serve --name my-agent --trace /tmp/serve.ndjson
```

标准 ACP v1 stdio server：`initialize` / `session/new` / `load` / `resume` / `list` / `prompt` / `cancel` / `set_mode` / `set_config_option`、流式 `session/update`、一次反向权限请求。返回的 modes / configOptions 全部带 `fixture-` 前缀，**是确定性夹具，不是 MiniMax 的真实模型目录或权限模式**。

### `bridge` — 透明代理

```bash
bin/minimax-acp bridge --trace /tmp/bridge.ndjson
```

上游（编辑器）与下游（`mcode acp`）的请求、响应、通知原样转发，未知 vendor 字段不丢失；`$/cancel_request` 的 requestId 在两侧做双向映射。stderr 只写诊断，stdout 永远是纯协议流。

## 接入编辑器

**原生方式最简单**（无需本项目）：

```json
{ "agent_servers": { "minimax-code": { "command": "mcode", "args": ["acp"], "env": {} } } }
```

完整模板：

| 文件 | 用途 |
| :--- | :--- |
| [examples/editor-generic.json](examples/editor-generic.json) | 最小通用配置（按宿主实际 schema 调整 `type` 字段） |
| [examples/zed-settings.json](examples/zed-settings.json) | Zed：原生 + Go bridge + Go echo fixture 三个条目 |
| [examples/jetbrains-acp.json](examples/jetbrains-acp.json) | JetBrains 同上的三条目 |

模板里的绝对路径按本机填写；换机器要改。若宿主不能传 `--command`，用环境变量 **`MINIMAX_ACP_CLI`**（本项目自定义，不是 MiniMax 官方变量）指向 `mcode` 全路径。Global 账号先在普通终端登录：`mcode login --region global`；国内账号用 `mcode login`。

## ACP 能力映射

| ACP 方法 / 能力 | 本项目 Go 实现 | 真实 `mcode acp` |
| :--- | :--- | :--- |
| `initialize` | ✅ Client / Agent / Bridge 全实现 | ⚠️ 本机未收到响应，能力声明未采集 |
| `session/new` / `load` / `resume` / `list` / `close` / `delete` | ✅ | ⚠️ 未验证 |
| `session/prompt` + `session/update` 流式 | ✅（含文本/思考/工具调用/计划/用量） | ⚠️ 未验证 |
| `session/cancel`、`$/cancel_request` | ✅（bridge 做 id 重映射） | ⚠️ 未验证 |
| `session/request_permission` | ✅（deny / allow / 交互式） | ⚠️ 未验证 |
| `session/set_mode` / `set_config_option` | ✅（只使用 agent 返回的 id） | ⚠️ 模型目录未采集 |
| `authenticate` / `logout` / `authMethods` | ✅ 协议层开放对象 | ⚠️ 未采集 |
| `fs/*`、`terminal/*` | ✅ 本地实现，默认只读 | ⚠️ 未验证 |
| 图片 / 音频 / 资源链接 | ✅ 协议层支持 | ⚠️ 官方集成文档称 ACP 仅接受文本，未在本机确认 |

官方资料列出 ACP 宿主可接管「对话与推理、代码变更 Diff、权限/问题/取消、模型与计划」四类交互。本项目的协议层覆盖这些**数据结构**，但真实 MiniMax 侧的呈现效果**未在本机验证**，不要把协议层支持等同于端到端兼容。详见 [docs/protocol.md](docs/protocol.md)。

## 环境变量

| 变量 | 默认 | 说明 |
| :--- | :--- | :--- |
| `MINIMAX_ACP_CLI` | `mcode` | **本项目自定义**：覆盖 `--command`，便于宿主用 `env` 固定 CLI 路径 |
| `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` / `NO_PROXY` | 继承 | MiniMax CLI 会读取；`localhost`、`127.0.0.1`、`::1` 始终绕过 |
| `MINIMAX_DATA_DIR` / `MAVIS_DATA_DIR` / `__MAVIS_RUNTIME_DATA_DIR` / `__MAVIS_RUNTIME_PROFILE` | 未设置 | 若被设置，`doctor` 会警告 ACP runtime 可能读取不同登录态；桌面宿主通常在启动原生 ACP 时移除它们 |
| `NODE_OPTIONS` | 继承 | 含 `--require` shim 时 `doctor` 会警告；本机实测该注入与 `mcode acp` 无响应同时出现 |

## 测试与证据边界

```bash
make test    # 72 项全部通过
make race    # go test -race ./... 通过
make vet     # go vet ./... 通过
```

测试覆盖 JSON-RPC 帧解析、双向 Conn、pending/取消/反向请求、ndjson transport、ACP 类型与开放联合体、client / agent / bridge / CLI 子进程往返、权限策略、环境变量诊断、会话状态渲染，以及 `doctor` 的 `no_response` 路径。测试只使用内存 transport、Go echo agent 与 `/bin/sleep` 这类沉默夹具，**不触碰任何真实 MiniMax 会话**。

真实环境只做了只读探测，输出不含凭证：

| 证据 | 内容 |
| :--- | :--- |
| [docs/probes/cli-surface-20261108.json](docs/probes/cli-surface-20261108.json) | `mcode --version` = 0.6.3；`mcode acp --help` 确认原生 ACP server |
| [docs/probes/doctor-go-20261108.json](docs/probes/doctor-go-20261108.json) | Go doctor 报告：`status=no_response`、`promptSent=false`、`sessionCreated=false`、环境诊断 |
| [docs/probes/doctor-trace-20261108.ndjson](docs/probes/doctor-trace-20261108.ndjson) | 线材证据：只有一条发出的 `initialize`，无任何响应帧 |
| [docs/probes/initialize-no-response-20261108.json](docs/probes/initialize-no-response-20261108.json) | 46 秒时间线、5 种环境配置尝试、环境变量发现 |

**结论必须说清楚**：`mcode acp` 原生命令存在，但在这个 WorkBuddy 桌面沙箱里启动后 46 秒没有任何 stdout/stderr 帧，`initialize` 无响应。尝试过移除 PATH 中的 `brokered-bin`/`safe-bin`、清除 `NODE_OPTIONS` shim 注入、配置 `127.0.0.1:7890` 代理、隔离 `MINIMAX_DATA_DIR`，都未改善；原因未定位。这是**环境阻塞**，既不证明不兼容，也不证明兼容。真实 `agentCapabilities` / `authMethods` / 模型目录 / 流式与权限行为**全部未验证**；模拟测试、echo agent 和只读握手都不冒充真实兼容。

**从未向真实 MiniMax 会话发送过 `session/prompt`**，也从未在真实环境调用 `session/new`。要补齐真实链路，请在普通终端（非 IDE 沙箱）执行：

```bash
make probe            # 只读；成功后把报告提交到 docs/probes/ 并更新 index.json
```

## 目录结构

```
acp/                    ACP v1 协议层：JSON-RPC 2.0、ndjson transport、双向 Conn、类型与方法常量
client/                 ACP Client：进程启动、会话方法、反向请求处理、本地 fs/terminal 实现
agent/                  ACP Agent：方法分派、可取消的 prompt 上下文、Emitter、echo fixture
bridge/                 透明 ACP stdio 代理与 requestId 双向映射
internal/minimax/       MiniMax Code CLI 与宿主环境的只读检测（不读凭证）
cmd/minimax-acp/        CLI：doctor / chat / serve / bridge / version
examples/               Zed / JetBrains / 通用编辑器配置
docs/probes/            只读探针证据与索引
docs/protocol.md        方法映射、能力边界与开放对象说明
```

## 与同级项目的关系

`/Volumes/BAK3T/projects/acp/` 下的 `kimi`、`workbuddy`、`zcode` 各自是对应 agent 的 ACP 实现。本项目沿用同一套零依赖 Go ACP v1 协议层设计，但**不共享代码、不互相 import**，每个仓独立维护自己的协议适配与证据边界。MiniMax 的特殊点在于它**原生就是 ACP server**，因此本项目的重心是 Client / Bridge / Doctor，而不是重新实现 agent。

## 已知限制

- 真实 MiniMax 链路未验证（见上）；`doctor` 的 `acpServerHelp=true` 只表示 CLI 帮助文本里有 ACP server，不代表握手可用。
- `serve` 的 modes / configOptions 是 fixture，不能用来推断 MiniMax 的模型名或权限模式。
- `chat` 的 `--mode` / `--set-config` 只转发 agent 返回的 id，错误 id 会收到 agent 的错误响应并退出。
- `bridge` 是透明代理，不做能力改写或内容过滤；权限决策仍由上游 ACP client 负责。
- 仅在 macOS / Go 1.26.1 / mcode 0.6.3 上做过上述只读探测；Windows、云端 Open API 与 Zed / JetBrains 实际联动未实测。
