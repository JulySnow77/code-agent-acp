# ACP v1 协议事实与方法映射

本文说明本项目实现的协议边界，以及哪些结论来自规范、哪些来自本机实测。**本机实测没有拿到 `mcode acp` 的 `initialize` 响应**，因此凡涉及真实 MiniMax 能力的条目都标注为未验证。

## 线材格式

ACP v1 = JSON-RPC 2.0 + UTF-8 换行分隔 JSON（ndjson）stdio：

- 一行一帧，帧内不允许出现未转义换行；`stdout` 只放协议帧，诊断一律写 `stderr`。
- request id 必须是 string 或 integer，不能是 `null`（仅在无法确定 id 的错误响应里允许 `id: null`）。
- 双向全双工：agent 处理 `session/prompt` 时可以反向调用 `session/request_permission`、`fs/*`、`terminal/*`。
- 未知方法返回 `-32601 methodNotFound`，未知通知忽略——这是 ACP 扩展保持兼容的方式。
- 单帧上限 `32 MiB`（`acp.MaxFrameSize`），避免无 Content-Length 分帧导致的内存耗尽。

## Client → Agent 方法

| 方法 | 本项目 Client | 本项目 Agent (echo fixture) | Bridge |
| :--- | :--- | :--- | :--- |
| `initialize` | ✅ 协商 `protocolVersion=1` | ✅ | ✅ 原样转发 |
| `authenticate` / `logout` | ✅ | ✅（fixture 返回 invalid params / 空结果） | ✅ |
| `session/new` | ✅ 自动补 `mcpServers: []` | ✅ 返回 `sessionId` + fixture state | ✅ |
| `session/load` / `session/resume` | ✅ 会 flush 重放的 `session/update` | ✅ 回放历史 | ✅ |
| `session/list` / `close` / `delete` | ✅（按 `sessionCapabilities` 决定是否调用） | ✅ | ✅ |
| `session/fork` | ✅ 原始 params 透传（vendor 扩展） | ✅ | ✅ |
| `session/prompt` | ✅ 返回前 flush 通知 | ✅ 分块流式 | ✅ |
| `session/set_mode` / `set_config_option` | ✅ 只传 agent 返回的 id | ✅ | ✅ |
| `session/cancel`（通知） | ✅ | ✅ 取消 prompt context | ✅ |
| `$/cancel_request`（通知） | ✅ | ✅ | ✅ requestId 双向重映射 |

## Agent → Client 方法与通知

| 方法 | 本项目 Client 实现 | 说明 |
| :--- | :--- | :--- |
| `session/update`（通知） | ✅ 有序 worker，保留未知 variant | 文本 / 思考 / tool_call / tool_call_update / plan / usage / notice 等 |
| `session/request_permission` | ✅ `deny` / `allow` / 交互式三种策略 | 未注册时 `acp.Conn` 正确返回 `-32601` |
| `fs/read_text_file` | ✅ 绝对路径校验，可限制 root | 默认只读 |
| `fs/write_text_file` | ✅ 需 `--allow-write` 才声明并实现 | 默认关闭 |
| `terminal/create` / `output` / `wait_for_exit` / `kill` / `release` | ✅ 需 `--allow-terminal` | 不使用 shell，args 分离 |

## 开放对象设计

真实 agent 的能力声明未采集到，因此所有能力对象都保持开放，避免硬编码某个厂商的形状：

- `SessionCapabilities` / `AgentAuthCapabilities` / `CapabilityObject`：`map[string]json.RawMessage`，**键存在即表示支持**（例如 `list`、`resume`、`fork`、`logout`）。
- `AuthMethod.Env`：`json.RawMessage`。规范写 `EnvVariable[]`，但已有 ACP agent 会发 `{}`；保留原始字节两种形状都能解码。
- `SessionUpdate` / `ToolCallContent` / `SessionConfigOption`：`map[string]any`，vendor 字段不丢失。
- `ContentBlock`：带 `Raw` 保底的标签联合，未知 variant 原样回传。
- `MCPServer`：自定义 marshal，stdio 一定带 `args`/`env` 数组且不发 `type`，http/sse 带 `type`/`url`/`headers`。

**不要把这些开放结构里的值当作 MiniMax 的真实能力清单。** `serve` fixture 返回的 `fixture-ask` / `fixture-model` 只是为了让 `session/set_mode` 与 `set_config_option` 有可测的 id。

## MiniMax Code 特有的接入事实

| 事实 | 来源 | 本机状态 |
| :--- | :--- | :--- |
| `mcode acp` 是原生 ACP v1 stdio server | 官方 Quick Start、npm README、`mcode acp --help` | ✅ 帮助文本确认 |
| 安装 `npm install -g @minimax-ai/code`，需 Node 22.19+ / 24.2+ / 25 / 26 | 官方文档 | ✅ 本机 22.22.2 |
| 登录 `mcode login`，Global 账号 `mcode login --region global`；`mcode acp login` 子命令存在 | 官方文档 + `--help` | ✅ `--help` 确认 login 子命令存在 |
| 编辑器配置 `{"command":"mcode","args":["acp"]}` | 官方/生态资料 | ⚠️ 未在本机做界面联动验证 |
| 宿主可接管对话推理、Diff、权限/问题/取消、模型与计划 | 用户提供的资料 + 第三方集成文档 | ⚠️ 协议层覆盖数据结构，端到端未验证 |
| ACP 接口目前接受文本 prompt，不声明 image/audio/video 输入 | 第三方集成文档 | ⚠️ 未采集 `promptCapabilities` |
| 原生支持模型选择、权限请求、MCP 配置、取消、会话加载 | 第三方集成文档 | ⚠️ 未采集能力声明 |
| 具体模型名（M3 / M2.7 等） | 第三方集成文档 | ❌ 不写入本项目代码；由 `initialize` / `configOptions` 动态决定 |

## 证据边界

- 真实探针只读：只发过 `initialize`（以及 CLI 的 `--version` / `acp --help`），**没有 `session/prompt`、没有 `session/new`、没有写用户文件**。
- 本机 `mcode acp` 在桌面沙箱内无任何 stdio 响应，详见 [probes/index.json](probes/index.json)。
- 协议层与 CLI 的 72 项测试使用内存 transport、Go echo agent 和沉默进程夹具，**不冒充真实 MiniMax 兼容**。
- 采集到真实 `initialize` 响应后，应把原始帧写入 `docs/probes/`，更新 `index.json` 的 `verified` / `notVerified`，并在本文件里补上 MiniMax 的实际能力声明。
