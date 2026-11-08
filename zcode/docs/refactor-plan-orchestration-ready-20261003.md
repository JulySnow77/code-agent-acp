# 面向 Agent 编排的桥改造计划

- 日期：2026-10-03
- 状态：**P0–P3 已实现并验证，P4 未开始；开发已暂停，本文档为恢复工作的唯一依据**
- 依据：[`docs/agent-orchestration-survey-20261003.md`](./agent-orchestration-survey-20261003.md)
- 关联：`docs/DEVELOPMENT_PLAN.md`（移植主线，与本文档互补不冲突）

## 1. 出发点：调研纪要的三个定论

1. 本项目是 **L1 协议桥**，本地 14 个项目中 ACP 支持最佳（能力协商、`_meta` 扩展、三态语义三件难事均做对）；全局最佳的 AO 本质也是「把 harness 桥成 ACP agent」，方向一致。
2. **编排主线不在本仓库**。`zcode-orchestrator-go` 是唯一显式规划「队列 + 派发」的项目；桥刻意无状态。因此改造不是给桥加编排，而是把桥做得更纯、更硬、更值得被编排。
3. 调研 §6.1（wmux）给出直接可抄的工程细节：**把提问 payload 当不可信 agent-authored 输入处理**。

## 2. 目标与红线

**目标**：让桥成为可被外部控制面（zcode-orchestrator-go）安全消费的无状态 ACP agent——协议面完整、行为可预期、边界显式。

**红线（全程有效）**：

- 不在本仓库实现任务队列 / 派发器 / 多 agent 编排；
- 不引入新的常驻状态（per-session 排队闸口除外，它是会话内串行的正确性设施，不是编排设施）；
- 桥保持「一连接对一 zcode 后端进程」的现状（per-session 后端进程是 P5+ 候选，见 §8）。

## 3. 已完成的阶段（P0–P3）

> 所有改动尚未提交（`git status` 可见 7 个修改文件 + 5 个新文件）。**恢复工作的第一件事是审阅这套未提交 diff，而非从头开发。**

### P0 不可信输入加固 ✅

| 项 | 内容 |
|---|---|
| 新文件 | `internal/bridge/sanitize.go`、`internal/bridge/sanitize_test.go` |
| 接入点 | `elicitation.go`（parseAskQuestions / properties / elicitationMessage / marshalBackQuestion）、`ask.go`（askOptions、降级权限路径 toolCall 展示字段）、`permission.go`（name/desc/title/rawInput） |
| 规则 | 清洗前先 `MAX_RAW_LEN=1024` 硬截断；题面 200 字符、选项标签 80、选项数 ≤8；剥控制字符（含 ANSI 转义）；shape 守卫（字段缺失/类型异常 → 空字段，**不 panic、不阻断请求创建**） |
| 数值来源 | 沿用 wmux 调研数值（已在方案阶段拍板） |
| 验证 | `go vet ./...`、`go test ./internal/... ./cmd/...` 全绿；敌意 fixture 覆盖超长字段/控制字符/错误类型/空 options 等 |

### P1 权限 kind 映射 ✅

| 项 | 内容 |
|---|---|
| 修改 | `permission.go`：`toolCall.kind` 由固定 `"other"` 改为复用翻译层 `toolKindMap` 映射 ACP `read/write/execute` 闭集 |
| 新文件 | `internal/bridge/permission_kind_test.go` |
| 依据 | ACP schema 中 `request_permission` 的 `toolCall.kind` 使用 `ToolKind` 枚举闭集（已核对 SDK v0.13.5 schema）；`_meta.zcode` 本就携带真实 kind，映射表小、风险低 |
| 验证 | 单元测试全绿 |

### P2 initialize 能力面决策与补齐 ✅

三项拍板均已有结论：

| 项 | 决策 | 落点 |
|---|---|---|
| `mcpServers` | **显式告警的忽略**（三选一中的中间项）：非空时记 warning 日志，不再静默吞掉。理由：zcode 只从用户配置读 `mcp.servers`，后端无按连接/按会话注入 MCP 的通道，假装支持会埋雷 | `handleInitialize` |
| `loadSession` | **声明 true** 并实现 `session/load` 处理器，委托给与 `session/resume` 共用的 `resumeSession` 核心（cwd 解析 + zcode resume + 模式/模型回读） | `conn.Handle("session/load", ...)` + `resumeSession` 抽取 |
| `promptCapabilities` | 维持 `image/audio/embeddedContext` 全 false（当前翻译层不支持，如实声明） | `handleInitialize` |

e2e 测试同步更新：`TestInitializeDeclaresExtensionBoundary` 断言能力面；`session_config_test.go` 新增 session/load 场景。受影响测试曾单独跑绿。

### P3 并发 prompt 排队 ✅（含拍板）

| 项 | 内容 |
|---|---|
| 拍板 | 用户已确认：**改排队**（方案阶段三选一：排队 / 维持硬拒绝 / 开关控制，选了排队） |
| 实现 | 每会话容量 1 的 FIFO 闸口（`promptGates` channel）+ 排队深度计数（`promptQueued`），上限 `maxQueuedPrompts = 8`，超上限仍 -32600；持有令牌的 turn 释放后下一个排队 prompt 自动执行；`session/cancel` 只取消在飞 turn，不动队列 |
| 连带修改 | `turn.go` 注释更新（旧「抢不到就是明确拒绝」论证已过时）；并发 e2e 测试 `TestConcurrentPromptRejected` 重写为 `TestConcurrentPromptQueued`（验证：排队、按序执行、权限请求各自归属正确 turn、被拒 prompt 不触碰后端） |
| 验证 | 排队 e2e 测试单独跑绿 |

### 当前验证状态

- ✅ `go build` / `go vet ./...` / 单元测试 / 受影响的 e2e 子集
- ⚠️ **全量 `go test ./test/e2e/` 未在最终状态跑过**（排队测试开发期间全量跑过一次，之后 turn.go/session.go 注释与格式有微调）。恢复后必跑。
- ⚠️ 未提交。建议：全量 e2e 绿后整批提交（或按 P0–P3 分四个提交）。

## 4. P4 详细计划：编排友好预留（未开始）

> 只留接口，不建编排。三项工作互相独立，可按任意顺序做，也可只做部分。

### P4.1 subagents 桥接调研（产出决策，不改码或微改）

- **调研对象**：zcode `session/subagents` 上游行为（`upstream/` 目录），以及 ACP SDK 是否有对应的子 agent 表示法。
- **要回答的问题**：
  1. zcode 的子会话在 wire 上如何表示（独立 session？嵌套 turn 事件？）
  2. 桥能否在不引入编排语义的前提下透传/规范化这些事件
  3. 若 ACP 无对应通道，是否继续走 `_meta.zcode` 扩展（本项目已有先例）
- **产出**：docs/ 下一页调研纪要 + 明确的「做 / 不做」决策。**调研优先于实现**——若结论是 zcode subagents 与「无状态 L1 桥」定位冲突，应明确不做并记录理由。

### P4.2 wire log 结构化事件流

- **现状**：wire log 是给人看的文本日志。
- **目标**：增加可选的结构化输出（如 `ZCODE_ACP_EVENT_LOG=<path>` 环境变量开启），按行输出 JSON 事件（方向、方法、sessionId、关键字段摘要、时间戳），供 zcode-orchestrator-go 的控制面消费。
- **约束**：默认关闭；不泄露 prompt 全文（敏感场景下事件 payload 要可裁剪）；与现有文本日志共存不互斥。
- **验收**：开启后能用 `jq` 消费；关闭后零行为差异；fake server 场景下有测试断言事件序列。

### P4.3 O04 契约对齐（已勘察，待执行）

**已确认的事实**（来自 `docs/handoffs/o04/` 与 orchestrator 仓库）：

- O04 补丁**完全属于 zcode-orchestrator-go 侧**（execution 模块、进程组回收、contract 测试），本仓库无对应改动义务。
- orchestrator 通过 `bridge.lock.json`（本仓库 `go.mod`/`go.sum`/`cmd`/`internal`/`test` 下全部 `.go` 的 sha256 清单）+ `scripts/check-bridge.py` **钉死桥源码版本**；`make test-execution` 先校验清单再跑真实桥二进制的 SDK contract 测试。
- 即 **本次 P0–P3 的任何源码改动都会使 orchestrator 侧的 check-bridge 失败**，这是预期内的契约动作，不是事故。
- O04 对桥的行为契约（恢复执行时逐项核对）：
  1. initialize 协商 protocol 1 + O01 cancelRequest 能力；
  2. `serve --mode build` 启动，NewSession 报 build mode；
  3. 显式 mode/model 变更在 Prompt 前可读回一致；
  4. 一连接绑一会话；带其它 sessionId 的 notification 使执行失败；
  5. fs/terminal 回调不声明、调用返回 unsupported；
  6. 生产 SDK 日志被丢弃，normalized notification 经回调持久化。

**待执行步骤**：

1. 对照上述 6 条契约，逐项核对本仓库当前行为（预计全部已满足，P0–P3 未触碰这些面）；
2. 在 SDK_COMPATIBILITY.md（或 docs/handoffs/ 新增一页）记录 P0–P3 对契约的影响面声明；
3. 与 orchestrator 侧约定 lock 清单的**再生成流程**（本仓库每次发版后由谁重算 sha256、如何传递），把流程写进两个仓库各一份的交接文档；
4. 运行 `make test-execution BRIDGE_DIR=../zcode-open-bridge-go` 验证端到端（需 orchestrator 侧先应用 O04 补丁，属对方排期）。

## 5. 测试与验证标准（恢复工作时逐条执行）

1. `go build ./...` && `go vet ./...`
2. `gofmt -l internal/ cmd/ test/` 为空
3. `go test ./internal/... ./cmd/... -count=1`
4. `go test ./test/e2e/ -count=1` **全量**（含 `-race` 更稳：`ZCODE_E2E_RACE=1`）
5. 受影响 e2e 子集单独复跑：`TestConcurrentPromptQueued`、`TestInitialize*`、`TestSessionLoad*`、`TestResumeAdvertises*`
6. 提交策略：建议 P0–P3 各一个提交，P4 独立提交。

## 6. 风险与遗留问题

| 风险/遗留 | 说明 | 处置 |
|---|---|---|
| 全量 e2e 未在最终状态复跑 | P3 收尾阶段有注释/格式微调 | 恢复工作第一步必跑（§5.4） |
| P1 kind 映射的客户端兼容性 | 部分客户端可能对 `read/write/execute` 的渲染与 `other` 不同 | 风险低（spec 合规方向）；如真机冒烟发现问题，`_meta.zcode` 仍携带原始 kind 可供回退判断 |
| P2 mcpServers 仅告警不报错 | 依赖客户端观察 agent 日志才能发现 | 可接受：报错会打断 initialize，告警是更平滑的退化 |
| O04 lock 清单失效 | P0–P3 改动必然使 orchestrator check-bridge 失败 | 预期内；按 §4.3 步骤 3 的流程再生成，不单独打桥侧补丁 |
| 队列上限 8 的依据 | 拍板时直接定的数值，无负载依据 | ACP 客户端正常串行发 prompt，此路径几乎走不到；如未来有真实并发客户端再校准 |

## 7. P5+ 候选（本次不做，仅记录）

- **Per-session zcode 后端进程**（cwd/环境真正隔离）——P3 的方案中评估过但选择保留共享后端，等编排侧提出真实隔离需求再做；
- **凭据范围收窄**：O04 已提到子环境只复制白名单环境变量，桥的 `serve` 是否也提供 `--env-allowlist` 类开关可对齐；
- **音频/embeddedContext 能力**：zcode 支持后再如实声明；
- **Rich form constraints**：O04 明确留给 O05 validator，本仓库不抢。

## 8. 恢复工作清单（按序执行）

1. `git status` + `git diff` 通读 P0–P3 未提交改动（本文档 §3 是导读）；
2. 跑 §5 的 1–5 验证项，全绿；
3. 按 P0–P3 分提交；
4. P4：从 §4.3（O04 契约对齐）入手——成本最低、事实已勘察完；再做 §4.2（事件流）；§4.1（subagents）排最后且以调研产出为先；
5. 每项完成后更新本文档状态，并在 `docs/DEVELOPMENT_PLAN.md` 的待决项处同步勾销。
