# kimi-acp

用 **Go 1.22+、零第三方依赖**实现的 Kimi Code ACP 工具集：驱动、诊断、代理 **Kimi Code 原生 `kimi acp` stdio server**，并提供一个确定性的 Go ACP agent 作为协议夹具。

**当前版本：`00.00.02`**（`bin/kimi-acp version` 可查；版本策略见[根 README](../README.md)）。

Kimi Code CLI（本机实测 2.1.1）通过 **`kimi acp` 原生提供 ACP server**。所以本项目不重复实现模型、工具或权限链路，而是补齐 Go 侧需要的东西：

| 形态 | 作用 |
| :--- | :--- |
| `doctor` | 只读体检：对真实 `kimi acp` 做 `initialize` + `session/list`，报协议版本、agentInfo、authMethods（默认不发 prompt、不建会话） |
| `chat` | 作为 ACP Client 驱动真实会话：流式输出、权限策略、会话加载/恢复、图片附件 |
| `serve` | Go 实现的确定性 ACP agent（echo fixture），给 Zed / JetBrains / 协议对拍用 |
| `bridge` | 透明 ACP stdio 代理，前置 `kimi acp`，双向转发帧，stderr 只写日志 |

```
ACP 宿主 (Zed / JetBrains / auto-run / ...)
   │  stdin/stdout  ACP v1 ndjson
   ▼
kimi-acp bridge ──────────────► kimi acp  (Kimi Code 原生 ACP server)
                                 │
kimi-acp doctor / chat ─────────┘   (直接作为 Client 连接)
```

## 构建

```bash
make build            # -> bin/kimi-acp（Go 二进制，不入 Git）
make test             # go test ./... -count=1
make vet              # go vet ./...
make race             # go test ./... -race -count=1
make doctor           # 只读握手：对真实 kimi acp 做 initialize + session/list
bin/kimi-acp version  # kimi-acp 00.00.01
```

测试共 43 个用例，覆盖 JSON-RPC 帧解析、ndjson transport、双向 Conn（pending/取消/反向请求）、ACP 类型、client / agent / bridge 往返，以及 CLI 子进程级测试（现编二进制跑 `version` / `doctor`）。测试只用内存 transport 与本地 echo agent，不触碰真实 Kimi 会话。

## 命令

### `doctor` — 只读体检（默认无副作用）

```bash
bin/kimi-acp doctor
bin/kimi-acp doctor --check-auth --include-titles
bin/kimi-acp doctor --command kimi --trace /tmp/doctor.ndjson
```

报告恒定输出 JSON（无 `--json` 旗标，这点与 minimax-acp 不同）。

- 只做「`initialize` →（agent 声明支持时）`session/list`」，**绝不发送 `session/prompt`**。
- `--check-auth` 会额外调用一次 `session/new` 验证认证状态（**有副作用**，可能创建会话）。本机未登录实测返回 `-32000 Authentication required`，未创建会话。
- `--include-titles` 才输出会话标题，默认打码，trace 中含标题的帧也会被移除。
- 通用旗标：`--command`（默认 `kimi`）、`--args`（默认 `acp`）、`--cwd`、`--trace`（ndjson 线材证据）、`--timeout`。

### `chat` — 驱动真实会话

```bash
bin/kimi-acp chat -p "解释这个仓库的 ACP 接入"        # 一次性 prompt
bin/kimi-acp chat                                    # 交互式 REPL
bin/kimi-acp chat --session <sessionId> --resume     # 恢复既有会话
bin/kimi-acp chat -p "..." --permission-mode prompt --allow-write --allow-terminal --image /abs/img.png
```

- `--permission-mode deny|allow|prompt`：默认 `deny`（最小权限）；`prompt` 在终端逐项询问反向的 `session/request_permission`。
- `--allow-write` / `--allow-terminal` 才会向 agent 声明并本地实现 `fs/write_text_file` / `terminal/*`，默认只读。
- `--raw` 打印原始 `session/update` JSON；`--stdin` 从 stdin 读一条 prompt。
- `--trace` 记录每帧 ndjson 证据；stdout 只给人看，协议永远走子进程 stdio。

### `serve` — Go ACP agent 夹具

```bash
bin/kimi-acp serve --name my-agent --trace /tmp/serve.ndjson
```

标准 ACP v1 stdio server（echo fixture）：`initialize` / `session/new` / `load` / `prompt` / `cancel`、流式 `session/update`、工具调用与一次反向权限请求。返回内容是**确定性夹具，不是 Kimi 的真实模型输出**。

### `bridge` — 透明代理

```bash
bin/kimi-acp bridge --trace /tmp/bridge.ndjson
```

上游（编辑器）与下游（`kimi acp`）的请求、响应、通知原样转发；stderr 只写诊断，stdout 永远是纯协议流。本配置纯靠旗标，无环境变量。

`00.00.02` 修复流式顺序：建会话、加载／恢复和 prompt 的响应发回宿主前，等待已收到的通知转发完成，保证最终文本与历史重放先于对应响应。新增阻塞通知 worker 的回归用例；只验证代理协议顺序，不代表真实模型链路已验证。

## 接入编辑器

```bash
make build
```

| 文件 | 用途 |
| :--- | :--- |
| [examples/zed-settings.json](examples/zed-settings.json) | Zed：原生 `kimi acp` + Go bridge + Go echo fixture 三个条目 |
| [examples/jetbrains-acp.json](examples/jetbrains-acp.json) | JetBrains 同上的三条目 |

模板里的 `/ABS/PATH/TO/...` 按本机填写。原生方式最简单（无需本项目）：`command: "kimi", args: ["acp"]`。

## 已验证的协议事实（kimi CLI 2.1.1，macOS，2026-11-08）

来自 [docs/probes/index.json](docs/probes/index.json) 的只读探测：

| 事实 | 证据 |
| :--- | :--- |
| `kimi acp` 说 ACP v1（JSON-RPC 2.0 / ndjson stdio），Go doctor 握手 `status=valid` | [doctor-go-20261108.json](docs/probes/doctor-go-20261108.json) |
| Kimi 声明 `loadSession`、`promptCapabilities.image/embeddedContext`、`mcpCapabilities.http/sse`、session 能力 `list/resume/close/delete/fork/additionalDirectories`、`auth.logout` | [kimi-acp-handshake-raw-20261108.json](docs/probes/kimi-acp-handshake-raw-20261108.json) |
| `session/new` 需要已认证的 CLI，否则 `-32000 Authentication required`；`session/list` 无需认证 | [doctor-check-auth-20261108.json](docs/probes/doctor-check-auth-20261108.json) |
| 未知方法 `-32601`（`data.method`）；无效 auth methodId `-32602`；`session/request_permission` 是 Agent→Client 方法 | [kimi-acp-methods-raw-20261108.json](docs/probes/kimi-acp-methods-raw-20261108.json) |
| `authMethods[].env` 是对象而非 schema 声明的 `EnvVariable[]`，本实现按 raw JSON 保留 | 同上 |
| bridge 转发：`initialize` 原样往返，未知方法透传后返回 `-32601` | [bridge-go-trace-20261108.ndjson](docs/probes/bridge-go-trace-20261108.ndjson) |
| serve echo agent 全流程：initialize → session/new → 流式 update → tool_call → 反向权限 → `end_turn` → session/load 重放 | [serve-go-trace-20261108.ndjson](docs/probes/serve-go-trace-20261108.ndjson) |

## 证据边界

- **从未向真实 Kimi 会话发送过 `session/prompt`**：模型输出、工具执行、Kimi 侧权限处理均未验证；echo agent 与模拟测试不冒充真实兼容。
- 未触发登录 / device-code 流程，CLI 保持未认证状态。
- 证据中的会话标题已打码；含标题的 `session/list` 原始帧已从 trace 中移除。
- 要补齐真实链路，请在已登录 `kimi` 的终端运行 `make doctor` 与 `chat`，并把新证据提交到 `docs/probes/` 后更新 `index.json`。

## 目录结构

```
acp/               ACP v1 协议层：JSON-RPC 2.0、ndjson transport、双向 Conn、类型与方法常量
client/            ACP Client：子进程启动、会话方法、反向请求处理、本地 fs/terminal 实现
agent/             ACP Agent：方法分派、可取消的 prompt 上下文、echo fixture
bridge/            透明 ACP stdio 代理
cmd/kimi-acp/      CLI 入口：doctor / chat / serve / bridge / version
docs/probes/       只读探针证据与索引（index.json）
examples/          Zed / JetBrains 编辑器配置模板
```

## 与同级项目的关系

本仓库下 [minimax](../minimax/)、[workbuddy](../workbuddy/)、[zcode](../zcode/) 各自是对应 agent 的 ACP 实现。本项目沿用同一套零依赖 Go ACP v1 协议层设计，但**不共享代码、不互相 import**，独立维护自己的协议适配与证据边界。
