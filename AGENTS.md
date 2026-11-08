# Repository Guidelines

## Project Structure & Module Organization

Four independent Go modules share one Git repository: `kimi/`, `minimax/`, `workbuddy/`, and `zcode/`. Each has its own `go.mod`, `Makefile`, and CLI entry point under `cmd/`. There is no root Go module or workspace; run Go commands inside the relevant module. Keep modules independently buildable without sibling imports or nested Git repositories.

Kimi and MiniMax organize protocol, client, agent, and proxy code in `acp/`, `client/`, `agent/`, and `bridge/`. WorkBuddy and ZCode primarily use `internal/`. Tests generally sit beside source files; ZCode also has `test/e2e/`, `test/smoke/`, and `testdata/`. Module-local `docs/` and `examples/` hold documentation and integration examples.

## Build, Test, and Development Commands

Kimi, MiniMax, and WorkBuddy declare Go 1.22; ZCode declares Go 1.26.1. Use a compatible toolchain.

- `make -C kimi build`: build `kimi/bin/kimi-acp`. Each module supports `make build`.
- `make -C minimax test`: run module tests. Each module supports `make test`; ZCode also runs vet.
- `(cd workbuddy && go vet ./...)`: perform static checks; use the same command in other modules.
- `(cd kimi && go test -race ./...)`: check concurrent behavior.
- `make -C workbuddy doctor`: build and diagnose the local WorkBuddy connection. Other modules also expose `doctor`; consult their implementation before contacting real agents.

## Coding Style & Naming Conventions

Use `gofmt` and standard Go tab indentation. Keep package names lowercase, exported identifiers in PascalCase, and unexported identifiers in camelCase. Preserve existing module paths and CLI names. Keep ACP stdout exclusively for protocol frames; send diagnostics to stderr.

## Testing Guidelines

Use Go's standard `testing` package, `*_test.go` files, and `TestXxx` functions. Cover changed behavior with deterministic fixtures, including cancellation, permission requests, and failure paths. No numeric coverage threshold is configured. Python 3 enables some ZCode fake-backend tests. Real ZCode tests require `-tags=real` and local configuration; use isolated workspaces because task tests can modify files. Report fixture results separately from real-agent verification.

## Commit & Pull Request Guidelines

The repository has no commits yet, so no established message convention exists. Prefer imperative, module-scoped subjects such as `fix(workbuddy): handle cancelled streams`. Keep commits focused. PRs should describe the problem, affected modules, behavior changes, related issues, and exact validation commands/results. Include sanitized protocol evidence when relevant.

## Configuration & Generated Files

Use `zcode/env.example` as the configuration template. Keep credentials, local `env` files, binaries, `dist/`, and private session traces out of commits. Rebuild executables locally and redact recorded evidence.
