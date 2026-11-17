# Codex app-server ↔ ACP v1 协议事实与映射

本文件是 `codex-acp-bridge` 的协议依据。事实来源为本机 **codex-cli 0.167.0** 导出的官方 schema，而非推测：

```bash
SCHEMA_DIR=$(mktemp -d)
codex app-server generate-json-schema --out "$SCHEMA_DIR"
# 关键文件：
#   $SCHEMA_DIR/v1/InitializeParams.json, InitializeResponse.json
#   $SCHEMA_DIR/v2/ThreadStartParams.json, TurnStartParams.json, TurnInterruptParams.json
#   $SCHEMA_DIR/ClientRequest.json, ServerRequest.json, ServerNotification.json
#   $SCHEMA_DIR/CommandExecutionRequestApprovalResponse.json
#   $SCHEMA_DIR/FileChangeRequestApprovalResponse.json
#   $SCHEMA_DIR/codex_app_server_protocol.v2.schemas.json (definitions 汇总)
```

`generate-ts` / `generate-json-schema` 的产物与生成它的 Codex 版本严格对应，升级 Codex 后应重新导出核对。

## 1. 传输与信封

| 侧 | 传输 | 信封 | 是否有 `jsonrpc` 成员 |
| :--- | :--- | :--- | :--- |
| ACP（宿主 ↔ 桥） | stdio，一行一个 JSON 对象 | 标准 JSON-RPC 2.0 | **必须** `"jsonrpc":"2.0"` |
| app-server（桥 ↔ Codex） | 子进程 stdio，一行一个 JSON 对象，`codex app-server` 默认即此模式 | JSON-RPC **lite**（`app-server-protocol/jsonrpc_lite.rs`） | **无**（但收到带该成员的请求也能容忍） |

四种消息形状（app-server 侧）：

```
请求      {"id":1,"method":"thread/start","params":{...}}
响应      {"id":1,"result":{"thread":{...}}}   或  {"id":1,"error":{"code":-32600,"message":"..."}}
通知      {"method":"item/agentMessage/delta","params":{...}}
反向请求  {"id":12,"method":"item/commandExecution/requestApproval","params":{...}}
```

字段名一律 camelCase。桥对两侧都用 32 MiB 的单帧上限（`bufio.Scanner`），写帧串行加锁，诊断只写 stderr。

## 2. 生命周期

```
桥 → Codex   {"id":1,"method":"initialize","params":{"clientInfo":{"name":"codex-acp-bridge",
              "title":"Codex ACP Bridge (Go)","version":"00.00.01"},
              "capabilities":{"experimentalApi":false,"optOutNotificationMethods":[...]}}}
Codex → 桥   {"id":1,"result":{"userAgent":"...","codexHome":"...","platformFamily":"unix","platformOs":"macos"}}
桥 → Codex   {"method":"initialized"}            ← 通知，无 params；必发
桥 → Codex   {"id":2,"method":"thread/start","params":{"cwd":...,"approvalPolicy":...,"sandbox":...}}
Codex → 桥   {"id":2,"result":{"thread":{"id":"..."}}}   + 通知 "thread/started"
桥 → Codex   {"id":3,"method":"turn/start","params":{"threadId":...,"input":[{"type":"text","text":...}]}}
Codex → 桥   {"id":3,"result":{"turn":{"id":...}}}      + 通知 "turn/started"
Codex → 桥   一串 item/*、*Delta 通知 …… 最后 "turn/completed"
```

- `initialize` 每条连接只能发一次，之后必须发 `initialized`；之前发别的请求会得到 "Not initialized"。
- `thread/start` 会自动订阅该 thread 的 turn/item 事件；`thread/resume` 用于恢复历史。
- `turn/start` 的响应**不含**模型输出，输出全在后续通知里。
- 终止：`turn/completed`（`turn.status` = `completed` / `interrupted` / `failed` / `inProgress`），或主动 `turn/interrupt`。

### 已确认的关键 schema

| 对象 | 必填 / 主要字段 |
| :--- | :--- |
| `InitializeParams` (v1) | `clientInfo{name,title,version}`；`capabilities{experimentalApi, optOutNotificationMethods, ...}` 可选 |
| `InitializeResponse` (v1) | `userAgent`, `codexHome`, `platformFamily`, `platformOs` |
| `ThreadStartParams` (v2) | 可空；支持 `cwd`, `model`, `approvalPolicy`, `sandbox`, `config`, `ephemeral`, `baseInstructions` … |
| `ThreadStartResponse` | `thread`, `cwd`, `model`, `modelProvider`, `approvalPolicy`, `sandbox` … |
| `ThreadResumeParams` | 必填 `threadId`；可选 `cwd`, `config`, `model`, `approvalPolicy` … |
| `TurnStartParams` | **必填** `threadId`, `input[]`；可选 `model`, `approvalPolicy`, `cwd`, `effort` … |
| `TurnStartResponse` | `turn`（`id`, `status`, `items`, `error` …） |
| `TurnInterruptParams` | **必填** `threadId`, `turnId` |
| `TurnCompletedNotification` | `threadId`, `turn{id,status,error,...}` |

`UserInput` 变体：`{type:"text",text}`、`{type:"image",url|fileId}`、`{type:"localImage",path}`、`{type:"audio",url|fileId}`、`{type:"localAudio",path}`、`{type:"skill"|"mention",name,path}`。

`ThreadItem.type` 取值包括 `userMessage`、`agentMessage`、`reasoning`、`plan`、`commandExecution`、`fileChange`、`mcpToolCall`、`dynamicToolCall`、`functionCallOutput` 等；`item/started` / `item/completed` 的 `params.item` 即该联合类型。

### 通知方法名（ServerNotification.json 摘录）

```
error, warning, thread/started, thread/status/changed, thread/tokenUsage/updated,
turn/started, turn/completed, turn/diff/updated, turn/plan/updated,
item/started, item/completed, item/agentMessage/delta, item/plan/delta,
item/reasoning/textDelta, item/reasoning/summaryTextDelta,
item/commandExecution/outputDelta, command/exec/outputDelta,
item/fileChange/outputDelta, item/mcpToolCall/progress, ...
```

增量通知统一带 `threadId`, `turnId`, `itemId`, `delta`。`thread/tokenUsage/updated` 带 `tokenUsage.total.totalTokens` 与 `tokenUsage.modelContextWindow`。

## 3. 反向请求（审批）

`ServerRequest.json` 里与桥相关的方法：

| 方法 | 关键 params | 响应 |
| :--- | :--- | :--- |
| `item/commandExecution/requestApproval` | `threadId`, `turnId`, `itemId`, `command`, `commandActions`, `cwd`, `reason`, `approvalId`, `kind` | `{decision: ...}` |
| `item/fileChange/requestApproval` | `threadId`, `turnId`, `itemId`, `grantRoot`, `reason` | `{decision: ...}` |
| `item/permissions/requestApproval` | 权限档案（结构化 grant） | `{permissions, scope, ...}` |
| `execCommandApproval` / `applyPatchApproval` | 旧版审批 | `{decision: ReviewDecision}` |

决策枚举（来自 schema，非猜测）：

- `CommandExecutionApprovalDecision` / `FileChangeApprovalDecision`：
  `accept`、`acceptForSession`、`acceptWithExecpolicyAmendment{...}`、`applyNetworkPolicyAmendment{...}`、`decline`、`cancel`。
- 旧版 `ReviewDecision`：`approved`、`approved_for_session`、`denied`、`abort`、`timed_out`，以及带 execpolicy/network 修正案的复合变体。

桥的映射：

| ACP `session/request_permission` 选项 | v2 decision | 旧版 decision |
| :--- | :--- | :--- |
| `allow_once` | `accept` | `approved` |
| `allow_always` | `acceptForSession` | `approved_for_session` |
| `reject_once` | `decline` | `denied` |
| `reject_always` / `outcome=cancelled` | `cancel` | `abort` |

`item/permissions/requestApproval` 不翻译（显式返回错误，走 `decline` 兜底），因为它的响应需要构造权限档案，凭空授予会扩大沙箱。客户端若不支持 `session/request_permission`，按 `--permission-fallback`（默认 `decline`）应答。

## 4. ACP v1 侧

- 帧：标准 JSON-RPC 2.0，一行一帧；`initialize` 返回 `protocolVersion: 1`。
- `session/new` 结果：`sessionId`（= Codex thread.id）、`modes{currentModeId,availableModes}`。
- `session/load`：在响应**之前**以 `session/update` 通知重放历史。
- `session/prompt`：入参 `prompt` 为 ContentBlock[]，返回 `{stopReason}`。
- `session/update` 载荷的 `sessionUpdate` 判别值：`user_message_chunk`、`agent_message_chunk`、`agent_thought_chunk`、`tool_call`、`tool_call_update`、`plan`、`usage_update`、`current_mode_update`。
- `session/request_permission`：`{sessionId, toolCall, options[]}`，`option.kind` 取 `allow_once` / `allow_always` / `reject_once` / `reject_always`；结果 `outcome{outcome:"selected"|"cancelled", optionId}`。
- `stopReason`：`end_turn`、`cancelled`、`refusal`、`max_tokens` …（本桥产生 `end_turn` / `cancelled`；失败走 JSON-RPC error）。

## 5. 易踩的坑

1. **别给 app-server 发 `jsonrpc:"2.0"`**：虽然 tolerant，但线材不是标准 JSON-RPC 2.0；本桥 `LiteFrame` 会显式剔除该成员。
2. **`initialized` 通知不能有 `params`**（schema 只有一个 `method`）。
3. **审批请求会阻塞模型**：桥必须能在 `session/prompt` 仍在执行时处理 client→server 的回复，否则死锁。这就是 ACP 侧采用「常驻 reader + 按 id 路由」而不是同步读帧的原因。
4. **通知可能早于 RPC 响应**：`turn/started` 与 `item/*` 可能在 `turn/start` 的 `result` 之前到达，turn 状态要提前建立。
5. **失败不要伪装成功**：`turn.status=failed` 时既把 `turn.error.message` 推到 transcript，也让 `session/prompt` 返回 JSON-RPC error。
6. **stdout 纪律**：任何日志误写 stdout 都会破坏 ACP 帧流。
