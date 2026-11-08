GO ?= go
BIN ?= bin
NAME ?= minimax-acp
DATE ?= $(shell date +%Y%m%d)
PROBE_OUT ?= docs/probes/doctor-go-$(DATE).json
PROBE_TRACE ?= docs/probes/doctor-trace-$(DATE).ndjson

.PHONY: all build test vet race fmt check clean doctor probe

all: build test

build:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/$(NAME) ./cmd/$(NAME)

test:
	$(GO) test ./... -count=1

race:
	$(GO) test ./... -race -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

check: vet test

# 只读握手：CLI/环境检查 + initialize（+ agent 声明支持时的 session/list）。
# 默认不发 session/prompt、不创建会话；--json=false 可看人类可读摘要。
doctor: build
	$(BIN)/$(NAME) doctor --json --handshake-timeout 30s

# 只读证据采集：即使 agent 无响应也保留报告（- 前缀忽略退出码）。
# 成功后请人工核对 docs/probes/index.json 的 verified / notVerified。
probe: build
	@mkdir -p docs/probes
	@echo "probe report -> $(PROBE_OUT)"
	-$(BIN)/$(NAME) doctor --json --handshake-timeout 30s --trace $(PROBE_TRACE) > $(PROBE_OUT)
	@echo "trace        -> $(PROBE_TRACE)"

clean:
	rm -rf $(BIN)
