GO ?= go
BIN ?= bin
NAME ?= kimi-acp

.PHONY: all build test vet race clean doctor

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

# 只读握手：对真实 kimi acp 做 initialize + session/list，不发 prompt。
# kimi 的 doctor 恒定输出 JSON，没有 --json 旗标（区别于 minimax）。
doctor: build
	$(BIN)/$(NAME) doctor

clean:
	rm -rf $(BIN)
