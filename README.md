# code-agent-acp

一个 Git 仓库，四个独立 Go 模块，把四个编码 agent 分别接入 **ACP（Agent Client Protocol）v1**：`kimi`、`minimax`、`workbuddy`、`zcode`。

每个模块有自己的 `go.mod`、`Makefile` 和 `cmd/` CLI 入口，**独立构建、独立测试、互不 import、不共享代码**；没有根 Go module。仓库级约定见 [AGENTS.md](AGENTS.md)。

```
ACP 宿主 (Zed / JetBrains / auto-run / ...)
   │  stdin/stdout  ACP v1 ndjson (JSON-RPC 2.0)
   ▼
<module> client / agent / bridge  ──►  对应 agent 的 ACP 入口
```

## 模块与版本

**项目版本：`00.00.01`**（记录在根目录 [VERSION](VERSION)）。

每个 ACP 客户端持有**独立版本号**，从 `00.00.01` 起各自递增，模块之间互不关联：

| 模块 | CLI | 主要角色 | 独立版本 | 版本定义（唯一事实来源） |
| :--- | :--- | :--- | :--- | :--- |
| [kimi/](kimi/) | `kimi-acp` | client / agent / bridge | `00.00.02` | [`kimi/cmd/kimi-acp/main.go`](kimi/cmd/kimi-acp/main.go) `const version` |
| [minimax/](minimax/) | `minimax-acp` | client / agent / bridge | `00.00.02` | [`minimax/cmd/minimax-acp/main.go`](minimax/cmd/minimax-acp/main.go) `const version` |
| [workbuddy/](workbuddy/) | `workbuddy-acp-bridge` | ACP stdio server ↔ WorkBuddy 网关 | `00.00.01` | [`workbuddy/internal/protocol/protocol.go`](workbuddy/internal/protocol/protocol.go) `VersionString` |
| [zcode/](zcode/) | `zcode-acp-bridge` | ACP agent ↔ headless ZCode CLI | `00.00.01` | [`zcode/cmd/zcode-acp-bridge/main.go`](zcode/cmd/zcode-acp-bridge/main.go) `defaultBridgeVersion` |

版本号随时可以用 `<cli> version` 查询，例如 `kimi-acp version` 输出 `kimi-acp 00.00.01`。

### 版本策略

- **格式**：`MM.mm.pp` 三段两位补零（如 `00.00.01`），与语义化版本对应——首位给不兼容的协议/CLI 契约变更，中位给新功能，末位给修复。
- **独立递增**：四个模块的版本号互不联动，只改了哪个模块就只升哪个；跨模块协同变更时各自同时升一版。
- **项目版本**：根目录 `VERSION` 记录仓库整体版本，与模块版本独立维护。
- **发布覆盖**：`zcode-acp-bridge` 发布构建会用 ldflags 注入版本（见 `zcode/.goreleaser.yaml`），未注入时报告 `defaultBridgeVersion`；其余模块以代码常量为准。

## 构建与测试

各模块需在模块目录内执行（无根 module）。Kimi / MiniMax / WorkBuddy 声明 Go 1.22，ZCode 声明 Go 1.26.1。

| 模块 | 构建 | 测试 | 静态检查 |
| :--- | :--- | :--- | :--- |
| kimi | `make -C kimi build` → `kimi/bin/kimi-acp` | `make -C kimi test` | `make -C kimi vet` |
| minimax | `make -C minimax build` → `minimax/bin/minimax-acp` | `make -C minimax test` | `make -C minimax vet` |
| workbuddy | `make -C workbuddy build` → `workbuddy/bin/workbuddy-acp-bridge`（另产 `bin/fake-acp-server` 夹具） | `make -C workbuddy test` | `make -C workbuddy vet` |
| zcode | `make -C zcode build` → `zcode/zcode-acp-bridge` | `make -C zcode test`（vet + 全量） | 含于 test |

补充：

- 各模块都有只读 `doctor` 目标（`make -C <module> doctor`），只做握手、不发 prompt；使用前先读各模块实现说明。
- ZCode 真机测试需 `-tags=real` 与本地 `env` 配置（模板 [zcode/env.example](zcode/env.example)），任务类测试会真实读写文件，请用隔离工作区；部分 fake-backend 测试依赖 Python 3。
- 编译产物（`bin/`、`dist/`）不入 Git；ACP stdout 永远只走协议帧，诊断一律走 stderr。

## 模块速览

| 模块 | 说明 | 详细文档 |
| :--- | :--- | :--- |
| **kimi** | 零依赖 Go ACP v1 实现：以 client 驱动 `kimi acp`、以 agent 提供确定性 fixture、透明 bridge | [kimi/README.md](kimi/README.md) |
| **minimax** | 驱动 MiniMax Code 原生 `mcode acp` server 的工具集：doctor / chat / bridge / serve，附编辑器接入模板 | [minimax/README.md](minimax/README.md) |
| **workbuddy** | 把 WorkBuddy PC 客户端本机 ACP 网关（HTTP/SSE）桥接为标准 ACP v1 stdio server | [workbuddy/README.md](workbuddy/README.md) |
| **zcode** | 把 headless ZCode CLI 桥接为 ACP agent 的单二进制，移植自 Python 版并保持语义等价 | [zcode/README.md](zcode/README.md) |
