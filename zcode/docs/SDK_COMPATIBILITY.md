# ACP SDK compatibility — O01

The contract suite uses `github.com/coder/acp-go-sdk v0.13.5` as the actual client,
the built bridge process, and `test/fakeserver`. Run:

```sh
ZCODE_E2E_RACE=1 go test -count=1 -race ./test/e2e -run '^TestSDK' -timeout 180s
go test -count=1 -race ./internal/jsonrpc
```

`ZCODE_E2E_RACE=1` also instruments the child bridge binary. These tests do not
start real ZCode or load real provider credentials.

## Cancellation extension v1

The bridge advertises `_meta._zcode.cancelRequest: true` in its initialize result.
A client opts in using this initialize request fragment:

```json
{
  "clientCapabilities": {
    "_meta": {"_zcode": {"cancelRequest": true}},
    "elicitation": {"form": {}}
  }
}
```

For opted-in clients, permission and form requests include
`_meta._zcode.expiresAt` (UTC RFC3339Nano). Existing `_meta.zcode` question/value
metadata remains available. When the bridge abandons a request it sends
`$/cancel_request` with `{"requestId":"brq-N"}`; the SDK cancels the inbound
callback context. Callback implementations must observe that context. The
bridge removes the pending request, so a late answer cannot grant permission.

Clients that do not opt in retain the legacy reverse-result behavior, covered
by the existing interaction/elicitation tests. This legacy behavior does not
cancel a coder SDK callback and is not suitable for the new orchestrator.

EOF/read error cancels connection handlers before joining them. For a closable
reader, cancellation also unblocks a pending read. A non-closable reader must
be unblocked by its owner. An interrupted or timed-out active turn requests
backend stop, allowing up to two seconds for acknowledgement before returning.
Stop acknowledgement does not prove all descendants exited: the orchestrator's
O04 supervisor must still terminate and reap its managed process tree.

## Scope and limits

The SDK tests cover new/resume with cwd binding, mode/model configuration and
model failures, sequential prompts, permissions, plan approval followed by
tool permission, form values including explicit all-false multi-select,
cancel/interaction timeout/turn timeout/EOF, late answers, and backend errors.

The orchestrator must issue `session/cancel` to stop a turn. Cancelling only a
local SDK Prompt context sends a separate request cancellation notification;
the bridge does not currently treat that notification as session cancellation.
Forms in SDK v0.13.5 do not retain the bridge's top-level sessionId/toolCallId,
so one active connection must be bound to one session/attempt. Resume does not
replay historical events or restore in-flight approvals. MCP, fs, terminal,
fork, and arbitrary extension dispatch are outside this client contract.

Validation uses a fake backend; actual ZCode and Linux runtime validation are
separate release checks. The sibling orchestrator's bridge.lock.json records
the tested working-tree files because the bridge has pre-existing uncommitted
changes and HEAD alone does not identify the tested source.
