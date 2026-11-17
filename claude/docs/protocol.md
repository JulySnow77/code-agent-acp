# Claude Code stream-json ↔ ACP v1 协议事实与映射

本文件是 `claude-acp-bridge` 的协议依据。事实来源为本机 **claude-cli 2.1.322** 的 `claude --help` 输出与 stream-json 模式行为核对，并交叉参考社区实测文档（spatie/bloom `docs/PROTOCOL.md`、多个 SDK 的控制消息类型定义）。CLI 升级后应重新核对。

## 1. 传输与信封

| 侧 | 传输 | 信封 | 判别字段 |
| :--- | :--- | :--- | :--- |
| ACP（宿主 ↔ 桥） | stdio，一行一个 JSON 对象 | 标准 JSON-RPC 2.0 | `jsonrpc:"2.0"` + `method`/`id` |
| Claude（桥 ↔ CLI） | 子进程 stdio，一行一个 JSON 对象 | 纯 NDJSON，**无** `jsonrpc` 信封 | 顶层 `type` 字段 |

四种 stream-json 消息形状（CLI 侧）：

```
事件       {"type":"system"|"user"|"assistant"|"result"|"stream_event", ...}
权限请求   {"type":"control_request","request_id":"...","request":{"subtype":"can_use_tool",...}}
控制响应   {"type":"control_response","response":{"subtype":"success","request_id":"...","response":{...}}}
控制请求   {"type":"control_request","request_id":"...","request":{"subtype":"interrupt"|"set_model"|...}}  ← 桥发出
```

桥对两侧都用 32 MiB 单帧上限（`bufio.Scanner`），写帧串行加锁，诊断只写 stderr。

## 2. 启动参数（事实，来自 `claude --help` 与实测）

```
claude -p
  --input-format stream-json      # 仅 --print 下可用；"text"(默认) | "stream-json"
  --output-format stream-json     # "text" | "json" | "stream-json"
  --verbose                       # -p + stream-json 输出必须，否则 CLI 拒绝运行
  --permission-prompt-tool stdio  # 启用 control_request/control_response；否则权限自动否決
  --permission-mode <mode>        # manual | acceptEdits | auto | bypassPermissions | plan | dontAsk
  --session-id <uuid>             # 指定新会话 UUID（或 --resume <id> 续接）
  [--model <id>] [--mcp-config <file>] [--effort <level>] [--add-dir <dir>]
```

关键事实：

- `--input-format` / `--output-format` 仅在 `-p`（`--print`）下生效；stdin 保持打开，一条会话可多轮。
- CLI 的 `system/init` **在首条用户消息到达时才发出**（空 stdin 启动会直接退出，无 init），所以桥自己生成 UUID 并经 `--session-id` 传给它，`session/new` 无需 prompt 即可返回 sessionId。
- `--permission-prompt-tool stdio` 未写入 `--help` 说明，但二进制支持；缺它时工具权限会被自动拒绝（不发出 `control_request`）。
- `--verbose` 与 `-p --output-format stream-json` 必须同时给，否则 CLI 拒绝。

## 3. 发送一轮（桥 → CLI）

一条 NDJSON 用户消息：

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"do the thing"}]}}
```

`content` 是 Anthropic content block 数组：

| 块 | 字段 |
| :--- | :--- |
| `text` | `text` |
| `image` | `source:{type:"base64",media_type,data}` 或 `{type:"url",url}` |
| `tool_result` | `tool_use_id`,`content`(string 或 block[]),`is_error` |

ACP `ContentBlock[]` 映射：`text`→text；`image`(data/mimeType)→base64 source；`image`(uri)→url source；`resource_link`/`resource`→带描述的 text 块。

## 4. 事件（CLI → 桥）

### `system` / `init`

会话首条事件（首条用户消息后）：

```json
{"type":"system","subtype":"init","session_id":"f93932c9-...","cwd":"/path",
 "model":"claude-sonnet-4","permissionMode":"manual","tools":["Bash",...],"uuid":"..."}
```

桥捕获 `session_id` / `model` / `tools` / `cwd` 作为 `_meta`，不据此改变路由。

### `assistant`

每个 content block 一条，块已完整：

```json
{"type":"assistant","uuid":"...","parent_tool_use_id":null,"session_id":"...",
 "message":{"id":"msg_...","role":"assistant","model":"...","stop_reason":null,
   "usage":{"input_tokens":2,"cache_creation_input_tokens":10,
            "cache_read_input_tokens":20,"output_tokens":2,...},
   "content":[ <恰好一个 block> ]}}
```

`content[0]` 三种：

| block | 字段 | ACP 映射 |
| :--- | :--- | :--- |
| `text` | `text` | `agent_message_chunk` |
| `thinking` | `thinking`,`signature` | `agent_thought_chunk` |
| `tool_use` | `id`(`toolu_...`),`name`,`input` | `tool_call_update`(pending→in_progress) |

`parent_tool_use_id` 非 null 表示来自子 agent（本桥不嵌套，按普通事件处理）。

### `user`（工具结果回灌，非人类输入）

```json
{"type":"user","message":{"role":"user","content":[
  {"type":"tool_result","tool_use_id":"toolu_...","content":"...","is_error":null}]}}
```

按 `tool_use_id` 匹配 tool_use，映射为 `tool_call_update`（`is_error:true`→failed）。

### `stream_event`（仅 --include-partial-messages 时出现）

Anthropic 流式增量，`event.type` 含 `content_block_start` / `content_block_delta` / `content_block_stop` / `message_*`。`content_block_delta.delta.type`：

- `text_delta`（`text`）→ 实时 `agent_message_chunk`
- `thinking_delta`（`thinking`）→ 实时 `agent_thought_chunk`
- `input_json_delta`（`partial_json`）→ tool input 增量
- `signature_delta` → 签名

桥按 `index` 累积已流式发出的文本，完整 `assistant` 块到达时去重，避免重复文本。

### `result`（一轮的最后一行）

```json
{"type":"result","subtype":"success","is_error":false,
 "duration_ms":7880,"num_turns":3,"stop_reason":"end_turn","session_id":"...",
 "result":"...","usage":{"input_tokens":6,"output_tokens":360,...},
 "modelUsage":{"claude-sonnet-4":{"contextWindow":1000000,...}},
 "permission_denials":[],"uuid":"..."}
```

`subtype`：`success` / `error_max_turns` / `error_during_execution`。桥的结束映射：

| result | ACP |
| :--- | :--- |
| `success` | `stopReason:end_turn` |
| `error_max_turns` | `stopReason:max_tokens` |
| `error_during_execution` / `is_error` | 推错误文本 + JSON-RPC error |
| 本桥已 `session/cancel` | `stopReason:cancelled`（cancel 直接结束 turn） |

`usage` → `usage_update.used`；`modelUsage.<model>.contextWindow` → `usage_update.size`。

### 其他事件

`rate_limit_event`、`task_started`、`task_updated`、`system/task_notification`、`system/status`、`system/thinking_tokens`、hook 事件等：本桥安全忽略（解码器规则：未知事件绝不使流失败）。

## 5. 控制协议

### CLI → 桥：`can_use_tool` 权限请求

```json
{"type":"control_request","request_id":"req_...","request":{
  "subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"toolu_...",
  "input":{"command":"git add -A"},"description":"...","decision_reason":"...",
  "permission_suggestions":[...],"blocked_path":"...","classifier_approvable":true}}
```

桥发 ACP `session/request_permission`（4 个标准选项），按选择回答：

```json
{"type":"control_response","response":{"subtype":"success","request_id":"req_...",
 "response":{"behavior":"allow","updatedInput":{"command":"git add -A"}}}}
```

- allow 必须带 `updatedInput`（原 input 或编辑后）；deny 必须带 `message`：

```json
{"type":"control_response","response":{"subtype":"success","request_id":"req_...",
 "response":{"behavior":"deny","message":"User denied this action"}}}}
```

`request_id` 必须匹配否则 CLI 拒绝。`decision_reason` 可能含 ANSI 转义，桥渲染前剥离。未知 `control_request` 子类型一律保守拒绝，不放行。

### 桥 → CLI：控制请求

| 目的 | `request` |
| :--- | :--- |
| `session/cancel` | `{"subtype":"interrupt"}` |
| `session/set_model` | `{"subtype":"set_model","model":"..."}` |
| `session/set_mode` | `{"subtype":"set_permission_mode","mode":"..."}` |

CLI 用 `control_response` 回答，桥按 `request_id` 关联。

## 6. ACP v1 侧

- 帧：标准 JSON-RPC 2.0，一行一帧；`initialize` 返回 `protocolVersion:1`。
- `session/new` 结果：`sessionId`（桥生成的 UUID）、`modes`、`_meta.claudeSessionId`。
- `session/load`：以 `--resume <sessionId>` 启动新 CLI 续接；CLI 在 stream-json 下不回吐完整历史，桥不额外重放 transcript。
- `session/prompt`：`prompt` 为 ContentBlock[]，返回 `{stopReason}`。
- `session/update` 的 `sessionUpdate` 判别：`user_message_chunk`、`agent_message_chunk`、`agent_thought_chunk`、`tool_call`、`tool_call_update`、`plan`、`usage_update`、`current_mode_update`（本桥产生 agent/thought/tool/usage）。
- `session/request_permission`：`option.kind` 取 `allow_once`/`allow_always`/`reject_once`/`reject_always`；结果 `outcome{outcome,optionId}`。
- `stopReason`：`end_turn`、`cancelled`、`max_tokens`、`refusal`（失败走 JSON-RPC error）。

## 7. 易踩的坑

1. **必须 `-p` + `--verbose`**：否则 stream-json 输出不可用或 CLI 拒绝运行。
2. **`--permission-prompt-tool stdio` 是权限通道**：缺它 `can_use_tool` 不会到达，工具被自动拒绝。
3. **`system/init` 晚到**：首条用户消息后才发；用 `--session-id` 前置已知 id。
4. **每个 session 一个进程**：不能用 Codex 那种「单进程多 thread」模型；`exec.CommandContext` 必须用长生命周期 ctx，否则 handler 返回时 `defer cancel()` 会杀掉子进程。
5. **权限是嵌套 RPC**：CLI 的 `can_use_tool` 到来时 `session/prompt` 仍在执行，ACP reader 必须常驻按 id 路由，否则死锁。
6. **未知事件/子类型不能让流失败**：CLI 版本升级会新增 `type`/`subtype`，解码器只处理认识的，其余安全忽略或保守拒绝。
7. **stdout 纪律**：任何日志误写 stdout 都会破坏 ACP 帧流。
