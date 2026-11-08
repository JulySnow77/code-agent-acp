# workbuddy-acp-bridge

把 **WorkBuddy 的 ACP 协议独立成一个项目**：一个用 **Go 1.22+ 编写、零第三方依赖**的程序，对上游是标准 **ACP v1 stdio (ndjson) server**，对下游是 **WorkBuddy PC 客户端本机 ACP 网关**的 HTTP/SSE 客户端。

任何 ACP client（auto-run、Zed、codeg、其他 agent 工作台）都可以用一条命令拉起本桥，把 WorkBuddy 里已登录的模型、连接器和会话作为自己的 ACP Agent 使用。

```
ACP client (auto-run / Zed / codeg / ...)
   │  stdin/stdout  ACP v1 ndjson (initialize / session/new / session/prompt / ...)
   ▼
workbuddy-acp-bridge                 ← 本项目（Go 二进制）
   │  POST /api/v1/acp/connect + POST /api/v1/acp (Accept: application/json, text/event-stream)
   │  SSE: session/update / agent_message_chunk / tool_call / session_end
   ▼
WorkBuddy PC 客户端的 loopback ACP gateway (127.0.0.1:<dynamic port>)
```

本项目**不属于** auto-run 仓库；auto-run 只保留通用 ACP client，通过 `workbuddy_bridge` 指向本项目的可执行文件。协议适配、会话发现、权限策略和证据边界都在这里维护。

## 参考与依据

| 来源 | 借鉴点 |
|---|---|
| WorkBuddy 开放平台 [Open API / ACP 说明](https://open.workbuddy.cn/docs/openapi) | ACP = JSON-RPC 2.0 + SSE 双通道；`initialize` / `session/load` / `session/prompt`；`session/update`、`session/request_permission`、`stopReason` |
| `workbuddy-acp` / `jie023/workbuddy-acp-bridge`、`Kxiandaoyan/workbuddy-acp-bridge` | 本机 loopback 网关发现、免密 `connect`、HTTP/SSE 调用序列、只读权限模型 |
| [`workbuddy-codeg-bridge`](https://www.npmjs.com/package/workbuddy-codeg-bridge) | 把 WorkBuddy 桥接成 **ACP stdio agent** 的形态；默认 deny 权限；`supports_mcp=false` |
| [`workbuddy-subagent-bridge`](https://github.com/BenjaminNH/workbuddy-subagent-bridge) | 有状态多轮会话、逐项权限回包、任务状态与后续反馈、报告边界 |
| WorkBuddy local bridge / `Kxiandaoyan` 的冒烟脚本 | 支持 `initialize`、`session/new`、`session/prompt`、`session/cancel` 等标准方法 |
| zcode-acp Go bridge（本机既有） | `version` 子命令、按 run 冻结桥二进制摘要的宿主接入约定 |

本机实测（2026-11-08，WorkBuddy 2.165.0 / macOS）见 [docs/protocol.md](docs/protocol.md)。官方 Open API 的 GET SSE + POST 双通道是**云端任务**模型；本机 loopback 网关把 SSE 直接挂在 POST 响应上，本项目按本机形态实现，同时保留 `Acp-Connection-Id` 关联头。

## 构建与运行

需要先有 Go 1.22+ 工具链。**编译产物不进 Git**：ACP 宿主（auto-run）会按每次运行冻结桥二进制摘要，因此它必须是当前平台真实编译出来的文件，而不是脚本或跨平台提交物。

```bash
make build          # -> bin/workbuddy-acp-bridge、bin/fake-acp-server
make test           # go test ./... -count=1
make vet            # go vet ./...

bin/workbuddy-acp-bridge version
bin/workbuddy-acp-bridge sessions --json
bin/workbuddy-acp-bridge doctor --json          # 只读握手，不发 prompt
bin/workbuddy-acp-bridge serve --mode build     # ACP stdio 服务
```

`bin/workbuddy-acp-bridge` 是可执行 Go 二进制（本机约 8.8 MB，Mach-O x86_64），路径与 [examples/auto-run.json](examples/auto-run.json) 中的 `workbuddy_bridge` 一致。换机器或清理 `bin/` 后重新 `make build` 即可；`make clean` 删除产物。

- `version`：输出 `workbuddy-acp-bridge 00.00.01`，供宿主的 `check` 校验桥身份。
- `sessions`：列出 `~/.workbuddy/sessions/*.json` 中 `kind=interactive` 的活跃会话（端点、sessionId、cwd、pid）。端口每次会话都会变，桥每次启动重新读取，不缓存端口。
- `doctor`：只做「发现端点 → `connect` → `initialize`」，打印协商出的 `agentCapabilities`。**不调用 `session/load`、`session/prompt`**，不会向用户正在使用的会话投递消息。
- `serve`：从 stdin 逐行读 JSON-RPC 2.0 帧，向 stdout 逐行写帧；stderr 只写诊断日志，绝不污染协议流。

### 目录结构

```
cmd/workbuddy-acp-bridge/   CLI 入口（version / sessions / doctor / serve）
cmd/fake-acp-server/        最小标准 ACP v1 stdio 夹具，供下游 client 协议对拍
internal/protocol/          ndjson 帧、JSON-RPC 错误、SSE 增量解码
internal/discovery/         loopback 端点发现与会话选择
internal/gateway/           WorkBuddy HTTP/SSE JSON-RPC 客户端
internal/backend/           Local（真实网关）与 Fake（确定性）后端、权限模型
internal/server/            ACP v1 stdio server、反向权限请求与 deferred 帧队列
internal/cli/               子命令与环境变量覆盖
internal/testutil/          httptest 模拟网关（127.0.0.1 临时端口）
```

`serve` 选项：`--mode plan|build`、`--backend local|fake`、`--endpoint URL`、`--session-id ID`、`--cwd PATH`、`--timeout SEC`、`--permission-mode deny|client`、`--fake-result JSON`。

宿主（如 auto-run）用固定的 `serve --mode plan|build` 拉起桥，无法传额外参数，因此同时支持环境变量覆盖：`WORKBUDDY_ACP_BACKEND=local|fake`、`WORKBUDDY_ACP_PERMISSION_MODE=deny|client`、`WORKBUDDY_FAKE_RESULT=<json>`、`WORKBUDDY_FAKE_PERMISSION_MODE`。未知后端、模式或权限策略取值明确报错，不静默回退。

## ACP 方法映射

| 上游 stdio 方法 | 本桥行为 | 下游网关 |
|---|---|---|
| `initialize` | 返回 `protocolVersion=1`、`agentInfo`、`agentCapabilities`、`_meta.workbuddy` | `initialize` |
| `session/new` | 建立外部 writer，返回 `sessionId` | `session/new {cwd, mcpServers:[]}` |
| `session/load` / `session/resume` | 精确恢复同一会话 | `session/load {sessionId, cwd, mcpServers:[]}` |
| `session/set_mode` | 本地记录 plan/build；plan 强制拒绝写/执行权限 | 无（本地权限策略） |
| `session/set_config_option` | 回显 `currentValue`，并在日志/`_meta` 标明模型选择未经独立验证 | 无 |
| `session/prompt` | 把下游 SSE 事件转成 `session/update` 通知，返回 `stopReason` | `session/prompt {sessionId, prompt:[...]}` + SSE |
| `session/cancel` | 取消当前回合 | `session/cancel` |
| `session/request_permission`（server→client） | 按权限策略回包 | 网关权限请求 |

`end_turn` 只表示回合结束，宿主仍需按自己的 schema 与产物判定完成；这与 auto-run 对 ZCode 桥的要求一致。

### 权限模型

- 默认 `--permission-mode client`：把网关的权限请求转发给上游 ACP client 逐项决策（auto-run 在只读阶段只授权 read/search/think，写阶段才允许 edit/execute/fetch）。
- `--permission-mode deny`：一律回 `cancelled`，适合只查询/总结，参考 codeg-bridge 的只读默认。
- `plan` 模式无论配置如何都拒绝写与执行；**桥模式不是 OS 沙箱**，宿主仍需自己核对文件与 Git 边界。
- 可用环境变量 `WORKBUDDY_ACP_PERMISSION_MODE` 覆盖（宿主无法给桥传额外参数时有用）。

## 接入 auto-run

`examples/auto-run.json` 是配置模板。`workbuddy_bridge` 指向本项目的可执行文件；`workbuddy_bridge_marker` 让宿主的 `check` 严格校验版本标识：

```json
{
  "schema_version": 1,
  "workbuddy_bridge": "/Volumes/BAK3T/projects/acp/workbuddy/bin/workbuddy-acp-bridge",
  "workbuddy_bridge_marker": "workbuddy-acp-bridge",
  "models": { "spec": { "agent": "workbuddy", "model": "<providerId>/<modelId>" } }
}
```

`workbuddy/<providerId>/<modelId>` 也可作为字符串前缀路由。模型标识由 WorkBuddy 会话自身决定；桥只记录并回显请求，`_meta.workbuddy.modelSelectionVerified=false`，**不冒充已切换服务端模型**。

## 测试与证据边界

```bash
make test                       # 34 项，全部通过
go test ./... -race -count=1    # 竞态检测通过
```

测试只使用 `internal/testutil` 起在 `127.0.0.1` 临时端口的模拟 HTTP/SSE 网关和 `--backend fake`，**不触碰任何真实 WorkBuddy 会话**，覆盖握手、会话恢复、SSE 事件转发、权限 deny/client 两种回包、deferred 帧、取消、错误帧、端点发现，以及版本／doctor／serve／环境变量覆盖的真实子进程往返。`cmd/fake-acp-server` 编译为 `bin/fake-acp-server`，是给下游 ACP client 做协议对拍的夹具，不参与本项目主测试链。

真实环境只做了**只读**探测，输出不含凭证：

| 证据 | 内容 |
|---|---|
| [docs/probes/doctor-go-20261108.json](docs/probes/doctor-go-20261108.json) | Go 实现 `connect` + `initialize`：`status=valid`、`loadSession=true`、`promptSent=false` |
| [docs/probes/initialize-stdio-go-20261108.ndjson](docs/probes/initialize-stdio-go-20261108.ndjson) | Go stdio server 对真实网关完成 `initialize`，能力透传 |
| [docs/probes/sessions-go-20261108.json](docs/probes/sessions-go-20261108.json) | 会话发现结果 |
| `docs/probes/*-20261108.json`（无 go 后缀） | Python 0.1.0 时期的历史只读证据，保留不改写 |

跨仓库集成已实测（2026-11-08）：auto-run 的 `check_bridge` 接受 Go 二进制（`workbuddy-acp-bridge 0.2.0`、`version_marker_verified=true`）；随后 `Executor` 以 `WORKBUDDY_ACP_BACKEND=fake` 拉起 `bin/workbuddy-acp-bridge serve --mode build`，完成 `initialize → session/new → set_mode → set_config_option → session/prompt`，回执 `outcome=succeeded`、`agent=workbuddy`、`result={"value":"integrated-go"}`、`cleanup_confirmed=true`、`errors=[]`。这证明两侧 ACP 契约对齐，但使用的是 fake 后端。

**没有对真实 WorkBuddy 会话发送过 `session/prompt`**：当前交互会话会被 `Persistent Session already has an external writer` 占用，未经授权向用户的实时对话投递消息会打断用户，属于外部动作。因此本项目的真实模型/工具链路**未验证**，模拟测试、只读握手和 fake 集成都不冒充真实兼容；需要真实探针时应由用户指定一个专用空闲会话并显式开启。

## 与 Python 0.1.0 的差异

2026-11-08 用户要求把实现语言改为 Go，0.1.0 的 Python 包（`workbuddy_acp/`、`tests/`、`pyproject.toml`）已整体删除，CLI 契约、环境变量、方法映射和桥路径保持不变，版本号升到 0.2.0。

- **构建产物**：从仓库内 Python 脚本变为需 `make build` 生成的 Go 二进制，二进制被 `.gitignore` 排除。宿主按摘要冻结桥身份时冻结的是**真实编译产物**，比脚本包装更严格。
- **HTTP 超时语义**：Go 只给 dial 与响应头阶段设时限，不设整体 `http.Client.Timeout`，避免长回合被总时限掐断，贴合原 Python 传输的 per-read socket timeout。
- **修复一个潜在忙循环**：Python 版在等待权限回复时把 deferred 帧出队后又入队，若期间收到带 `method` 的通知会无限循环。Go 版把「读 stdin」与「取 deferred」拆成两个方法（`readFrame` / `read`），新增专门回归测试覆盖该场景。
- **权限模式校验**：未知 `WORKBUDDY_ACP_PERMISSION_MODE` 明确报错，不再静默按 deny 处理。
- **测试**：20 项 Python unittest → 34 项 Go test，并额外通过 `go test -race ./...`。

### 已知限制

- 只能连接到 PC 客户端已激活的 interactive session；空闲会话会被回收进 prewarm 池，端口随之变化。
- 同一会话一次只能有一个外部 writer；并发投递会互相打断（网关返回 `writer_occupied`）。
- `session/new` 在本机网关上会附着到持久会话，不是完全隔离的新上下文；多任务隔离需要上游自行管理 sessionId。
- `doctor` 不检测 writer 占用（检测需要 `session/load`，有副作用），读 `status=valid` 不等于会话可写。
- 仅在 macOS / WorkBuddy 2.165.0 上做了只读握手；Windows 与云端 Open API 双通道未实测。
- 凭证是一次性的 `connectionId` / `sessionToken`，只在进程内存里保存，不写日志、不落盘。
