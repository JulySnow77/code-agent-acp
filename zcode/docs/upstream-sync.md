# 上游同步台账

- 台账格式：`docs/upstream-sync.md`（本文件），每次巡检一节
- 已审阅水位：`upstream.lock`（一行：`<commit> <日期>`）
- 巡检脚本：`scripts/upstream-check.sh`

基线变更记录见 §基线修正（2026-09-18）。

---

## 参考项目（上游）Git 信息

本项目以 **git submodule** 形式只读引用上游参考项目，所有移植决策均以它为基线。

| 项 | 值 |
|---|---|
| 上游仓库 | https://github.com/tizerluo/zcode-open-bridge |
| 协议 / License | MIT |
| 本地 submodule 路径 | `upstream/`（`upstream/.git` → `../.git/modules/upstream`） |
| 跟踪分支 | `origin/main` |
| 锁定 commit（完整） | `30690138aeefc1fc71feaadbb20fd65f8dc82073` |
| 锁定 commit（短） | `3069013` |
| 锁定日期 | 2026-09-18 |
| 当前 HEAD 最新提交 | `3069013` Merge pull request #34 from tizerluo/feat/f6-quota-wall |

> 注：`upstream/` 当前 HEAD 仍停在锁定的 `3069013`（即 `origin/main` 在该日的顶端），
> 其最新提交为 quota-wall 功能分支合入，尚未进入本项目的台账巡检（见 §下次巡检待办）。

同步与更新流程见 `docs/PORTING_PLAN.md` §6（上游跟踪流程）与 `scripts/upstream-check.sh`；
水位锁定在 `upstream.lock`（一行：`<commit> <日期>`）。

---

## 基线修正（2026-09-18）

`docs/PORTING_PLAN.md` 原定基线为 `3e8d2b9`（2026-07-27）。首次 fetch 后发现两处问题，已改基线：

| # | 发现 | 处置 |
|---|---|---|
| B1 | `3e8d2b9` **不是** `origin/main` 的祖先（`git merge-base --is-ancestor` 为 false）。它是本地克隆 `main` 分支上的一次本地合并（Merge branch 'feat/ask-user-question'），从未进入上游。计划作者当时未 fetch，误把本地分支当上游基线 | 改以 `origin/main` 为唯一基线源 |
| B2 | 上游自 2026-07-27 起有 **9 个提交直接改 `packages/acp-bridge`**（+743/−412），其中最关键是 `832d647`「适配 CLI 0.16.1（反向调用应答）」和 `2d449ff`「适配 App 3.12.3（同号 0.16.5 构建漂移）」。按旧基线移植出的 Go 版在当前安装的 ZCode 0.16.5 上**缺反向调用应答路径，必死** | 基线改到 `30690138aeefc1fc71feaadbb20fd65f8dc82073`（2026-09-18） |
| B3 | 上游已**删除** `tests/fakes/zcode_app_server`（464 行的可执行假 app-server）。计划 Phase 1/3 的验收标准依赖它 | 从本地 3e8d2b9 工作树取出存档到 `test/fakepy/zcode_app_server`，Phase 1~3 用它；Phase 4 起换成 Go 版 `test/fakeserver` |
| B4 | 本地克隆的 4 个未提交改动（bridge +76、fake +255、AskUserQuestion 测试 +468）是相对本地分支的，内容与上游已重写的区域重叠 | 仍导出为 `docs/upstream/local-baseline.patch` 存档；**不再默认全部移植**，逐条核对后决定（见 §本地未提交改动） |

`upstream.lock` 现值：

```
30690138aeefc1fc71feaadbb20fd65f8dc82073 2026-09-18
```

---

## 巡检 2026-09-18（首次，覆盖 2026-07-27 → 3069013）

### 涉及 `packages/acp-bridge` / `shared/` 的提交 → 一律「移植」

| 上游 commit | 日期 | 标题 | 决定 | 理由 | Go 侧落地 |
|---|---|---|---|---|---|
| `832d647` | 08-06 | 适配 ZCode CLI 0.16.1 协议（反向调用应答 / 双模探测 / 事件翻译层重写） | **移植** | 反向调用应答是 0.16+ 的生死路径：server 在 `session/create`、`session/send` 时会反向发 `session/requestRuntimePreferences` 等请求并**等应答**，不应答则永久卡住。事件翻译层重写定义了新的 payload 形状 | Phase 1 `internal/zcode/backend.go` 的 server 请求分发；Phase 2 `internal/bridge/translate.go` |
| `bb7ada5` | 08-06 | `turn.failed` 无条件判失败 + usage size 缓存兜底 | **移植**（已覆盖） | 与本计划 §8 D4「错误必须可见」一致 | `bridge/turn.go` + `--legacy-turn-failed` |
| `29b8794` | 08-06 | 投影回合重置 + cancel 单点化 + 探测超时三态回退 | **移植**（部分） | cancel 单点化 + 探测超时三态回退与 Go 版并发模型相关；投影回合重置属 `ProjectionDiffer`，本计划 §1.2 已放弃轮询降级，**只移植 cancel 单点化** | `bridge/turn.go` |
| `aa06769` | 08-06 | P3 nit：cancel 错误吞没用例 / isinstance 守卫 / prompt 别名文档化 | **移植**（别名部分） | prompt 别名与 §3.2 规则 5「扩展方法双名字」一致 | `bridge/passthrough.go` |
| `b8ff37b` | 08-08 | 整体 deep review 全量修复（无 P0/P1，P2 清零） | **移植** | 需逐条看；P2 级修复大多落在错误处理与边界 | 待 Phase 2~4 逐条 |
| `7903e9e` | 09-01 | 凭证兜底模型 GLM-5.2 → GLM-5.3（shared 权威版 + 三处内嵌副本） | **移植** | 兜底模型名随上游走 | `internal/creds` |
| `2d449ff` | 09-18 | 适配 App 3.12.3（同号 0.16.5 构建漂移）+ 透传 `-32601` 泛化 | **移植** | 版本同号但构建漂移：`workspace/*` 删 7/8、`updateRuntimeModelConfig` 删除。上游已放弃硬编码方法清单，改为「后端返 -32601 即按已移除降级」 | `internal/compat` + `bridge/passthrough.go` |
| `250284a` | 09-18 | P3：SKILL 口径同步 + short 死分支简化 + D7 核心路径守护用例 | **移植**（死分支部分） | 核心路径守护用例可用于保护 create/send/list/resume 的错误文案不被「已移除」文案掩盖 | `bridge/*_test.go` |
| `e423eca` | 09-18 | style：桥模块头 -32601 规则陈述合一 | 跳过 | 纯注释 | — |

### 其他路径 → 按 §6.3 默认决定

| 上游 commit | 涉及 | 决定 |
|---|---|---|
| `1ca4f61` quota-wall、`62fa71c` ruff | `packages/review-gate` | **跳过**（非 ACP 桥） |
| `42b27ab`…`f20cdd0` review-gate 系列（约 12 个） | `packages/review-gate`、`packages/mcp-server` | **延后到 backlog**（§1.2：mcp-server 与 ACP 桥无耦合） |
| `457ba5f` / `26cf6ec` agent-help 0.16.5 | `packages/agent-help` | **延后到 backlog** |
| `6b8cc8f` / `d17a2aa` / `82cf1c0` / `e70fd85` SKILL 文档 | `skills/` | **延后到 backlog** |
| `7ca2fc4` / `f3de60e` 适配 0.16.5 复测报告 | `docs/` | **设计已覆盖**（本计划 §2.2 的独立实测与之一致，互为交叉验证） |
| `90ed030` gitignore | — | 跳过 |
| `tests/test_review_gate.py`、`tests/test_security_review.py` | 新测试 | 跳过（非桥） |
| `tests/test_app_server_methods.py`（+1374）、`tests/test_event_translator.py`（+781）、`tests/test_prompt_enhance.py`（+351）、`tests/test_credentials.py`（+331） | 桥测试 | **移植**（先搬测试再搬实现；Phase 2~3） |

### 本地未提交改动（`docs/upstream/local-baseline.patch`，986 行）

| 文件 | 行数 | 处置 |
|---|---|---|
| `packages/acp-bridge/zcode-acp-bridge` | +72/−4 | 内容为 wire log、perm_responses 上限、多问题 reply 处理。**perm_responses 上限与多问题 reply 上游已在 `b8ff37b`/`29b8794` 重写区处理**，逐条核对后移植；wire log 已在本计划 §3.1 独立设计（`internal/wirelog`） |
| `tests/fakes/zcode_app_server` | +255 | 上游已删除该文件。存档到 `test/fakepy/zcode_app_server`，Phase 1~3 复用，Phase 4 换 Go 版 |
| `tests/test_ask_user_question.py` / `_e2e.py` | +468 | 上游已删除这两个文件。作为 Phase 4 的行为参考资料，**不作为测试基线**（Phase 4 按上游新测试重写） |

---

## 下次巡检待办

- [ ] 跑 `scripts/upstream-check.sh`，把 `3069013..origin/main` 的新提交按上表分类
- [ ] 检查 ZCode App 版本是否变化（`defaults read /Applications/ZCode.app/Contents/Info.plist CFBundleShortVersionString` 与 `zcode --version`）；变化则执行 §5.3 实录
