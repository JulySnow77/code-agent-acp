# claude-acp-bridge

把 **Claude Code** 接入 **ACP（Agent Client Protocol）v1** 的 Go 适配器，单一二进制、零第三方依赖。

## 为什么需要适配器

Claude Code 本身**不原生支持 ACP**：它没有 `claude acp` 这样的入口。官方适配器 `@agentclientprotocol/claude-agent-acp`（以及 Zed 维护的 `@zed-industries/claude-code-acp`）的做法是「起一个 ACP server，内部通过 Agent SDK 驱动 Claude Code，把两边消息互相翻译」。

Claude Code 的机器可读入口是 **stream-json 双向模式**：

```bash
claude -p --input-format stream-json --output-format stream-json \
       --verbose --permission-prompt-tool stdio --permission-mode manual
```

stdout 是一行一个 JSON 对象的事件流（`system`/`user`/`assistant`/`result`/`control_request`…），stdin 接受用户消息与 `control_response`。本模块用 Go 实现同样的翻译架构，遵循仓库里其余五个模块的工程约定：

```
ACP 宿主 (Zed / JetBrains / auto-run / ...)
   │  stdin/stdout  ACP v1 ndjson (JSON-RPC 2.0)
   ▼
claude-acp-bridge serve
   │  ├─ ACP server：initialize / session/* / session/update / request_permission
   │  └─ stream-json client：每个 session 一个 claude 子进程 + 控制协议
   ▼  子进程 stdin/stdout  纯 NDJSON（无 jsonrpc 信封）
claude（由 `claude -p ...` 启动）
```

宿主看到的是普通 ACP agent；模型调用、工具执行、会话持久化与凭据仍完全由 Claude Code 管理（使用你本机已登录的 Claude 账号与计费路径）。

### 与 npx 方案对照

官方适配器通常这样配置：

```json
{ "command": "npx", "args": ["-y", "@agentclientprotocol/claude-agent-acp"] }
```

Go 版换成编译好的二进制（无需 Node / npm，启动更快、可离线、可审计）：

```json
{ "command": "/ABS/PATH/claude/bin/claude-acp-bridge", "args": ["serve"] }
```

## 构建与安装

```bash
make -C claude build          # → claude/bin/claude-acp-bridge
claude/bin/claude-acp-bridge version
claude/bin/claude-acp-bridge doctor    # 只读自检：claude --version + argv 校验；不发 prompt
```

要求本机已安装并登录 Claude Code（`claude --version`），Go 1.22+（本仓库用 1.26.1 验证）。协议事实基于本机 **claude-cli 2.1.322** 的 `--help` 与 stream-json 行为核对，详见 [docs/protocol.md](docs/protocol.md)。

### 编辑器接入

Zed（`settings.json`，片段见 [examples/zed-settings.json](examples/zed-settings.json)）：

```json
{
  "agent_servers": {
    "Claude Code (Go ACP adapter)": {
      "type": "custom",
      "command": "/ABS/PATH/code-agent-acp/claude/bin/claude-acp-bridge",
      "args": ["serve"],
      "env": { "CLAUDE_BIN": "claude", "CLAUDE_ACP_PERMISSION_MODE": "manual" }
    }
  }
}
```

JetBrains / 通用 ACP 宿主见 [examples/jetbrains-acp.json](examples/jetbrains-acp.json)。把 `command` 换成绝对路径，宿主通常不接受相对路径。

## 子命令与配置

| 子命令 | 作用 |
| :--- | :--- |
| `serve`（默认） | 以 ACP stdio agent 运行；stdout **只**输出协议帧，诊断走 stderr |
| `doctor` | 只读自检：`claude --version` + stream-json argv 校验，输出 JSON，**不调用模型** |
| `version` | 输出 `claude-acp-bridge 00.00.01` |

`serve` 参数（也可用同名环境变量，命令行优先）：

| 参数 | 环境变量 | 默认 | 说明 |
| :--- | :--- | :--- | :--- |
| `--claude-bin` | `CLAUDE_BIN` | `claude` | Claude CLI 路径 |
| `--claude-arg`（可重复） | — | 空 | 追加到每次 CLI 启动，例如 `--effort high` |
| `--model` | `CLAUDE_ACP_MODEL` | 空 | 默认模型 id |
| `--permission-mode` | `CLAUDE_ACP_PERMISSION_MODE` | `manual` | `manual` / `acceptEdits` / `auto` / `bypassPermissions` / `plan` / `dontAsk` |
| `--cwd` | `CLAUDE_ACP_CWD` | 进程 cwd | `session/new` 缺省工作目录 |
| `--turn-timeout` | `CLAUDE_ACP_TURN_TIMEOUT` | `30m` | 单次 prompt 预算，超时自动 `interrupt` |
| `--permission-fallback` | `CLAUDE_ACP_PERMISSION_FALLBACK` | `decline` | 客户端不会回答 `session/request_permission` 时的决策 |
| `--wire-log` | `CLAUDE_ACP_WIRE_LOG` | 空 | 录制两侧原始帧为 JSONL（含工具输出，注意敏感信息） |

诊断开关：`CLAUDE_ACP_VERBOSE=1` 把子进程 stderr 转发到 stderr。

## ACP ↔ stream-json 映射

| ACP（客户端 → 桥） | Claude stream-json（桥 → CLI） | 说明 |
| :--- | :--- | :--- |
| `initialize` | — | 直接返回 ACP `agentCapabilities`；桥自身持有版本 |
| `session/new` | 启动 `claude ... --session-id <uuid>` | sessionId 是桥生成的 UUID，无需先发 prompt |
| `session/load` | 启动 `claude ... --resume <sessionId>` | 续接已有会话 |
| `session/prompt` | `{"type":"user","message":{"role":"user","content":[...]}}` | ContentBlock[] → Anthropic content（text / image） |
| `session/cancel` | `control_request{subtype:"interrupt"}` | 立即返回 `stopReason:cancelled` |
| `session/set_mode` | `control_request{subtype:"set_permission_mode"}` | `ask`→`manual`、`auto`→`acceptEdits`、`untrusted`→`plan` |
| `session/set_model`（扩展） | `control_request{subtype:"set_model"}` | 与 zcode/codex 桥同名扩展 |
| `session/set_config_option` | 同上 | 支持 `configId=model` / `mode` |
| `authenticate` | — | 返回 `-32601`：Claude Code 自行管理凭据 |

| Claude stream-json（CLI → 桥） | ACP（桥 → 客户端） |
| :--- | :--- |
| `assistant.content[]` 的 `text` / `thinking` | `agent_message_chunk` / `agent_thought_chunk` |
| `stream_event` 的 `text_delta` / `thinking_delta` | 同上（实时增量，完整块到达时去重） |
| `assistant.content[]` 的 `tool_use` | `tool_call_update`（pending → in_progress） |
| `user.content[]` 的 `tool_result` | `tool_call_update`（completed / failed + rawOutput） |
| `system/init` | 捕获 session_id / model / tools 到 `_meta` |
| `result.usage` / `result.modelUsage` | `usage_update`（used / size） |
| `control_request{subtype:"can_use_tool"}` | `session/request_permission`，选项映射为 allow/deny 行为 |
| `result{subtype:success}` | `session/prompt` 的 `stopReason: end_turn` |
| `result{subtype:error_max_turns}` | `stopReason: max_tokens` |
| `result{subtype:error_during_execution,is_error}` | 推错误文本 + 返回 JSON-RPC error（不伪装 end_turn） |

协议事实（事件类型、字段、`control_request` 信封、`result` 判别值）来自本机 **claude-cli 2.1.322** 的 `--help` 与 stream-json 行为核对，并参考多个社区实测协议文档，详见 [docs/protocol.md](docs/protocol.md)。

## 设计要点

- **每个 ACP session 一个常驻 CLI 进程**：Claude 不像 Codex app-server 那样在一个进程里多路复用 thread；一条会话对应一个 `claude` 子进程。
- **双向嵌套 RPC**：CLI 的 `can_use_tool` 是 server→client 请求，桥再向 ACP 客户端发 `session/request_permission`。ACP reader 常驻并按 id 路由回复，因此在 `session/prompt` 仍在执行时也能完成审批，不死锁。
- **会话 id 前置**：桥生成 UUID 并经 `--session-id` 传给 CLI，`session/new` 无需先发 prompt 即可返回 sessionId（CLI 的 `system/init` 要首条用户消息后才出现）。
- **错误可见**：`result` 失败时既推文本也返回 JSON-RPC error（沿用 zcode/codex 策略）；未知 `control_request` 一律保守拒绝而非放行。
- **stdout 纪律**：`serve` 期间 stdout 只有 ACP 帧；所有日志、CLI stderr、`doctor` 输出都走 stderr 或非 ACP 子命令。
- **独立模块**：自带 `go.mod` / `Makefile`，不 import 兄弟模块，零第三方依赖。版本号 `00.00.01`，唯一事实来源是 `internal/protocol/frame.go` 的 `VersionString`。

## 测试

```bash
make -C claude test     # 单元 + fake 端到端（真实子进程 e2e）
make -C claude race     # go test -race ./...
make -C claude vet      # go vet ./...
make -C claude doctor   # 真机只读自检（不调用模型）
```

覆盖：握手与版本协商、会话创建与 CLI 启动参数、流式文本/思考、tool_use/tool_result、usage、权限四种选项 + 客户端不支持时的 fallback、取消、失败可见、max_tokens、`set_mode`/`set_model`、`session/load` 续接、输入校验、stream-json 无 jsonrpc 帧格式；`test/e2e` 用 Python 假 CLI 驱动**编译后的真实二进制**，验证子进程与管道链路。

真机验证（claude-cli 2.1.322 / macOS）：`doctor` 读到 CLI 版本并校验 argv，**未向模型发送任何 prompt**（无额度消耗）。

## 已知限制

- `session/load` 用 `--resume` 续接会话；CLI 在 stream-json 下不会像 Codex `thread/resume` 那样把完整历史作为事件回吐，因此桥不额外重放 transcript（续接的模型上下文仍完整）。
- `can_use_tool` 的 `allow_always` 目前映射为本次 `allow`（带原始 input），未构造 `updatedPermissions` 持久化规则；如需会话级持久授权请用 `--permission-mode acceptEdits`。
- `bypassPermissions` / `dontAsk` 等 CLI 原生模式可作为 `--permission-mode` 传入，但 ACP `session/set_mode` 只暴露 `ask`/`auto`/`untrusted` 三种（与其余桥一致）；直接传 CLI 模式绕过审批，请仅在可信环境使用。
- 未实现 `authenticate`、`session/list`、`session/fork`、`fs/*`、`terminal/*`：Claude Code 自行管理凭据与文件。
- CLI 升级可能改变 stream-json 事件字段或控制子类型；用 `doctor` 与 `docs/protocol.md` 复核，未知事件/子类型会被安全忽略或保守拒绝，不会让流失败。
