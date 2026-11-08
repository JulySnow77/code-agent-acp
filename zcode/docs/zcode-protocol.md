# ZCode App-Server 协议事实(按版本)

记录桥接层实测得到的 ZCode CLI app-server 协议事实。所有条目都来自**本机实录**,
不是推测;每个版本一节,升级 ZCode 后按 `PORTING_PLAN` §5.3 的流程补新节并录制
新样本。

- 样本目录:`testdata/zcode-<version>/`
- 兼容规则实现:`internal/compat/compat.go` 的 `Rules` 表(规则 ID 在下文用 `→ 规则` 标注)
- 首次成文:2026-09-18(Phase 1)

---

## 0.16.5(App 3.12.3,darwin-x86_64)

### 启动与握手

| 事实 | 值 / 说明 | 样本 |
|---|---|---|
| 子命令 | `app-server --stdio`(`agent-server` 等价;`--surface terminal\|desktop` 可选) | — |
| 冷启动耗时 | 本机(1.2 GB 会话库 `~/.zcode/cli/db/db.sqlite`)实测 20 s 量级:`zcode --version` 21 s、`session/create` 15~22.5 s | C2 → 规则 `create-timeout` |
| 线格式 | 换行分隔 JSON,双向;**请求信封不携带 `jsonrpc` 键**(0.16 起 zod 校验层拒收) | 全部样本 |
| 协议声明 | `session/create` 响应带 `result.protocol = {name:"ZCode Protocol", version:1}` | `session-create.response.json` |

### provider 配置(C1)→ 规则 `provider-config-path`

独立启动的 CLI 按**入口文件目录**相对路径找 `provider/zcode-builtin.json`(或
`../../../../../config/provider/zcode-builtin.json`),找不到直接退出,stderr:

```
无法定位 CLI ZCode Built-in Provider Config
```

上游 README 的 symlink 方案因此失效。Go 版注入 `ZCODE_BUILTIN_PROVIDER_CONFIG_FILE`,
取值优先级:

1. 用户已有的 `ZCODE_BUILTIN_PROVIDER_CONFIG_FILE`(doctor 输出标记「来源: 环境变量」;
   本机实际值 `~/.zcode/v2/runtime/provider/darwin-x86_64/3.12.3/endpoint-<hash>/zcode-builtin.json`,
   即 App 下载的运行时 provider 配置)
2. 按 `ZCODE_BIN` 真实路径推导 `<Resources>/config/provider/zcode-builtin.json`

### 会话方法

| 方法 | 请求要点 | 响应要点 | 样本 |
|---|---|---|---|
| `session/create` | `{mode, workspace:{workspacePath, workspaceKey}}` | `result.session.sessionId`;`result.settings.model.available[]`(模型目录,见下);`result.runtime.{eventSeq,stateRevision}`;`messages[]`、`projection`、`slashCommands[]`、`todos[]` | `session-create.*.json` |
| `session/subscribe` | 必填四字段 `{sessionId, deliveryKind:"desktop-continuous", includeSnapshot, afterSeq}`,缺字段报 ZodError(C6 → 规则 `subscribe-required-fields`) | 快照 + `eventSeq` | `session-subscribe.*.json` |
| `session/setModel` | 必须 `{sessionId, model:{providerId, modelId, options:{reasoningLevel}}}`;传旧 `{modelId}` 报 `Unrecognized key "modelId"`;部分模型 `reasoningLevel` 必填(`Reasoning level is required`);不在目录报 `Provider Registry 中不存在 Model`(C4 → 规则 `setmodel-upgrade`) | 更新后的 settings | `session-setmodel.*.json` |
| `session/send` | `content` 必须是字符串,数组报 `expected string, received array`(C5 → 规则 `send-content-flatten`) | 立即 ack(不等 turn 结束) | `session-send.*.json` |

**模型目录形状(C3)** —— `settings.model.available[]` 每项:

```json
{
  "ref": {"providerId": "…", "modelId": "…"},
  "label": "…", "providerLabel": "…",
  "contextWindow": 200000,
  "reasoning": {
    "defaultLevel": "max",
    "levels": [{"label": "disabled", "value": "disabled"}, …]
  }
}
```

注意 `reasoning.levels` 是 **`{label,value}` 对象数组**,不是字符串数组(计划阶段
按字符串解析得到空列表,导致 setModel 被 `Reasoning level is required` 拒绝;已修,
回归测试 `TestParseModelCatalogRecorded`)。另:同名 `modelId` 可挂在多个 provider 下
(实录 `gpt-5.5` 出现两次),裸 modelId 解析必须做歧义检测。

本机目录共 19 个模型,全部是用户自建 provider —— **没有**内置 `builtin:bigmodel-*`
(见 C11)。

### 事件信封(C7)

`session/event` 通知:

```json
{"method": "session/event", "params": {
  "type": "…", "payload": {…},
  "seq": 6, "eventId": "…", "sessionId": "sess_…", "turnId": "…",
  "timestamp": …, "deliveryKind": "desktop-continuous"}}
```

实录出现过的 `type`: `session.titleUpdated`、`session.updated`(payload 带 `type` 子类型)、
`turn.started`、`turn.failed`。上游翻译器(基线 3069013)另依赖:`model.streaming`
(`payload.kind = text_delta | reasoning_delta`,0.16 起增量为 `payload.delta`)、
`tool.updated`(`payload.kind = scheduled | started | progress | result`,带 `toolCallId`)、
`turn.completed`(usage 明细,`contextWindow` 缺失,用订阅快照兜底)。
未知 type 忽略并计数 → 规则 `unknown-notification`。

### server→client 反向请求(C8)

**必须应答** `session/requestRuntimePreferences`(scope: `runtime-materialization` /
`user-execution`),不应答则 `session/create`、`session/send` 永久挂起(上游 832d647
注释:"旧版桥无此代码路径,即死于此")。桥统一回:

```json
{"nativeSearchEnhancementsEnabled": false, "memoryEnabled": false,
 "askUserQuestionAutoResolutionEnabled": false}
```

实录完整清单(`server-requests.json`):`session/requestRuntimePreferences`、
`interaction/requestOfficialMcpAuthHeaders`(未识别,回 `-32601`)。
已知全集另含 `interaction/requestUserInput`、`interaction/requestPermission`(有 handler)。
未识别一律 `-32601`,不致命 → 规则 `unknown-server-request`。

### 通知(丢弃,C9)

高频、桥不消费:`startup/storageState`(启动阶段连发多条)、`state.updated`、
`computer-use/operation-event`、`v4/telemetry/event`、`process/mcpTelemetry`。
静默丢弃,`-v` 下打日志 → 规则 `unknown-notification`。

### 失败面(C10)

`turn.failed` 载荷:

```json
{"type": "turn.failed", "payload": {
  "error": {
    "type": "…", "code": "model_request_failed", "message": "Model request failed.",
    "detail": "…", "underlyingErrorMessage": "…",
    "attribution": {"statusCode": 404, "retryable": false,
                    "providerId": "…", "modelId": "…", "providerKind": "…", "transport": "…"}},
  "turnPhase": "…"}}}
```

实录两例:`turn-failed.jsonl`(内置 GLM provider 404)、`turn-failed-glm-5.2.jsonl`
(自建 provider 429/限额)。两例的完整事件序列都是:
`session.titleUpdated → session.updated×3 → turn.started → turn.failed`。
Go 版处理(D4):推错误文本给客户端 + `session/prompt` 返回 JSON-RPC error,
不再伪装 `end_turn`。

### 凭证与环境(C11、C12)

**C11 调查结论(2026-09-18,Phase 1)**:独立 CLI 进程的模型目录里没有内置
`builtin:bigmodel-*` provider,只有用户自建 provider。凭证存于
`~/.zcode/v2/credentials.json`,值为 `enc:v1:<nonce>.<ciphertext>.<tag>`
(AES-256-GCM),密钥 = SHA-256(secret),secret 缺省取 fallback:

```
zcode-credential-fallback:<platform>:<home>:<user>
```

本机用 Node `crypto`(与 App 同实现)验证:**fallback secret 可解密全部 8 条凭证
(ok=8 fail=0)**,包括 coding-plan 的 api-key。结论:**桥不需要 App 配合注入
`ZCODE_CREDENTIAL_SECRET`**,独立进程直接可用 `~/.zcode/config.json` 的 provider
(`internal/creds`)。适用条件:用户未在 App 侧设置自定义 secret;若设置了,
需显式传 `ZCODE_CREDENTIAL_SECRET`(doctor 会打印注入值,api-key 脱敏)。

**2026-09-18 补充(Phase 2 真机冒烟)**:凭证可解密 ≠ 端点可用。本机目录里
全部自建 provider 实测均被 provider 端点拒绝(glm-5.2 → 404、stealth/ox-alpha →
404、gpt-5.5 → 403 auth_failed;attribution 里 `providerKind: anthropic|openai`,
transport=sse)。即:桥能正确解密注入 key、setModel 生效、请求确实打到了所选
模型,但 key 本身不被第三方端点接受(疑似 key 过期或端点校验桌面 App 上下文)。
在独立进程下想跑通成功 turn,需要一个确认有效的 provider/model —— 桥对失败
turn 的呈现(D4:错误文本 + attribution)已验证无误。

透传环境变量(C12):`ZCODE_DATA_BASE_DIR`、`ZCODE_BUILTIN_PROVIDER_CONFIG_FILE`、
`ZCODE_PERSONAL_PROVIDER_CONFIG_FILE`、`ZCODE_CREDENTIAL_SECRET`、`ZCODE_BASE_URL`、
`ZCODE_ENDPOINT_ORIGIN`、`ZCODE_APP_VERSION`、`ZCODE_HTTP_PROXY`、`ZCODE_NO_PROXY`。

`ZCODE_BASE_URL` 残留检测:env 值若是 config 里**另一个** provider 的 endpoint
(旧 plan 残留,本机实测 `https://zcode.z.ai` vs config 的
`https://open.bigmodel.cn/api/anthropic`),以 config 值为准并告警。

### 构建漂移警告

同号 `0.16.5` 的不同 App 构建方法集不同(上游 2d449ff 实测 App 3.12.3 构建删掉了
`workspace/*` 7/8 与 `updateRuntimeModelConfig`)。因此:版本号只用于「引入版本」型
规则;**能力是否真存在一律以后端 `-32601` 为准**,透传方法被拒时映射为
「当前 ZCode 版本已移除该能力」→ 规则 `passthrough-32601-removed`。

### 样本敏感信息检查

所有入库样本已检查:不含 `apiKey` / `sk-` / token / Bearer / api-key 前缀。

---

## 协议探测方法(v16 vs ≤0.15)

以旧信封(含 `jsonrpc` 键)发 `workspace/readState`:

- 0.16+ zod 拒收 → `-32600 invalid_union`,且该错误帧 id 是固定占位符
  `"invalid-message"`(非请求 id)→ 判 v16
- ≤0.15 理解信封 → 回显 probe id 的正常/错误响应 → 判 legacy
- 超时 → 暂判 v16,标 `confirmed=false`,允许被 subscribe 失败推翻

探测在后台 goroutine 执行(冷启动 20 s 量级,不能阻塞 initialize);窗口默认 45 s。

## 新版本录制清单(`scripts/record-protocol.sh` 的产出物)

对每个新版本,`testdata/zcode-<ver>/` 至少包含:

| 文件 | 内容 |
|---|---|
| `session-create.{request,response}.json` | 建会话往返(含完整模型目录) |
| `session-subscribe.{request,response}.json` | 订阅往返(含快照) |
| `session-send.{request,response}.json` | 发送往返 |
| `session-setmodel.{request,response}.json` | setModel 新格式往返 |
| `turn-*.jsonl` | 至少一次成功 turn 与一次 `turn.failed` 的全部 `session/event` |
| `server-requests.json` | 出现过的全部 server→client 请求方法名及次数 |

`record` 子命令(`cmd/zcode-acp-bridge/record.go`)可自动产出上述全部文件。
