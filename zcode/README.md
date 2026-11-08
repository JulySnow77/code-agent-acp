# zcode-open-bridge-go

把 headless ZCode CLI 桥接为 **ACP (Agent Client Protocol)** Agent 的单一 Go 二进制。
派生自 [tizerluo/zcode-open-bridge](https://github.com/tizerluo/zcode-open-bridge)(MIT,Python 版),
移植决策与全流程见 [docs/PORTING_PLAN.md](docs/PORTING_PLAN.md),协议事实见
[docs/zcode-protocol.md](docs/zcode-protocol.md)。

**状态(2026-09-18)**:Phase 0~5 完成 —— 后端链路、兼容层、doctor、record、
ACP 核心方法、事件翻译与流式输出、turn.failed 错误可见、扩展方法透传
(双名字 `_zcode/` 别名)、`session/setModel` 适配器、`prompt/enhance`、
人在回路(AskUserQuestion 多问题扇出与 multiSelect、工具权限确认 ——
`--mode build` 可用,仍默认 `yolo`)。双跑对比(fake 驱动)与 Python 版
**语义等价**。AskUserQuestion 移植自本地克隆(f256268 + baseline patch,
上游 3069013 无此功能)。

**omnigent 已切换到 Go 二进制(2026-09-18)**:`~/.local/bin/zcode-acp-bridge`
软链从 Python 版重指向 `/usr/local/bin/zcode-acp-bridge`(构建版本 c585846,
带 `go build -ldflags "-X main.bridgeVersion=…"` 版本戳;`zcode-acp-bridge version`
可查)。AcpExecutor 全链路(initialize → session/new 含 mcpServers → prompt →
流式 → 错误上报)实测通过且稳定;真机 model turn 因 zcode provider 全量
404(环境问题,见 docs/zcode-protocol.md C11 补充,Python 版同样失败)只能
验证到 D4 错误可见路径。回滚:`ln -sf
/Volumes/BAK3T/projects/kanban/zcode-open-bridge/packages/acp-bridge/zcode-acp-bridge
~/.local/bin/zcode-acp-bridge`。

注意:omnigent 注册命令里的 `ZCODE_MODE=build` 在 Python 版从来就是被忽略的
(Python 版硬编码 yolo),Go 版行为一致。Go 版连接 zcode 的**缺省 mode 就是
yolo**(跳过工具权限确认); 要收紧默认行为请用 `ZCODE_ACP_MODE=build`
(或启动 `zcode-acp-bridge serve --mode build`)。为兼容上游配置,
旧变量 `ZCODE_ACP_DEFAULT_MODE` 仍可作为兜底, 但 `ZCODE_ACP_MODE` 优先。

## 安装与使用

```bash
go build -ldflags "-X main.bridgeVersion=$(git describe --tags --always)" \
  -o /usr/local/bin/zcode-acp-bridge ./cmd/zcode-acp-bridge
```

### Zed 配置

```json
{
  "agent_servers": {
    "ZCode (Go bridge)": {
      "command": "/usr/local/bin/zcode-acp-bridge",
      "args": ["serve"],
      "env": {
        "ZCODE_BIN": "/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs",
        "ZCODE_ACP_MODEL": "<providerId>/<modelId>"
      }
    }
  }
}
```

### 子命令

| 子命令 | 作用 |
|---|---|
| `serve` | 以 ACP agent 运行(默认) |
| `doctor` | 启动自检:路径 / provider 配置 / 版本 / 协议 / 建会话计时 / 模型目录 |
| `record` | 录制协议样本到 `testdata/zcode-<ver>/`(ZCode 升级后用) |
| `version` | 版本与构建信息 |

### 主要环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZCODE_BIN` | `zcode` | 推荐 App 内置 CLI 全路径 |
| `ZCODE_ACP_MODEL` | 空 | `providerId/modelId[/reasoningLevel]`,建会话后立即 setModel |
| `ZCODE_ACP_MODE` / `--mode` | `yolo` | `yolo\|build\|edit\|plan`(Phase 4 前 build 不可用) |
| `ZCODE_ACP_CREATE_TIMEOUT` | `120s` | 建会话超时(大会话库冷启动实测 15~22s) |
| `ZCODE_ACP_TURN_TIMEOUT` | `30m` | 单 turn 预算 |
| `ZCODE_ACP_WIRE_LOG` | 空 | JSONL 线材日志目录 |
| `ZCODE_BUILTIN_PROVIDER_CONFIG_FILE` | 按路径推导 | provider 配置(0.16.5 启动必需) |
| `ZCODE_ACP_TRACE_NOTIF` | 空 | `=1` 时打印「被忽略的未知通知」。**默认连 `-v` 都不打**: 一次 turn 就有上千条 `v4/telemetry/event`, 打开会把其它诊断淹掉 |

### 在终端里体验 AskUserQuestion

模型的 `AskUserQuestion` 在终端里可以直接被人回答, 包括多选:

```bash
make ask-interactive          # 或 scripts/demo-ask.sh multi / single / two
```

会看到 (多选题):

```
🔷 人工应答 (elicitation/create, form)
  问题 1/1 [多选]: 这个项目需要覆盖哪些业务模块？（可多选）
    [1] 商品与库存      [2] 报表与数据分析
    [3] 用户与权限管理  [4] 订单与支付
  问题 1/1 可多选, 输编号 (如 1,3; 留空=都不选): ▸ 1,3
```

- 单选: 输编号或直接输 value; 留空 = 作废
- 多选: `1,3` / `1 3` / `1、3`; 留空 = 都不选
- 必须在**真实终端**里执行(需控制终端)

桥对此有两条路径, 由客户端是否声明 `elicitation.form` 决定:

| 客户端能力 | 协议 | 多选 |
|---|---|---|
| 声明 `elicitation.form` | `elicitation/create`(整组问题一次送达) | ✅ 支持 |
| 未声明 | `session/request_permission`(逐题) | ❌ 整组 cancel, 模型会改问单选 |

**问几次由 prompt 决定, 不是问一次就完。** 模型重复提问时终端会再弹一张表, 每一轮
都会正常 accept 回去; 但如果 prompt 只写「用 AskUserQuestion 问我 X」而没说「答完
之后做什么」, 模型可能拿到答案后不知道该干什么, 于是再问一遍。想要一次问完, 在
prompt 里补一句收尾指令 (例如「等我作答后用一句话复述你的选择, 不要重复提问」) ——
`scripts/demo-ask.sh` 的三个默认 prompt 都带了。

想对照未声明时的行为: `ZCODE_SMOKE_ASK_FALLBACK=1 make ask-interactive`。

在指定项目目录里干活时用 `make test-task-interactive DIR=… PROMPT="…"` —— 交互模式
下它同样声明 `elicitation.form`, 所以**多选题也支持**; 非交互的 `make test-task`
仍走降级路径且自动应答(多选题会被整组 cancel, 模型改问单选)。两个 target 在走过
AskUserQuestion 时都会核对「人的作答」与「zcode 实收的」是否一致, 而不是只看
`end_turn` —— 整组 cancel 后模型自己改问一遍, 同样会正常收尾。

## 与 Python 版的差异

- `turn.failed` **错误可见**:推送错误文本(`agent_message_chunk`)+ `session/prompt`
  返回 JSON-RPC error(带 attribution);`--legacy-turn-failed` 恢复旧行为。
- 不支持 CLI < 0.14.8 的轮询降级(只走事件订阅)。
- 子进程 stderr 保留最后 20 行并进入失败信息(上游丢 DEVNULL)。
- 版本探测、provider 配置推导、`ZCODE_BASE_URL` 残留自愈为启动自检项。

## 开发

```bash
go test ./...                    # 单元 + fake e2e
go test -tags=real ./test/smoke  # 真机冒烟 (需 ZCODE_BIN)
scripts/upstream-check.sh        # 上游巡检
scripts/record-protocol.sh       # ZCode 升级后实录协议
# 双跑对比 (Phase 5 验收): 同一 fake 驱动 Python 版与 Go 版, 比对归一化
# session/update 序列
python3 scripts/compare-bridges.py \
  --go-bin ./zcode-acp-bridge \
  --py-bin ../zcode-open-bridge/packages/acp-bridge/zcode-acp-bridge
```

上游跟踪台账:[docs/upstream-sync.md](docs/upstream-sync.md);基线锁定
`upstream.lock`(当前 `3069013`,2026-09-18)。

## License

MIT(派生自 tizerluo/zcode-open-bridge,见 `upstream/`)。
