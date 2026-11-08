# O04 verification — temporary-copy handoff

The implementation was developed in `/private/tmp/zorch-o04-gr474grt/worktree` from the O03 sibling checkout. The current managed session made `/Volumes/BAK3T/projects/kanban/zcode-orchestrator-go` read-only. The real checkout was not modified; apply the reviewed handoff before treating this as project progress.

## Passed checks

- Full real-SDK → actual bridge → fake ZCode contract suite with the race detector: PASS (5.674s). Uses the production two-second cooperative shutdown allowance.
- Contracts exercise model failure/mismatch without Prompt, file modification in the isolated workspace, actual validation commands, durable dispatch-before-output ordering, result persistence, explicit permission/form answers, unanswered permission expiry, active cancellation, and failed validation.
- Focused race suites for agent, SQLite, app, workspace, configuration, and domain: PASS.
- Active-budget pause/resume, child environment allowlist, and diagnostic credential-redaction tests: PASS.
- `go vet ./...`: PASS.
- `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...`: PASS (cross-build only).

## Unresolved check

`go test -count=1 -race ./...` FAILS at `TestProcessGroupStopsDescendant` because the managed sandbox rejects the group signal with `operation not permitted` (EPERM). All other packages in that run passed. This test remains enabled with its original safety assertion; it was not skipped or suppressed. Its fixture exits cooperatively during cleanup so the denied signal does not require leaving the test writer running.

The first contract attempt used a 100ms shutdown allowance and also reached the denied forced-stop path on expiry. The contract now uses the actual production two-second allowance, and cooperative shutdown passes. This does not validate SIGTERM/SIGKILL escalation: the separate forced-stop test still fails and must pass outside the restricted sandbox before O04 completion.

## Implementation evidence

Migration 003 stores execution phases and output. A fault-injection test proves that a failed dispatch event rolls back the phase instead of advancing it. Final outcome/state/events and a workspace result reference commit together; a pinned result cannot be cleaned. Terminal attempts reject late output events and outcome replacement. The O02 database upgrade test was updated to expect all three additive migrations; historical migrations were not edited.

Model/mode readback is mandatory before Prompt. Pending interactions are recorded before waiting and are not automatically accepted. Primitive answers are validated before reply-attempt audit. Active time pauses during waits and resumes without resetting. End-turn is followed by separate validation; no checks are recorded as not_configured rather than passed. Files are collected only after confirmed process shutdown. If cleanup or persistence cannot be confirmed, the durable workspace hold remains.

## Remaining work

1. Verify the baseline and apply the handoff patch to the real orchestrator checkout.
2. Run `make test` in a normal local environment that permits signals to owned child process groups, especially TestProcessGroupStopsDescendant.
3. Run `make test-execution BRIDGE_DIR=../zcode-open-bridge-go` against the matching bridge source.
4. Only then mark O04 complete and proceed to the O05 persistent interaction broker.

No real ZCode model, installation, public endpoint, or daemon was used. CLI remains version-only. The Answer hook is an internal context-aware callback, not the O05 API broker. Cross-restart process reconciliation, escaped process groups, full form schemas, exports, and public task APIs remain later work.
