BIN := zcode-acp-bridge

.PHONY: build test test-real test-task test-task-interactive test-ask ask-interactive print-version lint fmt vet doctor smoke upstream-check clean

build:
	go build -o $(BIN) ./cmd/zcode-acp-bridge

test:
	go vet ./...
	go test -timeout 30m ./...

# 真机冒烟: 冷启动 ~20s, 总时长分钟级。
# 参数不再靠 export —— 从项目根的 env 读 (cp env.example env 后按本机路径填),
# TestMain 会把它注入测试进程; TestSmokeEnvFromFile 逐个 KEY 核对装载结果。
# 想临时换一份配置: ZCODE_SMOKE_ENV_FILE=/path/to/other-env go test ...
test-real:
	go test -tags=real -count=1 -timeout 10m ./test/smoke

# 在自己指定的项目目录里跑自己指定的任务 —— 整个 target 就两个参数,
# 对应 TestSmokeRealTask 的唯二入参:
#
#   make test-task DIR=/path/to/project PROMPT="给 main.go 加上单元测试"
#
# 懒得敲就写进 env: ZCODE_SMOKE_PROJECT_DIR / ZCODE_SMOKE_PROMPT (那时直接 make test-task)。
# ⚠ 模型会在 DIR 里真的创建/修改/删除文件 —— 挑 git 干净的目录。
# 另有 ZCODE_SMOKE_TASK_TIMEOUT_MIN 控制等一个 turn 收尾的上限 (默认 20 分钟)。
test-task:
	go test -tags=real -count=1 -timeout 30m ./test/smoke -run '^TestSmokeRealTask$$' -v \
	  -args -task.dir="$(DIR)" -task.prompt="$(PROMPT)"

# 真机验证 AskUserQuestion: 逼模型先提一个带选项的问题, 客户端回答后断言
# 线材日志里桥回灌给 zcode 的是 accept + 非空 answers (并非 cancel / 卡死)。
# 默认提示词可用 ZCODE_SMOKE_ASK_PROMPT 覆盖, 工作区用 ZCODE_SMOKE_ASK_WORKSPACE。
test-ask:
	go test -tags=real -count=1 -timeout 12m ./test/smoke -run '^TestSmokeRealAsk$$' -v

# 终端交互式 AskUserQuestion demo: 问题+选项直接打到终端, 从**控制终端
# (/dev/tty)** 读你的选择, 选完断言桥把答案 accept 回灌给 zcode。
#
# 交互模式下客户端会声明 elicitation.form 能力, 桥因此走 elicitation/create
# (整组问题一次送达), **多选题也支持** (按 1,3 / 1 3 勾选, 留空=都不选);
# 单选输编号或直接输 value, 留空=作废。
#
# 想要对照「客户端不支持多选」的行为 (桥会整组 cancel、模型被迫改问单选),
# 设 ZCODE_SMOKE_ASK_FALLBACK=1 强制走降级路径 (session/request_permission)。
#
# 为什么必须开 /dev/tty: go test 会把测试进程的 stdin 接到 /dev/null, 读 os.Stdin
# 一定立刻 EOF (现象「读 stdin 失败, 作废」), 所以绕开 stdin 直连控制终端。
# 前提是进程有控制终端 —— 在真实终端里直接执行即可; 后台任务/CI 里跑仍会作废。
#
# 可选:
#   ZCODE_SMOKE_ASK_PROMPT="..."      覆盖默认提示词 (默认会逼模型提一个带选项的问题)
#   ZCODE_SMOKE_ASK_WORKSPACE=<dir>  指定工作区 (默认临时目录, 本用例不落文件)
ask-interactive:
	@$(MAKE) --no-print-directory print-version
	ZCODE_SMOKE_INTERACTIVE=1 go test -tags=real -count=1 -timeout 12m ./test/smoke -run '^TestSmokeRealAsk$$' -v

# 开场先报桥的版本。真机验证默认从工作树现编, 而日常入口用的是已安装那份 ——
# 两者常常不是同一份代码, 不先把版本摆出来, 后面看到的行为就没法归属。
# 优先级与 test/smoke 的 bridgeBinary 一致 (ZCODE_ACP_BRIDGE_BIN 指定的先用),
# 免得这里报的版本和实际被测的对不上。
print-version:
	@bin="$${ZCODE_ACP_BRIDGE_BIN:-}"; \
	if [ -z "$$bin" ]; then \
	  bin="$$(mktemp -d)/zcode-acp-bridge"; \
	  go build -o "$$bin" ./cmd/zcode-acp-bridge || exit 0; \
	fi; \
	echo "── zcode-acp-bridge 版本 ─────────────────────────"; \
	"$$bin" version 2>&1 | sed 's/^/  /'; \
	echo "  路径: $$bin"; echo

# 交互式真机冒烟: 同 test-task, 但权限/提问请求不再自动应答 —— harness 把问题+
# 选项打到终端, 从**控制终端 (/dev/tty)** 读你的选择 (回车=作废 / 数字=选项编号 /
# 其它=optionId)。适合手动验证 AskUserQuestion 这类「要人答」的流程。
#
# 为什么必须开 /dev/tty: go test 会把测试进程的 stdin 接到 /dev/null, 读 os.Stdin
# 一定立刻 EOF (现象「读 stdin 失败, 作废」), 所以这里绕开 stdin 直连控制终端。
# 前提是进程有控制终端 —— 在真实终端里直接执行即可; 后台任务/CI 里跑仍会作废。
test-task-interactive:
	ZCODE_SMOKE_INTERACTIVE=1 go test -tags=real -count=1 -timeout 30m ./test/smoke -run '^TestSmokeRealTask$$' -v \
	  -args -task.dir="$(DIR)" -task.prompt="$(PROMPT)"

doctor: build
	ZCODE_BIN=$${ZCODE_BIN:-/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs} ./$(BIN) doctor

smoke: build
	ZCODE_BIN=$${ZCODE_BIN:-/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs} ./$(BIN) record --out /tmp/zbridge-record

upstream-check:
	bash scripts/upstream-check.sh

fmt:
	gofmt -w cmd internal test

clean:
	rm -f $(BIN)
