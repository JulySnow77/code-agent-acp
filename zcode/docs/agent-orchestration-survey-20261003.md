# 多 Agent 编排全景调研纪要

**日期**：2026-10-03
**范围**：`/Volumes/BAK3T/projects/kanban` 下 16 个目录（14 个 git 仓库）+ GitHub 生态调研
**方法**：git 一手取证 + 4 组并行只读代码勘察 + GitHub API 直接查询

---

## 0. 执行摘要

本次调研有四个可独立引用的结论：

1. **这 14 个项目不是同类竞品。** 它们分处四个架构层，大量项目之间是上下游而非替代关系。用「谁更好」作比较维度本身是错的。

2. **AskUserQuestion 的实现分四种态度**，比「有没有实现」更值得记：建完整闭环 / 协议等价物+适配器架构 / 自动应答 / 主动禁用。后两种（自动应答、主动禁用）都是刻意设计，不是遗漏。

3. **GitHub 上 12 个项目满足「code agent 调度 + AskUserQuestion」**。最值得关注的是 `Untrivial-ai/agent-orchestrator`（★12,637），它用 per-harness ACP 驱动矩阵取代了对私有工具名的绑定。

4. **ACP 支持本地最佳是 `zcode-open-bridge-go`**，全局最佳是 AO。判据不是方法数，而是有没有做对能力协商、`_meta` 扩展、三态语义这三件难事。

---

## 1. 操作记录：全量 Git 对齐

### 1.1 最终状态

13 个有提交的仓库全部达成 `ahead 0 / behind 0`，工作区干净（3 个仓库保留 5 个与远端无冲突的本地未跟踪文件）。

### 1.2 实际发生的变更

| 仓库 | 动作 | 变更量 |
|---|---|---|
| paperclip | 快进 | +108 提交 |
| vibe-kanban | 快进 | +16 提交 |
| herdr | 快进 | +10 提交 |
| multica | 重置到 `origin/main` | 跨 2897 提交、6249 文件 |
| zcode-open-bridge | 重置到 `origin/main` | 跨 98 提交 |
| omnigent | 提交 + 推送 `origin/v2_loop` | 16 提交（含新增 `3379a2871`） |
| zcode-open-bridge-go | 提交 + 推送 `t03-ask-negotiation` 建跟踪 | 新增 `eb6efe6`，53 文件 +10836 行 |
| Maestro / agent-kanban-mvp / agent-wrapper / codux | 无需处理 | 本就 0/0 |
| bernstein / zcode-orchestrator-go | 按选择跳过 | 无有效 git 状态 |

### 1.3 回滚点（重要）

按用户选择丢弃本地改动前，已建立双保险：

```bash
# multica 的 23 个本地改动
git -C multica stash pop                                        # 或
git -C multica checkout backup/pre-align-multica-20261003-155141

# zcode-open-bridge 的 4 个未推送提交（含 AskUserQuestion 功能）
git -C zcode-open-bridge checkout backup/pre-align-20261003-155159
git -C zcode-open-bridge stash pop
```

### 1.4 遗留问题：Py 版 AskUserQuestion 能力已降级 ⚠️

**这是本次操作唯一的实质性副作用，需要立即决策。**

对齐 `origin/main` 后，`zcode-open-bridge` 的 AskUserQuestion 实现**不复存在**：

| 版本 | 行数 | `askUserQuestion` 命中 |
|---|---|---|
| 当前 `origin/main` | 2932 | **1**（仅剩 `askUserQuestionAutoResolutionEnabled` 配置开关，`:120`） |
| 对齐前 HEAD（备份分支） | 2606 | 8（含完整扇出实现） |
| 被丢弃的 stash 改动 | — | 含 `_ask_one_question` / `_answer_from_perm_response` / `{"questions":[...], "options":[...]}` 多问题扇出 + wire log |

即：**本地改动不是修饰，而是该能力的唯一实现载体。**

恢复方式二选一（会与当前 2932 行版本冲突，需手工合并）：
```bash
git -C zcode-open-bridge stash pop                              # 取回工作区改动
git -C zcode-open-bridge checkout backup/pre-align-20261003-155159  # 取回 2606 行整版
```

`tests/fakes/zcode_app_server` 在 stash 与备份分支中均确认完好。

### 1.5 顺带修复

`zcode-open-bridge-go/.gitignore` 补入 `.DS_Store` 与 `.workbuddy/`。原文件未覆盖这两项，直接 `git add -A` 会把 macOS 垃圾文件与本地工具记忆目录推上共享仓库。推送前已扫描全部待提交文件，无真实密钥命中（`env.example` 为纯模板；`orchestration.py` 的命中是变量名 `_policy_auth_token`，误报）。

---

## 2. 架构全景：四个层次

> 完整可视化见 [`../agent-orchestration-landscape.html`](../agent-orchestration-landscape.html)（14×8 对比矩阵 + 13 张项目档案）

### 2.1 分层地图

| 层 | 项目 | 性质 |
|---|---|---|
| **L4 治理与自治平台** | paperclip、multica、omnigent | 编排单元＝组织里的岗位；有预算、审批、依赖唤醒 |
| **L3 控制面 / 看板** | agent-kanban-mvp、Maestro、vibe-kanban、vibe-todo、codux | 有的自带执行引擎，有的纯转发 |
| **L2 执行后端** | agent-runner、zcode-orchestrator-go | 真正 spawn coding agent 进程 |
| **L1 协议桥 / 封装库** | zcode-open-bridge(Py/Go)、agent-wrapper | 单会话协议翻译，不做任务编排 |
| **L0 终端承载** | herdr | PTY 生命周期 + agent 状态感知 |

### 2.2 七个共同点：技术选型高度收敛

1. **全部走子进程 + CLI 封装**，无一选择 SDK 嵌入。Claude 侧普遍用 `--output-format=stream-json`（`agent-runner/crates/executors/src/executors/claude.rs:227`）
2. **worktree 是并行隔离的共识解，且没有任何项目用容器做 agent 隔离**（omnigent 的 bwrap 是沙箱 harness 进程，非替代）
3. **审批门禁是标配且显式建模**：`run_approvals` 表 / ACP elicitation / `audit_logs` + `human_interventions`
4. **都把「挡住私有协议」当硬约束**：spec 写死「executor 错误必须先归一再发事件」（`runner-kanban-execution-spec.md:439`）
5. **事件流都要求可重放**：`run_events(run_id, seq)` 唯一索引
6. **控制面/执行面分离写进 ADR-0001**（local-first，workspace 即安全边界）
7. **持久化全用外部 DB**：SQLite 五家、Postgres 三家，仅 zcode 桥与 agent-wrapper 刻意无状态

### 2.3 最大的差异：调度模型分五派

| 派别 | 代表 | 机制 |
|---|---|---|
| **Lane 串行** | omnigent | 每 agent 一条 lane，lane 内并发恒为 1，lane 间并行。完成判定 terminal-event-first（PTY idle 会中途震荡） |
| **DB 队列 pull** | multica | 数据库行本身就是队列，daemon 主动 claim。5 个 `ReasonCode` 各附修复动作，并发可配 1–50（默认 6） |
| **心跳唤醒** | paperclip | 不轮询，issue-assignment/comment/decision/chat 四类唤醒服务自唤醒，带依赖 DAG |
| **无调度** | vibe 系、zcode 系、agent-wrapper | UI 触发即跑，或状态机完备但无派发器 |
| **上层调度** | Maestro、codux | 唯一有真正编排抽象的两个。Maestro 双编排器（Group Chat moderator + Auto Run 文档队列 + 写串行队列）；codux 的 `AgentWorktreeCreateRequest{agent, prompt, base_branch}` 是最标准的任务编排抽象 |

### 2.4 两个推翻预设的发现

- **herdr 不是「可能无关的 PTY 工具」**：README 直写 "the runtime your coding agents live on"，识别 22 种 agent 的 idle/working/blocked 状态，暴露 `agent.prompt`（支持 `wait{until,timeout_ms}`，让 agent 互相提问与等待）
- **codux 不是「可能无关的 fork」**：`AgentWorktreeCreateRequest` + `task_monitor` 线程推进终态 + Merge/Remove 生命周期

二者是**竞品而非上下游**，无任何 crate 或 git 依赖关系。

---

## 3. AskUserQuestion 支持矩阵

### 3.1 第一档：原生支持 Claude Code AskUserQuestion 完整闭环

| 项目 | 角色 | 证据 |
|---|---|---|
| **vibe-kanban** | 解析 + UI（最完整） | `claude.rs:1182,2686` 解析 `ClaudeToolData::AskUserQuestion` → `ActionType::AskUserQuestion`；`AskUserQuestionBanner.tsx` 242 行（答案追踪/超时/自定义自由文本）；MCP 权限规则 `claude.rs:227,251` |
| **agent-runner** | 解析端 | 与 vibe-kanban 同源代码血统（`claude.rs:1219,2686`），无 UI |
| **agent-kanban-mvp** | 作答端 | `PendingApprovalEntry.tsx:4-6` 直接 import `findAskUserQuestionEntry` / `AskUserQuestionItem` / `AskUserQuestionOption` |

三者构成完整闭环：**runner 解析 → 事件归一 → kanban 渲染作答**。

### 3.2 第二档：协议等价物（elicitation / requestUserInput）

| 项目 | 深度 |
|---|---|
| **zcode-open-bridge-go** | `ask.go` 处理 zcode 私有 `requestUserInput` 的**重发去重**（zcode 按 `reannounceIntervalMs` 翻倍重发，新 JSON-RPC id 但同 requestId；不去重会重复问人**且第二份答案被 `cleanupClientRequest` 静默丢弃**）；`ask_form.go` 的 `parseElicitationSchema` + `propKeySplit(isMulti)` 渲染多选表单 + `askSingleSelect` |
| **omnigent** | 专用模块族：`pending_elicitations.py`(383)、`routes_elicitations.py`(182)、`_codex_elicitation.py`(**868**)、`_antigravity_elicitation.py`、`_elicitation_schema.py`、`_elicitation_registry.py`(parked/pre-resolved 状态)。全仓 2334 处命中 |
| **paperclip** | 唯一做成**适配器架构**：`codex-question-adapter.ts`(632) + `acp-question-adapter.ts`(378) + `question-adapter-conformance.test.ts`(136) + `question-set.ts` + 协议 fixture `questions/{codex,acpx}.json` |
| **multica** | **半支持，Claude 侧主动禁用**：`claude.go:1091` 加 `--disallowedTools AskUserQuestion`，注释说明非交互 stream-json 模式下会返回空答案、用户根本看不到问题（GitHub #2588），改用 issue comment；Codex 侧支持 `codex.go:2939` `mcpServer/elicitation/request` |

### 3.3 第三档：只做状态感知

- **herdr** — `claude.toml:106-111` 的 `mcp_elicitation_prompt` 规则识别 MCP elicitation 的 Accept/Decline 控件，仅用于判定 blocked 态
- **codux** — `probe/claude.rs:464` / `constants.rs:22` 从「permission/elicitation wait signature」推导等待态

### 3.4 明确不支持（主动设计决策）

- **Maestro** — 8 处 prompt 全是反向指令。`maestro-system-prompt.md:222`：「Do NOT call any tool that waits for user input (e.g. `AskUserQuestion` in Claude Code...)，这些会阻塞执行，在 batch/Auto Run 下不可靠」，要求把问题写进普通回复文本。`renderer/constants/app.ts:25` 仅列入常量数组
- **agent-wrapper**、**zcode-orchestrator-go** — 零命中

### 3.5 核心观察：四种态度

同一个能力，业界分成四类，**后两类都是刻意决策不是遗漏**：

1. **建完整闭环** — vibe-kanban、agent-runner、agent-kanban-mvp
2. **协议等价物 + 适配器架构** — paperclip、AgentsMesh、peri
3. **自动应答** — project-orchestrator 的 Guard、Maestro 的 prompt 反向指令
4. **主动禁用** — multica（`--disallowedTools`）；反方向的 opendev 是 `ALWAYS_ALLOWED = ["TaskStop", "AskUserQuestion"]`

---

## 4. GitHub 生态调研

**检索方式**：GitHub 连接器未接入（目录中仅 `email_workbench` 与 `matrix` 多媒体两组工具），故使用本机 `gh` CLI 直查 GitHub API 取一手数据；网页搜索仅用于交叉验证生态动态。

### 4.1 第一档：规模与成熟度最高

**`Untrivial-ai/agent-orchestrator` (AO)** ★12,637 · Go · Apache-2.0 · 2026-10-03
最大最活跃。关键选择：**走 ACP 标准 elicitation，不绑定 Claude 私有工具名**（`AskUserQuestion` 全仓仅 1 处命中且在 `docs/research/`，elicitation 有 32 处）。落点：
- `backend/internal/adapters/chatdriver/acp/client.go`（1166 行，官方 `acpsdk`）
- `backend/internal/adapters/chatdriver/` 下 **13-15 个 per-harness 驱动**：`claudeacp` / `cursoracp` / `geminiacp` / `droidacp` / `deepseekharnessacp` / `kimiacp` / `kimiacp` / `ompacp` / `nativeacp` / `codexappserver` / 通用 `acp`
- 桌面 `frontend/src/renderer/components/chat/ElicitationDock.tsx`
- 移动端 `packages/mobile/lib/chat/elicitation-native-controls.{tsx,ios.tsx,android.tsx}`
- `docs/adr/0003-persistent-chat-provider-host.md`（有 ADR 记录）

### 4.2 第二档：与本地栈最像

| 项目 | 星 | 要点 |
|---|---|---|
| **openwong2kim/wmux** | ★409 | worktree fan-out + atomic hunk adoption；`src/daemon/approvals/askUserQuestion.ts` 工程细节最讲究（见 §6.1） |
| **mvschwarz/openrig** | ★4,560 | tmux agent 网络：角色分配、共享上下文、**文件所有权**（Codex 改 auth.ts 时 Claude 排队请求） |
| **desplega-ai/agent-swarm** | ★852 | Lead Agent 规划派发 + Worker 跑 Docker 容器，DAG 带重试与审批门 |
| **alamops/agetor** | ★84 | 解析 Claude Code **TUI 渲染层**，9 个 fixture 暴露真实难度（见 §6.2） |

### 4.3 第三档：运行时侧

| 项目 | 星 | 接法 |
|---|---|---|
| **AgentsMesh/AgentsMesh** | ★2,360 | pod I/O 层把答案作为 `updatedInput` 注入工具调用；PTY-mode 与 ACP-mode 双 pod 接口；10+ 协议集成测试 |
| **KonghaYao/peri** | ★221 | `HumanInTheLoopMiddleware` 持有该工具，**关闭即从 LLM 工具视图消失**；注释记教训——此前 AskUserQuestion「游离于 middleware 体系外」 |
| **dmotles/sprawl** | — | Supervisor 完整生命周期：`AskUserQuestion` / `RegisterQuestionConsumer` / `ResolveQuestion` / `CancelByAgent`（agent 被杀时其待决问题一并取消） |
| **this-rs/project-orchestrator** | ★138 | Guard **自动应答**走自治模式（第四种立场） |
| **kagent-dev/kagent** | ★3,918 | 基于 Google ADK v2，`Multiple` 多选 + `Choices`，ToolConfirmation 暂停返回 pending |
| **chaitin/MonkeyCode** | ★4,787 | 任务流中 `AskUserQuestion` 与 `AutoApprove` 是**同级分支** |
| **opendev-to/opendev** | ★844 | `ALWAYS_ALLOWED = ["TaskStop", "AskUserQuestion"]` 策略层永不拦截 |
| **harnessclaw/harnessclaw-engine** | ★196 | `internal/tools/builtin/askuserquestion/askuserquestion.go` |

### 4.4 协议层基础设施

- **`agentclientprotocol/agent-client-protocol`** ★4,362 · Rust · Apache-2.0 — 规范本体，schema 分 `v1` / `v2`，**Elicitation 已是 v2 一等能力**，40+ agent 宣布支持
- **`openclaw/acpx`** ★3,310 — headless ACP CLI client，`--format json` 输出 NDJSON 事件

---

## 5. ACP 支持对比

### 5.1 结论

- **本地 14 个最佳：`zcode-open-bridge-go`**
- **全局最佳（含 GitHub）：`Untrivial-ai/agent-orchestrator`**

判据不是方法数量，而是有没有做对三件难事。

### 5.2 本地支持度排序

| 项目 | ACP 角色 | 依赖/规模 | 判定 |
|---|---|---|---|
| **zcode-open-bridge-go** | ACP agent（桥） | `coder/acp-go-sdk v0.13.5` | **纯度最高** |
| **multica** | ACP client | 14 个 `acp_*.go` | **面最宽** |
| **paperclip** | ACP client | `drivers/acpx/` 30 模块 30 测试 | **纪律最强** |
| **agent-runner** | ACP client | `agent-client-protocol v0.8` **+ `unstable` feature** | 跟得最前但窄 |
| **vibe-kanban / vibe-todo** | ACP client | 1911 行 / 5 文件（其中 868 行在日志归一化，真协议约 1043 行） | 中等 |
| **omnigent** | ACP client | `acp_executor.py` 1291 + `acp_harness.py` 124 | 中等 |
| **agent-wrapper** | — | SDK 已声明，但仅 `tasks/prd-acp-client.md` + `examples/acp/main.go` | **未落地** |
| Maestro / agent-kanban-mvp / herdr / zcode-orchestrator-go | — | 零 | **零 ACP** |

### 5.3 zcode-open-bridge-go 为什么是本地最佳

1. **能力协商** — `supportsElicitationForm()` 先读 `p.ClientCapabilities.Elicitation["form"]` 判断客户端是否支持表单模式，才决定走不走 form；注释明确「initialize 之后才有意义」
2. **`_meta` vendor extension** — `p.ClientCapabilities.Meta.Zcode.CancelRequest`，走规范为扩展预留的官方命名空间
3. **三态语义严谨** — accept/decline/cancel 区分清楚，专门注明 `elicitationCancelled` 与 permission 路径的 `permCancelled` 是「同一件事、两种说法」；并知晓 spec 约束「Property definitions must be primitive types」

对比：multica 面更宽但止于把 ACP 方法映射到自有 Go 类型；paperclip 测试最密但 driver 仅 5 个；agent-runner 虽唯一开 `unstable` 跟实验区，ACP 在其体系中只是 9 个 executor 之一。

### 5.4 三处事实修正

- **codux 不用 ACP。** `crates/codux-protocol/src/` 仅 `lib.rs`，全仓**无 `agent-client-protocol` 依赖**；其 v3.2 remote protocol + FFI 是自研私有协议。此前命中 "elicitation" 仅为状态探测字符串（`"permission/elicitation wait signature"`）
- **agent-runner 的 ACP 实现路径。** 实际在 `crates/executors/src/executors/acp/`（client / session / harness / normalize_logs）；`crates/acpx/` 是**空目录**
- **agent-wrapper 名不副实。** README 架构图标 "Session Store (memory)" 但代码中无任何 `Store` 类型；`tasks/prd-multi-agent-expansion.md` 标题含 "Multi-Agent"，实际内容只是「新增 3 个 provider」

---

## 6. 最值得借鉴的两个工程细节

### 6.1 wmux：把「盲按」问题写进代码注释

`src/daemon/approvals/askUserQuestion.ts` 文件头的问题定义值得直接抄进自家代码：

> 审批被编码成「按第一个选项键」，**一个不告诉人「在问什么」的待决请求，会让那一下按键变盲**。`Approve` 对同意型问题是安全的，对「该删哪个文件？」是危险的——手机端必须显示题面和选项，否则操作员是在回答一个自己读不懂的问题。

处理上把 payload 当 **agent-authored 不可信输入**：
- shape：任何字段可能缺失或类型异常，每次访问都守卫，未识别结构返回空字段而**不抛异常也不阻止请求创建**（「没题干的请求也远胜于没有请求」）
- content：清洗控制字符（agent 能在选项标签里塞 ANSI 转义）+ 硬截断（题面 200 字符、选项 80、选项数 8）
- **在任何正则处理之前先截断**（`MAX_RAW_LEN = 1024`），避免多 MB 字段让清洗变成 O(n)

### 6.2 agetor：9 个 fixture 暴露 TUI 层解析的真实难度

它解析的是 Claude Code 的**终端渲染输出**，因此 fixture 集合本身就是难度证明：

```
src/bun/fixtures/askuserquestion/
  v284_preview.txt            multi_size_tab.txt
  review_submit.txt           single_with_other.txt
  multi_initial_toppings.txt  multi_toppings_toggled.txt
  single_wrapped_question.txt single_select.txt
```

配套 `docs/plans/askuserquestion-2-1-284-parsing.md` 是针对 Claude Code 2.1.284 的解析设计。

**架构含义**：`zcode-open-bridge-go` 在**协议层**解决同类问题，agetor 在**渲染层**解决。后者所需的 fixture 数量本身就是难度量级——若走渲染层路线，测试资产成本要提前计入。

---

## 7. 风险与待决

| # | 问题 | 建议 |
|---|---|---|
| 1 | **Py 版 zcode-open-bridge 的 AskUserQuestion 实现已随对齐丢失**（§1.4） | **最高优先级**，立即决定是否恢复及如何与 2606 行备份合并 |
| 2 | **agent-runner 无并发限流器** | 全仓无 Semaphore/permit，并发上限零代码约束；retry 仅存在于 opencode SDK 传输层。多 run 同跑只靠 worktree 路径隔离兜底，而限流责任本应落在 agent-kanban-mvp 侧，那边也没有 |
| 3 | **控制面/执行面分工未落实到底** | ADR-0001 要求「runner 与 adapter 不得假设部署位置」，但 agent-runner 绑死本地路径 `repo/.agent-runner/worktrees/`，Hosted Workspace 扩展点目前是假的 |
| 4 | **zcode-orchestrator-go 规划最完整但落地最空** | 14 态状态机、私有 bare 快照隔离、DB `one_active_attempt` 串行约束、跨仓契约测试（`make test-contract` 用 `bridge.lock.json` 校验桥源码指纹）齐备，唯独无派发器，`go.mod` 连 `acp-go-sdk` 都没有。若要把编排主线收敛到 Go，这是唯一显式规划过「队列+派发」的项目 |
| 5 | **vibe-todo 的 agent-runner 集成只有文档** | `docs/superpowers/specs/2026-05-12-agent-runner-design.md` + m1–m4 plan 存在，但 `grep agent-runner crates/ packages/` 零命中 |
| 6 | **`zcode-open-bridge-go/crates/acpx/` 是空目录** | 清理或补齐，避免误导后续排查 |

---

## 8. 方法论与可信度说明

### 8.1 取证方式

- **架构对比**：4 组并行只读代码勘察，结论均附 `文件路径:行号`
- **GitHub 调研**：`gh` API 直查（一手），星数/活跃度/语言/许可证均取自 API 返回
- **ACP 深度**：逐项目核实依赖声明与文件结构

### 8.2 已知局限

- GitHub 代码搜索有速率限制（中途撞到 403 一次，等限流恢复后补齐关键文件路径）。AO 与 agetor 做到路径级验证，其余项目到代码片段级
- 4 组深挖中有 1 组（codux/herdr）**推翻了派发时给出的两个预设判断**，说明预设不可轻信
- ACP 支持度**未经统一量表打分**，排序基于实测能力覆盖度判断
- 多次出现「文档声称但代码无证据」的情况（agent-wrapper 的 Session Store、codux 的 ACP、vibe-todo 的 agent-runner 集成），均已单独标注
- 网页搜索结果中 `kahma.io` / `rcwap.com` / `coddykit.com` 属 SEO 内容农场，其数据未采信

### 8.3 配套资产

| 文件 | 说明 |
|---|---|
| [`../agent-orchestration-landscape.html`](../agent-orchestration-landscape.html) | 14×8 对比矩阵、分层地图、13 张项目档案（§2 的可视化） |
| [`../runner-kanban-execution-spec.md`](../runner-kanban-execution-spec.md) | runner↔kanban 执行契约规范 |
| [`../runner-kanban-integration-prd.md`](../runner-kanban-integration-prd.md) | 控制面/执行面分离的 PRD 依据 |
| [`../docs/adr/0001-local-first-workspace.md`](adr/0001-local-first-workspace.md) | local-first 架构决策 |
| [`Loop-Engineering橙皮书-v260615.md`](Loop-Engineering橙皮书-v260615.md) | 六零件框架（Automation / Worktree / Skills / Connectors / Sub-agents / Memory）——评估新项目时的对照标尺 |
