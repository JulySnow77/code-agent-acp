.PHONY: build test vet race doctor clean

BINARY := bin/claude-acp-bridge

# The binary is deliberately not committed: ACP hosts freeze its digest per
# run, so it must be an actual compiled artifact for the current platform.
build:
	@mkdir -p bin
	go build -trimpath -o $(BINARY) ./cmd/claude-acp-bridge
	@echo "built $(BINARY)"

test:
	go test ./... -count=1

race:
	go test -race ./... -count=1

vet:
	go vet ./...

# Read-only self check: checks `claude --version` and the stream-json flags
# without sending a prompt or calling the model.
doctor: build
	$(BINARY) doctor

clean:
	rm -f $(BINARY)
