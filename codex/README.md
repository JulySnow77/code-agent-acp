# codex-acp-bridge

把 **OpenAI Codex CLI** 接入 **ACP（Agent Client Protocol）v1** 的 Go 适配器，单一二进制、零第三方依赖。

## 为什么需要适配器

Codex CLI 本身**不原生支持 ACP**：它没有 `codex acp` 这样的入口，底层通信是自己的 **app-server JSON-RPC 协议**（stdio、ndjson、省略 `jsonrpc:"2.0"` 头的 "JSON-RPC lite"）。官方 TypeScript 适配器 `@agentclientprotocol/codex-acp` 的做法是「起一个 ACP server，内部驱动 Codex，把两边消息互相翻译」；本模块是同样的架构，只是用 Go 实现，并与仓库里其余四个模块保持一致的工程约定：

```
ACP 宿主 (Zed / JetBrains / auto-run / ...)
   │  stdin/stdout  ACP v1 ndjson (JSON-RPC 2.0)
   ▼
codex-acp-bridge serve
   │  ├─ ACP server：initialize / session/* / session/update / request_permission
   │  └─ app-server client：initialize / thread/start / turn/start / 通知流
   ▼  子进程 stdin/stdout  app-server JSON-RPC lite
codex app-server（由 `codex app-server` 启动）
```

宿主看到的是一个普通 ACP agent；Codex 的 thread / turn / item 概念、认证、会话持久化、工具执行仍完全由 Codex 自己管理。

### 与 npx 方案对照

官方适配器通常这样配置：

```json
{ "command": "npx", "args": ["-y", "@agentclientprotocol/codex-acp"] }
```

Go 版换成编译好的二进制（无需 Node / npm，启动更快、可离线、可审计）：

```json
{ "command": "/ABS/PATH/codex/bin/codex-acp-bridge", "args": ["serve"] }
```

## 构建与安装

```bash
make -C codex build          # → codex/bin/codex-acp-bridge
codex/bin/codex-acp-bridge version
codex/bin/codex-acp-bridge doctor    # 只读自检：起 app-server、握手、打印 JSON；不发 prompt
```

要求本机已安装并登录 Codex CLI（`codex --version`），Go 1.22+（本仓库用 1.26.1 验证）。

### 编辑器接入

Zed（`settings.json`，片段见 [examples/zed-settings.json](examples/zed-settings.json)）：

```json
{
  "agent_servers": {
    "Codex (Go ACP adapter)": {
      "type": "custom",
      "command": "/ABS/PATH/code-agent-acp/codex/bin/codex-acp-bridge",
      "args": ["serve"],
      "env": { "CODEX_BIN": "codex" }
    }
  }
}
```

JetBrains / 通用 ACP 宿主见 [examples/jetbrains-acp.json](examples/jetbrains-acp.json)。把 `command` 换成绝对路径，宿主通常不接受相对路径。

## 子命令与配置

| 子命令 | 作用 |
| :--- | :--- |
| `serve`（默认） | 以 ACP stdio agent 运行；stdout **只**输出协议帧，诊断走 stderr |
| `doctor` | 只读自检：`codex --version`、`initialize` + `initialized` 握手、输出 JSON，**不创建 thread、不调用模型** |
| `version` | 输出 `codex-acp-bridge 00.00.01` |

`serve` 参数（也可用同名环境变量，命令行优先）：

| 参数 | 环境变量 | 默认 | 说明 |
| :--- | :--- | :--- | :--- |
| `--codex-bin` | `CODEX_BIN` | `codex` | Codex CLI 路径 |
| `--codex-arg`（可重复） | — | 空 | 追加到 `codex app-server` 之后，例如 `-c model=...` |
| `--approval-policy` | `CODEX_ACP_APPROVAL_POLICY` | `on-request` | `on-request` / `never` / `untrusted` |
| `--sandbox` | `CODEX_ACP_SANDBOX` | `workspace-write` | `read-only` / `workspace-write` / `danger-full-access` |
| `--model` | `CODEX_ACP_MODEL` | 空 | 默认模型 id，作用于 thread/start 与 turn/start |
| `--cwd` | `CODEX_ACP_CWD` | 进程 cwd | `session/new` 缺省工作目录 |
| `--turn-timeout` | `CODEX_ACP_TURN_TIMEOUT` | `30m` | 单次 prompt 预算，超时自动 `turn/interrupt` |
| `--permission-fallback` | `CODEX_ACP_PERMISSION_FALLBACK` | `decline` | 客户端不会回答 `session/request_permission` 时的决策 |
| `--wire-log` | `CODEX_ACP_WIRE_LOG` | 空 | 录制两侧原始帧为 JSONL（含工具输出，注意敏感信息） |

诊断开关：`CODEX_ACP_VERBOSE=1` 把子进程 stderr 转发到 stderr。

## ACP ↔ app-server 映射

| ACP（客户端 → 桥） | app-server（桥 → Codex） | 说明 |
| :--- | :--- | :--- |
| `initialize` | `initialize` + `initialized` | 启动时完成握手；返回 `agentCapabilities` / `_meta.codex` |
| `session/new` | `thread/start` | `cwd`、`sandbox`、`approvalPolicy`、`model`、`mcpServers`→`config.mcp_servers`；**sessionId 就是 thread.id** |
| `session/load` | `thread/resume` | 把 thread 历史重放为 `session/update`（user/agent message、tool_call） |
| `session/prompt` | `turn/start` | ContentBlock[] → UserInput[]（text / image / audio / resource） |
| `session/cancel` | `turn/interrupt` | 通知或请求两种形态都接受；返回 `stopReason: cancelled` |
| `session/set_mode` | 下一次 `turn/start.approvalPolicy` | `ask`→`on-request`、`auto`→`never`、`untrusted`→`untrusted` |
| `session/set_model`（扩展） | `turn/start.model` | 与 zcode-acp-bridge 同名扩展 |
| `session/set_config_option` | 同上 | 支持 `configId=model` / `approval_policy` |
| `authenticate` | — | 返回 `-32601`：Codex 自行管理 `~/.codex` 凭据 |

| app-server（Codex → 桥） | ACP（桥 → 客户端） |
| :--- | :--- |
| `item/agentMessage/delta` | `agent_message_chunk` |
| `item/reasoning/*Delta` | `agent_thought_chunk` |
| `item/started` / `item/completed` | `tool_call` / `tool_call_update`（commandExecution / fileChange / mcpToolCall / dynamicToolCall） |
| `item/*/outputDelta` | `tool_call_update`（实时输出） |
| `item/plan/delta` + plan item 完成 | `plan` |
| `item/commandExecution/requestApproval`、`item/fileChange/requestApproval` | `session/request_permission`，选项映射为 `accept` / `acceptForSession` / `decline` / `cancel` |
| `thread/tokenUsage/updated` | `usage_update` |
| `turn/completed` | `session/prompt` 的 `stopReason`：`end_turn` / `cancelled`；失败时推错误文本并返回 JSON-RPC error |
| `error`（`willRetry=false`） | 错误文本进 transcript，并在 `turn/completed` 时带上 |

协议事实（方法名、字段、决策枚举）来自 `codex app-server generate-json-schema`，基于本机 **codex-cli 0.167.0** 导出，详见 [docs/protocol.md](docs/protocol.md)。

## 设计要点

- **双向嵌套 RPC**：Codex 的审批是 server→client 请求，桥再向 ACP 客户端发 `session/request_permission`。ACP reader 常驻并按 id 路由回复，因此在 `session/prompt` 仍在执行时也能完成审批，不会丢帧或死锁。
- **提前注册 turn**：`turnState` 在 `turn/start` 发出**之前**创建，因为 app-server 可能先推 `turn/started` 再回 RPC 响应。
- **错误可见**：模型/后端失败不会伪装成 `end_turn`，既推文本也返回 JSON-RPC error（沿用 zcode 模块的策略）。
- **stdout 纪律**：`serve` 期间 stdout 只有 ACP 帧；所有日志、Codex stderr、`doctor` 输出都走 stderr 或非 ACP 子命令。
- **独立模块**：自带 `go.mod` / `Makefile`，不 import 兄弟模块，零第三方依赖。版本号 `00.00.01`，唯一事实来源是 `internal/protocol/frame.go` 的 `VersionString`。

## 测试

```bash
make -C codex test     # 单元 + fake 端到端 + 真实子进程 e2e
make -C codex race     # go test -race ./...
make -C codex vet      # go vet ./...
make -C codex doctor   # 真机握手自检（只读）
```

覆盖：握手与版本协商、会话创建/MCP 转换、流式文本与工具调用、usage、权限四种选项 + 客户端不支持时的 fallback、取消、失败可见、`turn/start` 失败、`session/load` 历史重放、输入校验、JSON-RPC-lite 帧格式、请求关联与关闭唤醒；`test/e2e` 用 Python 假 app-server 驱动**编译后的真实二进制**，验证子进程与管道链路。

真机验证（2026-11-09，codex-cli 0.167.0 / macOS）：`doctor` 握手成功，返回 `userAgent`、`codexHome`、`platformFamily/Os`；**未向模型发送任何 prompt**（无额度消耗）。

## 已知限制

- `mcpServers` 的 stdio 配置完整映射；`http`/`sse` 只映射 `url` + `http_headers`，取决于 Codex 当前版本的 MCP transport 支持。
- `item/permissions/requestApproval`（权限档案类请求）不翻译，显式拒绝，避免凭空授予沙箱权限。
- ACP `audio` 用 `data:` URI 传给 app-server；若目标版本只接受本地文件，请改用带 `uri` 的块。
- 未实现 `session/list`、`session/close`、`session/fork`、`logout`、`fs/*`、`terminal/*`：Codex 自己读写文件与管理会话，桥不代劳。
- Codex 升级可能改变 app-server 方法名/字段；用 `doctor` 和 `docs/protocol.md` 里的 schema 导出命令复核。
