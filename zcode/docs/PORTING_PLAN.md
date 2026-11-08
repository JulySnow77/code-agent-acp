# zcode-open-bridge-go 移植计划

| 项 | 值 |
|---|---|
| 文档日期 | 2026-09-18 |
| 上游项目 | https://github.com/tizerluo/zcode-open-bridge （MIT） |
| 上游基线 | commit `3e8d2b9`（2026-07-27），本地克隆 `/Volumes/BAK3T/projects/kanban/zcode-open-bridge` 另有 4 个文件未提交改动（见 §2.1） |
| 目标环境 | ZCode App 3.12.3 / 内置 CLI 0.16.5（`/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs`） |
| 工具链 | Go 1.26.1（本机已装），`github.com/coder/acp-go-sdk` v0.13.5（2026-06-02） |
| 本项目目录 | `/Volumes/BAK3T/projects/kanban/zcode-open-bridge-go` |

---

## 1. 目标与非目标

### 1.1 目标

1. 用 Go 重写上游 `packages/acp-bridge/zcode-acp-bridge`，产出单个静态二进制 `zcode-acp-bridge`，不依赖 Python。
2. 开箱兼容 ZCode CLI 0.16.5（App 3.12.x），并且把"随 ZCode 升级而漂移的协议细节"收敛到一个可版本化的兼容层里。
3. 建立一套每周（或不定期）审阅上游更新、决定是否移植的流程和台账。
4. 行为上对现有 ACP 客户端（Zed、JetBrains、本地 omnigent 的 AcpExecutor）保持兼容，扩展方法保留旧名字。

### 1.2 非目标（明确放弃或延后）

| 项 | 决定 | 理由 |
|---|---|---|
| CLI < 0.14.8 的轮询降级（`ProjectionDiffer`，181 行 + 314 行测试） | **不移植** | 当前 CLI 0.16.5，`session/subscribe` 事件流稳定可用；轮询分支是上游为兼容旧版保留的，Go 版只支持事件驱动 |
| `packages/mcp-server`、`packages/agent-help` | **延后到 backlog** | 与 ACP 桥无耦合，Python 版可继续独立使用 |
| ZCode TUI / 桌面 App 集成 | 不做 | 超出桥接范围 |

---

## 2. 现状调研结论

### 2.1 上游项目

- 单文件 Python 脚本 2674 行，纯标准库。结构：

| 类 | 行数 | Go 处理方式 |
|---|---|---|
| `ZCodeBackend` | 266 | 重写：子进程 + JSON Lines + pending 表 + server 反向请求分发 |
| `TurnMonitor` / `EventStreamListener` | 94 | 重写为 turn 状态机 |
| `EventTranslator` | 169 | 重写为纯函数，移植测试 |
| `ProjectionDiffer` | 181 | 放弃（§1.2） |
| `ACPBridge` | 1766 | 核心 ACP 方法重写；扩展方法收敛为透传表 |

- 测试 3729 行，其中可直接转成 Go 表驱动测试的：`test_event_translator.py`（306）、`test_app_server_methods.py`（666）、`test_ask_user_question*.py`（389 + 750）、`test_prompt_enhance.py`（310）、`test_provider_error.py`（189）。`tests/fakes/zcode_app_server`（464 行）是一个可执行的假 app-server，Go 版可通过 `ZCODE_BIN` 直接复用。
- 提交节奏：43 次提交集中在 2026-06 到 2026-07，最近一次 07-27，无 tag。
- **本地克隆有未提交改动**（bridge +76 行、fake +255 行、AskUserQuestion 测试 +468 行，内容是 wire log、perm_responses 上限、多问题 reply 处理）。这些改动尚未进入上游。基线决策见 §8 D6。

### 2.2 ZCode 0.16.5 实测协议差异（Go 版必须覆盖）

以下全部是 2026-09-18 在本机实测得到，不是推测。

| # | 现象 | 根因 / 细节 | Go 版对策 |
|---|---|---|---|
| C1 | `zcode app-server` 启动即退出，stderr：`无法定位 CLI ZCode Built-in Provider Config` | 0.16.5 按入口文件目录找 `provider/zcode-builtin.json` 或 `../../../../../config/provider/zcode-builtin.json`；README 的 symlink 方案因此失效 | 若 `ZCODE_BUILTIN_PROVIDER_CONFIG_FILE` 未设，按 `ZCODE_BIN` 真实路径自动推导 `<Resources>/config/provider/zcode-builtin.json` 并注入；`doctor` 子命令检查该文件存在 |
| C2 | `session/create` 耗时 15~20 s，上游硬编码 `timeout=15` 导致 `session/new` 必失败 | 会话库 `~/.zcode/cli/db/db.sqlite` 已 1.2 GB，启动阶段推送多条 `startup/storageState` | 建会话超时默认 120 s，可配；所有对 ZCode 的请求超时集中在一处 |
| C3 | `session/create` 返回结构 | `result.session.sessionId`；`result.settings.model.available[]`（`ref{providerId,modelId}`、`label`、`providerLabel`、`contextWindow`、`reasoning.levels[]`）；`result.protocol = {name:"ZCode Protocol", version:1}`；`result.runtime.eventSeq/stateRevision` | 建会话后缓存模型目录，供 setModel 解析和 `doctor` 输出 |
| C4 | `session/setModel` 参数变了 | 必须是 `{sessionId, model:{providerId, modelId, options:{reasoningLevel}}}`；传旧的 `{modelId}` 报 `Unrecognized key "modelId"`；部分模型 `reasoningLevel` 必填（`Reasoning level is required`）；模型不在目录里报 `Provider Registry 中不存在 Model` | 兼容层接受旧格式 `{modelId}`：按 C3 缓存的目录解析 `providerId`，缺省 `reasoningLevel` 取目录中第一个可用等级 |
| C5 | `session/send` 的 `content` 必须是字符串 | 传数组报 `expected string, received array` | ACP prompt 的 content blocks 先拼成文本（上游已如此），图片/资源块按能力声明拒绝 |
| C6 | `session/subscribe` 必填字段 | `{sessionId, deliveryKind:"desktop-continuous", includeSnapshot, afterSeq}`；缺字段报 ZodError | 固定传全 |
| C7 | 事件信封 | `session/event` 通知的 `params = {type, payload, seq, eventId, sessionId, turnId, traceId, timestamp, deliveryKind}`。实测 `type`：`session.titleUpdated`、`turn.started`、`session.updated`、`turn.failed`；上游翻译器还依赖 `model.streaming`（`payload.kind = text_delta|reasoning_delta`，`payload.delta`）、`tool.updated`（`payload.kind = scheduled|started|progress|result`，`payload.toolCallId`）、`turn.completed` | 翻译器对未知 `type` 忽略并计数；每种 type 有实录样本做测试 |
| C8 | server→client 请求 | `session/requestRuntimePreferences`（`scope: runtime-materialization` / `user-execution`）、`interaction/requestOfficialMcpAuthHeaders`、`interaction/requestPermission`、`interaction/requestUserInput`。回 `-32601` 不致命 | 未识别的请求统一回 `-32601`；已知的四种各有 handler |
| C9 | 大量通知 | `startup/storageState`、`state.updated`、`computer-use/operation-event`、`v4/telemetry/event`、`process/mcpTelemetry` | 未知通知静默丢弃，只在 `-v` 下打日志 |
| C10 | `turn.failed` 载荷 | `payload.error = {type, code:"model_request_failed", message, detail, underlyingErrorMessage, attribution{statusCode, retryable, providerId, modelId, providerKind, transport}}`。上游把它当 `end_turn` 兜底，客户端看不到任何错误 | 见 §8 D4：默认把错误文本作为 `agent_message_chunk` 推给客户端，并让 `session/prompt` 返回 JSON-RPC error |
| C11 | 独立 CLI 进程的模型目录里**没有**内置 GLM provider（`builtin:bigmodel-coding-plan` 等），只有用户自建 provider | 凭证在 `~/.zcode/v2/credentials.json`（`enc:v1:` AES-256-GCM），密钥来自 `ZCODE_CREDENTIAL_SECRET`，缺省用 `zcode-credential-fallback:<platform>:<home>:<user>`。桌面 App 大概率给子进程传了真实 secret。**待调查** | Phase 1 调查项；在此之前用 `ZCODE_ACP_MODEL` 显式指定可用的 provider/model，`doctor` 提前报"所选模型不在目录中" |
| C12 | CLI 相关环境变量 | `ZCODE_DATA_BASE_DIR`、`ZCODE_BUILTIN_PROVIDER_CONFIG_FILE`、`ZCODE_PERSONAL_PROVIDER_CONFIG_FILE`、`ZCODE_CREDENTIAL_SECRET`、`ZCODE_BASE_URL`、`ZCODE_ENDPOINT_ORIGIN`、`ZCODE_APP_VERSION`、`ZCODE_HTTP_PROXY`、`ZCODE_NO_PROXY` | 全部允许透传；`doctor` 打印实际注入值（apiKey 脱敏） |
| C13 | 子命令与参数 | `app-server` 与 `agent-server` 等价；`--surface terminal|desktop`；无 `--max-turns`（README 里提到的部分参数已不存在） | 版本探测后按表选择参数 |

### 2.3 ACP Go SDK 评估

- `github.com/coder/acp-go-sdk` v0.13.5：提供 `acp.Agent` 接口、`acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)`、全部 schema 类型（`SessionUpdate`、`ToolCall`、`RequestPermission` 等）、`ExtensionMethodHandler`。
- 两个限制：
  1. 接口方法很多（含大量 `Unstable*`），示例是逐个实现的，没有 `Unimplemented` 基类，需要自己写一个。
  2. `ExtensionMethodHandler` 只路由 `_` 开头的方法。上游的扩展方法（`session/fork`、`workspace/readState`、`prompt/enhance` 等）不带下划线，直接用 SDK 连接层会返回 method not found。
- 结论见 §8 D3：**自写 JSON-RPC 分发层（约 200 行），只用 SDK 的类型**。这样既能保留旧方法名，又能拿到强类型的 ACP 消息。

---

## 3. 架构设计

### 3.1 目录布局

```
zcode-open-bridge-go/
├── cmd/
│   └── zcode-acp-bridge/main.go        # 入口：serve（默认）| doctor | version | record
├── internal/
│   ├── jsonrpc/                        # ACP 侧 JSON-RPC 2.0 over stdio：分发、id 管理、agent→client 请求
│   ├── acpx/                           # 对 acp-go-sdk 类型的薄封装 + Unimplemented 基类 + 扩展方法别名表
│   ├── zcode/
│   │   ├── backend.go                  # 子进程、JSON Lines、pending 表、server 请求分发、退出处理
│   │   ├── events.go                   # 事件信封与 payload 的宽松类型（map / RawMessage）
│   │   └── version.go                  # `zcode --version` 探测与比较
│   ├── compat/                         # 唯一拦截点：按版本改写请求参数、统一超时、环境推导（C1~C6、C12、C13）
│   ├── bridge/
│   │   ├── session.go                  # ACP sessionId ↔ zcode sessionId、模型目录缓存
│   │   ├── turn.go                     # turn 状态机（started / streaming / completed / failed / cancelled）
│   │   ├── translate.go                # zcode 事件 → ACP session/update（纯函数）
│   │   ├── ask.go                      # interaction/requestUserInput ↔ session/request_permission
│   │   ├── permission.go               # interaction/requestPermission ↔ session/request_permission
│   │   ├── passthrough.go              # session/* workspace/* prompt/* 扩展方法透传表
│   │   └── enhance.go                  # prompt/enhance 同步 + 异步 job
│   ├── creds/                          # 读 ~/.zcode/v2/config.json 注入 env（移植 shared/credentials.py）
│   └── wirelog/                        # ZCODE_ACP_WIRE_LOG JSON Lines
├── test/
│   ├── fakeserver/                     # Go 版假 app-server（Phase 4 前先复用上游 Python fake）
│   └── smoke/                          # 对真实 zcode 的冒烟测试（build tag: real）
├── testdata/
│   ├── upstream/                       # 从上游测试搬来的事件样本
│   └── zcode-0.16.5/                   # 本次实录：create / subscribe / send / event / turn.failed
├── scripts/
│   ├── upstream-check.sh               # §6 上游巡检
│   ├── record-protocol.sh              # §5.3 ZCode 升级后实录协议样本
│   └── smoke.sh
├── docs/
│   ├── PORTING_PLAN.md                 # 本文
│   ├── upstream-sync.md                # §6 巡检台账
│   ├── zcode-protocol.md               # 按版本记录的协议事实（从 §2.2 起步）
│   └── upstream/local-baseline.patch   # 本地未提交改动的快照（D6）
├── upstream/                           # git submodule → tizerluo/zcode-open-bridge，只读参考，锁定 commit
├── upstream.lock                       # 已审阅到的上游 commit
├── go.mod / go.sum / Makefile / .goreleaser.yaml
├── .github/workflows/ci.yml            # lint + 单测 + fake e2e
├── .github/workflows/upstream-check.yml# 每周一自动巡检并开 issue
├── LICENSE（MIT）/ NOTICE（注明派生自上游）
└── README.md
```

### 3.2 关键设计规则

1. **ZCode 侧宽类型，ACP 侧强类型。** ZCode 协议是逆向得来且每版都漂移（C4、C6），所有 ZCode 请求参数和事件 payload 用 `map[string]any` / `json.RawMessage`，只在翻译器里按需取字段。ACP 侧用 SDK 类型。
2. **一个拦截点。** 所有发往 ZCode 的请求都经过 `compat.Rewrite(version, method, params) (params, timeout)`。版本相关的改写规则是一张表，每条规则带"引入版本、对应实录样本、上游 commit"。
3. **启动即自检。** `serve` 启动时依次做：解析 `ZCODE_BIN` 真实路径 → 推导/校验 provider 配置文件 → `zcode --version` → 注入凭证。任一步失败立即以可读错误退出，不要等客户端 `session/new` 才发现。`doctor` 子命令额外做一次 `session/create` 计时并打印模型目录。
4. **错误必须可见。** `turn.failed` 不再伪装成 `end_turn`（D4）。
5. **扩展方法双名字。** 旧名字 `session/fork` 与规范名 `_zcode/session/fork` 都接受，响应一致；文档推荐新名字，旧名字保留给现有客户端。
6. **并发模型。** 一个 reader goroutine 读 ZCode stdout；pending 请求用 `map[id]chan` + `context` 超时；每个 ACP session 一个事件通道；agent→client 请求（request_permission）与 session/update 写 stdout 共用一把互斥锁。

### 3.3 配置项

| 来源 | 名称 | 默认 | 说明 |
|---|---|---|---|
| env | `ZCODE_BIN` | `zcode`（PATH） | 推荐设为 `/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs` |
| env | `ZCODE_BUILTIN_PROVIDER_CONFIG_FILE` | 由 `ZCODE_BIN` 推导 | C1 |
| env / flag | `ZCODE_ACP_MODE` / `--mode` | `yolo` | `yolo|build|edit|plan`；Phase 4 后 `build` 可用 |
| env / flag | `ZCODE_ACP_MODEL` | 空（用 zcode 默认） | `providerId/modelId[/reasoningLevel]`，建会话后立即 setModel |
| env | `ZCODE_ACP_CREATE_TIMEOUT` | `120s` | C2 |
| env | `ZCODE_ACP_TURN_TIMEOUT` | `30m` | 与上游一致 |
| env | `ZCODE_ACP_QUESTION_TIMEOUT` | `10m` | 与上游一致 |
| env | `ZCODE_ACP_WIRE_LOG` | 空 | 与上游一致 |
| flag | `--legacy-turn-failed` | off | 恢复上游"failed 当 end_turn"行为 |
| flag | `--surface` | `terminal` | 透传给 app-server |

---

## 4. 分阶段计划

每阶段给出产出、验收标准、预估工作量。总计约 8~10 个工作日。

### Phase 0 — 基线与脚手架（0.5 天）

- 建目录、`go mod init`、添加 `upstream/` submodule 并锁定 `3e8d2b9`，写 `upstream.lock`。
- 把本地未提交改动导出为 `docs/upstream/local-baseline.patch`（D6）。
- 把 §2.2 的实录 JSON 整理进 `testdata/zcode-0.16.5/`，并落成 `docs/zcode-protocol.md` 首个版本。
- 技术 spike（半天内给结论）：`acp-go-sdk` 类型在自写分发层里的可用性；确认 union 类型的 JSON 编解码行为。
- CI 骨架：`go vet`、`golangci-lint`、`go test ./...`。
- **验收**：仓库可构建出空的 `zcode-acp-bridge version`。

### Phase 1 — ZCode Backend 与兼容层（1.5~2 天）

- `internal/zcode/backend.go`：启动 `app-server --stdio`、JSON Lines 读写（scanner 缓冲上限 16 MB）、pending 表、server 请求分发、进程退出时让所有 pending 失败。
- `internal/zcode/version.go`：解析 `zcode --version`。
- `internal/compat`：实现 C1、C2、C4、C5、C6、C12、C13 的规则。
- `internal/creds`：移植 `shared/credentials.py` 的凭证读取和"ZCODE_BASE_URL 残留检测"。
- `doctor` 子命令：输出版本、路径、注入的 env（脱敏）、`session/create` 耗时、模型目录、`ZCODE_ACP_MODEL` 是否在目录中。
- 调查 C11：桌面 App 如何把 `ZCODE_CREDENTIAL_SECRET` 交给子进程（查 `app.asar` 的 `out/main`、`out/host` 里 spawn 环境；可能来自 Electron `safeStorage` 或 Keychain）。结论写进 `docs/zcode-protocol.md`。
- **验收**：对真实 zcode 0.16.5，`doctor` 全绿；用上游 Python fake（`ZCODE_BIN=upstream/tests/fakes/zcode_app_server`）跑 backend 单测通过。

### Phase 2 — ACP 核心链路（2~3 天）

- `internal/jsonrpc` + `internal/acpx`：initialize、session/new、session/prompt、session/cancel、session/list、session/resume，agent→client 请求发送。
- `bridge/translate.go`：移植 `EventTranslator`，覆盖 `model.streaming`（text/reasoning）、`tool.updated` 四态、`turn.*`、`usage`、`plan`、files changed。测试数据来自 `testdata/upstream` + `testdata/zcode-0.16.5`。
- `bridge/turn.go`：事件驱动的 turn 状态机，含 C10 的失败处理与 turn 超时。
- `test/smoke`：`initialize → session/new → prompt "只回复两个字：收到"`，断言收到 `agent_message_chunk` 与 `end_turn`。
- **验收**：Zed 配置该二进制后能建会话、流式看到文本、看到工具调用状态；`turn.failed` 时客户端能看到错误原因。

### Phase 3 — 扩展方法（1 天）

- `bridge/passthrough.go`：白名单透传 `session/{fork,rewind,rewindCascade,goal,compact,steer,setThoughtLevel,updateRuntimeModelConfig,cancelBackgroundTask,setMode}`、`workspace/*`，双名字别名。
- `session/setModel` 适配器（C4）。
- `bridge/enhance.go`：`prompt/enhance` 同步与 `start/result/cancel` 异步 job（移植 `test_prompt_enhance.py` 的用例）。
- 移植 `test_app_server_methods.py` 为表驱动测试。
- **验收**：上游对应测试用例全部有 Go 等价物且通过。

### Phase 4 — 人在回路（1.5~2 天）

- `bridge/ask.go`：`interaction/requestUserInput` ↔ ACP `session/request_permission`，含多问题、超时回 cancel、reply 容量上限（移植 `test_ask_user_question*.py`，包括本地 baseline patch 的改动）。
- `bridge/permission.go`：`interaction/requestPermission` ↔ `session/request_permission`，让 `--mode build` 可用，消除上游"默认 yolo"的风险（上游 README 限制 #7）。
- `test/fakeserver`：把 Python fake 移植成 Go，CI 去掉 Python 依赖。
- **验收**：fake 下 AskUserQuestion 与权限确认 e2e 通过；真实 zcode 在 `build` 模式下工具调用不再卡死。

### Phase 5 — 发布与切换（0.5~1 天）

- goreleaser：darwin/arm64、darwin/amd64、linux/amd64、windows/amd64。
- README：Zed / JetBrains 配置片段、环境变量表、与 Python 版差异。
- 双跑对比脚本：同一 ACP 客户端脚本分别驱动 Python 版和 Go 版，比对 `session/update` 序列（忽略时间戳和 id）。
- 把 omnigent 的 AcpExecutor 指向 Go 二进制，观察一周。
- **验收**：对比脚本在 5 个代表性 prompt 上无语义差异；omnigent 切换后无回退。

### Backlog（不排期）

- 移植 `packages/mcp-server` 为 `cmd/zcode-mcp-server`。
- `session/requestRuntimePreferences` 给出有意义的回复而不是 `-32601`（需先弄清字段）。
- 文件 diff 内容（ZCode 协议不暴露 oldText/newText，可研究 checkpoints 目录能否补齐）。
- 若将来需要支持旧 CLI，再考虑轮询降级。

---

## 5. ZCode 新版兼容策略

### 5.1 支持矩阵与政策

| ZCode CLI | App | 状态 | 备注 |
|---|---|---|---|
| 0.16.5 | 3.12.3 | **主目标**，本机实测 | §2.2 全部差异 |
| 0.15.x | 3.2 ~ 3.11 | 尽力兼容 | 上游文档描述的协议面；无实机，靠上游测试样本 |
| 0.14.8 | 3.1.4 | 不保证 | 事件驱动首个版本 |
| < 0.14.8 | — | 不支持 | 需要轮询降级，已放弃 |

政策：**始终支持当前安装的 App 版本和上一个小版本**。App 自动更新后由 §6.3 的巡检发现版本变化。

### 5.2 兼容层规则表（初始内容）

| 规则 | 触发版本 | 行为 | 实录样本 |
|---|---|---|---|
| provider 配置路径推导 | ≥ 0.16.0（0.15 是否需要待验证） | 注入 `ZCODE_BUILTIN_PROVIDER_CONFIG_FILE` | C1 |
| 建会话超时 | 全部 | 默认 120 s | C2 |
| setModel 参数升格 | ≥ 0.16.0 | `{modelId}` → `{model:{providerId,modelId,options}}` | C4 |
| send content 扁平化 | 全部 | blocks → string | C5 |
| subscribe 必填字段 | ≥ 0.14.8 | 固定四字段 | C6 |
| 未知 server 请求 | 全部 | 回 `-32601` | C8 |
| 未知通知 | 全部 | 丢弃 | C9 |

每新增一条规则必须同时新增：一条 `testdata/zcode-<ver>/` 样本、一条表驱动测试、`docs/zcode-protocol.md` 一段说明。

### 5.3 ZCode 升级后的固定动作（`scripts/record-protocol.sh`）

1. 读 `/Applications/ZCode.app/Contents/Info.plist` 的 `CFBundleShortVersionString` 与 `zcode --version`。
2. 对真实 zcode 录制：`session/create` 响应、`session/subscribe` 响应、一次成功 turn 的全部 `session/event`、一次失败 turn 的 `turn.failed`、所有出现过的 server 请求方法名。写入 `testdata/zcode-<ver>/`。
3. 与上一版样本做结构 diff（字段增删、type 集合变化），生成 `docs/zcode-protocol.md` 的新章节草稿。
4. 跑全部翻译器与兼容层测试；失败项即新的兼容规则待办。
5. `doctor` 全绿后把新版本写进 §5.1 矩阵。

### 5.4 失败面呈现规则

- `turn.failed` → `agent_message_chunk`（内容取 `error.detail` 或 `underlyingErrorMessage`）+ `session/prompt` 返回 JSON-RPC error（`code -32000`，`data` 带 `attribution`）。
- ZCode 进程退出 → 所有 pending 请求返回 `-32603 zcode backend exited (code N, last stderr: ...)`，并在 stderr 打印最后 20 行子进程 stderr（上游把 stderr 丢到 DEVNULL，C1 的错误因此被吞掉）。
- 超时 → 错误信息里带上实际等待秒数与配置项名。

---

## 6. 上游跟踪流程

### 6.1 台账

- `upstream.lock`：一行，已审阅到的上游 commit 与日期。
- `docs/upstream-sync.md`：每次巡检一节，表格列：上游 commit、标题、涉及路径、决定（移植 / 跳过 / 设计已覆盖 / 延后）、理由、Go 侧 PR 或 commit。

### 6.2 巡检脚本 `scripts/upstream-check.sh`

```bash
#!/usr/bin/env bash
# 用法: scripts/upstream-check.sh            # 只看
#       scripts/upstream-check.sh --bump     # 审阅完毕后把 upstream.lock 推到 origin/main
set -euo pipefail
cd "$(dirname "$0")/.."
git -C upstream fetch origin --quiet
last=$(cut -d' ' -f1 upstream.lock)
head=$(git -C upstream rev-parse origin/main)
if [ "$last" = "$head" ]; then echo "上游无新提交 ($last)"; exit 0; fi
echo "== 上游新提交 $last..$head"
git -C upstream log --date=short --format='%h %ad %s' "$last..$head"
echo; echo "== 变更文件（按优先级）"
git -C upstream diff --stat "$last..$head" -- packages/acp-bridge tests/fakes tests/test_event_translator.py tests/test_app_server_methods.py tests/test_ask_user_question*.py
echo "-- 其他"
git -C upstream diff --stat "$last..$head" -- . ':!packages/acp-bridge' ':!tests'
echo; echo "== 提示：ACP 桥/测试改动逐条填入 docs/upstream-sync.md"
if [ "${1:-}" = "--bump" ]; then
  echo "$head $(date +%F)" > upstream.lock
  git -C upstream checkout --quiet "$head"
  echo "upstream.lock 已更新为 $head"
fi
```

### 6.3 节奏与检查清单

**每周一（或 ZCode App 更新后立即）：**

1. 运行 `scripts/upstream-check.sh`。
2. 对每个涉及 `packages/acp-bridge` 或测试的提交，按下表决定：

| 上游改动类型 | 默认决定 |
|---|---|
| ZCode 协议字段 / 方法变化 | **移植**，并补 §5.2 规则与样本 |
| 翻译逻辑 bug 修复 | **移植**，先把上游新增测试搬过来复现 |
| 新扩展方法透传 | **移植**（只加一行白名单） |
| AskUserQuestion / 权限行为变化 | **移植** |
| 轮询降级相关 | 跳过（§1.2） |
| Python 实现细节（线程、queue、打包） | 跳过 |
| mcp-server / agent-help / skills / README | 延后到 backlog |

3. 填写 `docs/upstream-sync.md`，`--bump` 更新 `upstream.lock`，提交。
4. 检查 ZCode 版本是否变化（`defaults read /Applications/ZCode.app/Contents/Info.plist CFBundleShortVersionString` 与 `zcode --version`）。变化则执行 §5.3。
5. 若某条修复对 Python 版用户也有价值（如 C1 的路径推导、C2 的超时），顺手给上游开 issue 或 PR，减少两边分叉。

**自动化（可选，Phase 5 后）：** `.github/workflows/upstream-check.yml` 每周一 09:00 跑巡检脚本，把输出写进一个标题为 `Upstream sync <年-周>` 的 issue；没有新提交就不开。

### 6.4 什么情况下重新评估"继续移植"

- 上游连续 3 个月无提交：改为按需巡检（App 更新时）。
- 上游出现大规模重构（单次 diff > 1000 行）：先做一次专项评估，再决定是逐条移植还是只吸收测试样本。

---

## 7. 测试策略

| 层 | 内容 | 依赖 |
|---|---|---|
| 单元 | 翻译器、兼容层规则、session 映射、turn 状态机、ask/permission 状态机 | 无 |
| 假服务 e2e | 用上游 Python fake（Phase 1~3）→ Go fake（Phase 4 起）驱动完整二进制 | fake |
| 真机冒烟 | `go test -tags=real ./test/smoke`，需要 `ZCODE_BIN` 与可用模型 | 本机 ZCode |
| 双跑对比 | Python 版与 Go 版对同一输入的 `session/update` 序列比对 | 两个版本 |
| 协议实录回归 | 每个 `testdata/zcode-<ver>/` 都跑一遍翻译器 | 无 |

---

## 8. 待拍板的决策（已给推荐值，未反对即按推荐执行）

| # | 决策 | 推荐 | 备选 |
|---|---|---|---|
| D1 | 是否移植轮询降级 | **不移植** | 移植（多约 500 行，无实机可验） |
| D2 | 扩展方法命名 | **双名字**：旧名 + `_zcode/` 前缀 | 只保留规范名（会破坏 omnigent 等现有客户端） |
| D3 | SDK 用法 | **自写分发层 + SDK 类型** | 直接用 SDK 连接层（扩展方法必须改名） |
| D4 | `turn.failed` 语义 | **错误可见**：推错误文本 + JSON-RPC error；`--legacy-turn-failed` 可回退 | 沿用上游 end_turn 兜底 |
| D5 | 默认权限模式 | **Phase 4 前 `yolo`（与上游一致），Phase 4 后默认仍 `yolo` 但文档推荐 `build`** | Phase 4 后默认改 `build` |
| D6 | 基线是否包含本地未提交改动 | **包含**：导出为 patch 存档，Phase 4 一并移植；同时建议把这些改动在 Python 克隆里提交到分支，避免丢失 | 只以上游 HEAD 为基线 |
| D7 | 是否同期移植 mcp-server | **不**，进 backlog | 同期做（+1~2 天） |

---

## 9. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| ZCode 协议无官方文档，每版漂移 | 升级后桥失效 | §5 的规则表 + 实录样本 + `doctor` 自检；只支持最新两个版本 |
| C11 内置 provider 凭证在独立进程不可用 | 只能用自建 provider | Phase 1 调查；`ZCODE_ACP_MODEL` 显式指定；`doctor` 提前报错 |
| 上游继续给 Python 版加功能 | 移植成本持续存在 | §6 流程把成本固定为每周一次小批量；先搬测试再搬实现 |
| 1.2 GB 会话库拖慢建会话 | 客户端体验差 | 超时放宽；文档说明可用 `ZCODE_DATA_BASE_DIR` 指向独立数据目录（代价：与桌面 App 不共享会话） |
| SDK 类型与自写分发层的耦合 | union 类型编解码坑 | Phase 0 spike 先验证；必要时只用 SDK 的常量与部分类型 |
| 许可证 | 派生作品 | 保留 MIT，`NOTICE` 注明派生自 tizerluo/zcode-open-bridge |

---

## 10. 附录

### 10.1 Zed 配置示例（Go 版）

```json
{
  "agent_servers": {
    "ZCode (Go bridge)": {
      "command": "/usr/local/bin/zcode-acp-bridge",
      "args": ["serve"],
      "env": {
        "ZCODE_BIN": "/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs",
        "ZCODE_ACP_MODEL": "<providerId>/<modelId>/disabled"
      }
    }
  }
}
```

### 10.2 本次实录的最小样本（完整版放 `testdata/zcode-0.16.5/`）

`session/create` 请求与响应骨架：

```json
{"id":"1","method":"session/create","params":{"workspace":{"workspacePath":"/tmp","workspaceKey":"tmp"},"mode":"yolo"}}
{"id":"1","result":{"session":{"sessionId":"sess_…","mode":"yolo","status":"idle","workspace":{…}},
  "settings":{"mode":…,"model":{"available":[{"ref":{"providerId":"…","modelId":"…"},"label":"…","reasoning":{"levels":[…]}}]},"permission":{"mode":"yolo"},"thoughtLevel":{…}},
  "protocol":{"name":"ZCode Protocol","version":1},"runtime":{"eventSeq":0,"stateRevision":0},
  "messages":[],"projection":{…},"slashCommands":[…],"todos":[],"todoGroups":[]}}
```

`session/setModel`（0.16.5）：

```json
{"id":"2","method":"session/setModel","params":{"sessionId":"sess_…","model":{"providerId":"…","modelId":"…","options":{"reasoningLevel":"disabled"}}}}
```

`session/event` 信封：

```json
{"method":"session/event","params":{"type":"turn.failed","payload":{"error":{"code":"model_request_failed","message":"Model request failed.","detail":"…","underlyingErrorMessage":"…","attribution":{"statusCode":404,"retryable":false,"providerId":"…","modelId":"…"}},"turnPhase":"…"},"seq":6,"eventId":"…","sessionId":"sess_…","turnId":"…","timestamp":…,"deliveryKind":"desktop-continuous"}}
```

### 10.3 上游到 Go 的模块映射速查

| 上游位置 | Go 位置 |
|---|---|
| `ZCodeBackend` | `internal/zcode/backend.go` |
| `EventStreamListener` + `TurnMonitor` | `internal/bridge/turn.go` |
| `EventTranslator` | `internal/bridge/translate.go` |
| `ACPBridge._on_initialize/_on_session_*` | `internal/bridge/session.go` + `internal/jsonrpc` |
| `ACPBridge` 扩展方法 handler 群 | `internal/bridge/passthrough.go` |
| `ACPBridge` prompt/enhance | `internal/bridge/enhance.go` |
| `ACPBridge` AskUserQuestion | `internal/bridge/ask.go` |
| `shared/credentials.py` | `internal/creds` |
| `tests/fakes/zcode_app_server` | `test/fakeserver`（Phase 4） |
| `tests/test_event_translator.py` 等 | `internal/bridge/*_test.go` |
