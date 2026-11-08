# WorkBuddy 本机 ACP 网关：协议记录

记录时间 2026-11-08；环境 macOS、WorkBuddy 2.165.0、`~/.workbuddy/sessions/72069.json` 的 interactive 会话。所有探测均为**只读**：`connect`、`initialize`、`session/new`、`session/load` 的握手/错误响应；**没有发送 `session/prompt`**，也没有向用户可见对话投递任何消息。一次性凭证未记录。

## 会话发现

`~/.workbuddy/sessions/<pid>.json`：

```json
{
  "pid": 72069,
  "sessionId": "1434eb87-3aff-48ae-b320-d940f02f3ff6",
  "cwd": "/Volumes/BAK3T/projects/skills/auto-run",
  "kind": "interactive",
  "url": "http://127.0.0.1:64250",
  "endpoint": "http://127.0.0.1:64250",
  "mode": "local",
  "version": "2.165.0",
  "lastHeartbeat": 1794109499722
}
```

本机文件直接给出 `url` / `endpoint`，比社区项目在 Windows 上用 `netstat -ano` 按 pid 反查端口更直接；端口仍会随会话回收变化，因此每次启动重新读取。`lsof -nP -iTCP@127.0.0.1 -sTCP:LISTEN` 独立确认 `Electron pid=72069` 监听 `127.0.0.1:64250`。桥只接受纯 `http://127.0.0.1:<port>`（或 localhost/::1）且无 path/query 的端点。

## 握手

```
POST /api/v1/acp/connect   {}   (loopback 免密)
  -> 200 {"connectionId":"...","sessionToken":"..."}

POST /api/v1/acp
  Content-Type: application/json
  Accept: application/json, text/event-stream      # 两个 MIME 缺一不可
  Acp-Connection-Id: <connectionId>
  acp-session-token: <sessionToken>
  x-codebuddy-request: 1
  body: {"jsonrpc":"2.0","id":1,"method":"initialize","params":{...}}
  -> 200 text/event-stream
     :ok

     event: message
     data: {"jsonrpc":"2.0","id":1,"result":{...}}
```

只带 `Accept: text/event-stream` 或 `Accept: application/json` 都返回 `406`，错误信息为 `Not Acceptable: Client must accept both application/json and text/event-stream`。这是本机实测与社区 README 描述最明显的差异。

`initialize` 实测 result：

```json
{
  "protocolVersion": 1,
  "agentCapabilities": {
    "promptCapabilities": {"image": true, "embeddedContext": true},
    "mcpCapabilities": {"http": true, "sse": true},
    "loadSession": true,
    "delegateToolsSupport": true,
    "mainAgentSupport": false,
    "multitaskSupport": true,
    "metaPromptSupport": true
  },
  "authMethods": [{"id": "iOA"}, {"id": "external"}, {"id": "internal"}, {"id": "selfhosted"}]
}
```

`loadSession: true` 而没有 `sessionCapabilities.resume`，所以桥向上游声明 `loadSession`，宿主恢复会话时会走 `session/load`（auto-run 的 `Connection.setup` 正是按这个优先级选择方法）。

## 会话与 writer 占用

`session/new {cwd, mcpServers:[]}` 返回 SSE，先推一条 `session/update` 的 `config_option_update`（含 `mode` = Permission Mode 等可选项），后给 id 匹配的 result；返回的 sessionId 是持久会话 id。也就是说本机网关的 `session/new` 是**为持久会话建立外部 writer**，不是创建完全隔离的新上下文。

`session/load {sessionId, cwd, mcpServers:[]}` 在当前正被使用的会话上返回：

```json
{"jsonrpc":"2.0","id":2,"error":{"code":-32000,
  "message":"Persistent Session already has an external writer",
  "data":{"reason":"writer_occupied"}}}
```

这与社区项目记录的「同一会话一次只能处理一条消息」「忙闲检测」一致。本桥把该错误原样作为 ACP error 上抛，不自动重试、不静默换会话。

## 与官方云端 Open API 的差异

官方 [Open API](https://open.workbuddy.cn/docs/openapi) 面向**云端任务**：先创建/查询任务取得 `link` + `token`，再用「GET SSE 长连接收 + POST JSON-RPC 发」的双通道，靠 `Acp-Connection-Id` 关联，`session/load` 的 `sessionId` 是 `task_id`。本机 loopback 网关把 SSE 直接挂在 POST 响应上，无需单独 GET 通道，也不需要长期密钥。本项目实现的是本机形态；云端双通道不在当前范围。

## 方法映射

| 上游 ACP stdio | 下游 JSON-RPC | 备注 |
|---|---|---|
| `initialize` | `initialize` | 能力透传并补 `loadSession: true` |
| `session/new` | `session/new {cwd, mcpServers:[]}` | 若未返回 sessionId，回退到发现文件中的 sessionId |
| `session/load` / `session/resume` | `session/load {sessionId, cwd, mcpServers:[]}` | `mcpServers` 必为数组 |
| `session/prompt` | `session/prompt {sessionId, prompt:[ContentBlock]}` | SSE `agent_message_chunk` / `tool_call` / `session_end` → `session/update` |
| `session/cancel` | `session/cancel` | 通知或请求都接受 |
| `session/request_permission` | 网关发起的同名请求 | `deny` 回 `cancelled`，`client` 转发上游决策 |
| `session/set_mode` | 无 | plan 模式在桥本地拒绝写/执行权限 |
| `session/set_config_option` | 无 | 回显 `currentValue`；`modelSelectionVerified=false`，不冒充切模型 |

## 证据边界

只读握手证明：会话发现、端点可达、`connect` 凭证下发、`initialize` 协商出 ACP v1 能力。它**不证明** `session/prompt` 的真实模型回复、工具调用、权限回包、取消或用量统计。真实 `session/prompt` 探针需要用户指定专用空闲会话并显式授权；当前交互会话正被外部 writer 占用，不得擅自投递。

## Go 实现复核（2026-11-08 同日）

桥的实现语言由 Python 0.1.0 改为 Go 0.2.0 后，用 Go 客户端重新对上述结论做了**同样只读**的复核：

- `bin/workbuddy-acp-bridge doctor --json` → `status=valid`、`protocolVersion=1`、`loadSession=true`、`promptSent=false`，`agentCapabilities` 与 Python 版一致（`delegateToolsSupport`、`metaPromptSupport`、`multitaskSupport`、`mcpCapabilities.http/sse`、`promptCapabilities.image/embeddedContext`），`authMethods` 为 `iOA / external / internal / selfhosted`。证据 `docs/probes/doctor-go-20261108.json`。
- `bin/workbuddy-acp-bridge serve --mode plan` 只发一个 `initialize` 帧，Go stdio server 通过 `LocalBackend.Capabilities()` 触发真实 `connect` + `initialize`，返回帧中能力透传、`_meta.workbuddy.backend=local`、`modelSelectionVerified=false`。证据 `docs/probes/initialize-stdio-go-20261108.ndjson`，stderr 只有启动日志一行。

复核再次确认：必须同时声明 `Accept: application/json, text/event-stream`；响应 SSE 挂在 POST 上；凭证头 `Acp-Connection-Id` / `acp-session-token` / `x-codebuddy-request: 1` 有效。**仍未发送 `session/prompt`**，本节不扩大证据边界。
