.PHONY: build test vet clean doctor sessions fixtures

BINARY := bin/workbuddy-acp-bridge
FIXTURE := bin/fake-acp-server

# The binary is deliberately not committed: ACP hosts freeze its digest per
# run, so it must be an actual compiled artifact for the current platform.
build:
	@mkdir -p bin
	go build -trimpath -o $(BINARY) ./cmd/workbuddy-acp-bridge
	go build -trimpath -o $(FIXTURE) ./cmd/fake-acp-server
	@echo "built $(BINARY) and $(FIXTURE)"

test:
	go test ./... -count=1

vet:
	go vet ./...

doctor: build
	$(BINARY) doctor --json

sessions: build
	$(BINARY) sessions --json

clean:
	rm -f $(BINARY) $(FIXTURE)
