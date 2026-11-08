# O04 handoff — not yet applied

The source orchestrator checkout was read-only under this session's managed permissions. This patch contains the implementation and its tests from a temporary copy; it has not changed the real sibling project.

Implemented: persisted execution phases and output, the real ACP SDK adapter, process-group supervision, context-bound interactive callbacks, active/wall budgets, actual validation commands, and pinned result/workspace manifests.

Verification: cooperative SDK → real bridge → fake ZCode contracts pass with race detection, as do storage/adapter/workspace/config checks, go vet, and Linux cross-build. The full suite still fails the **forced process-group shutdown** test with sandbox EPERM. That safety assertion remains enabled. See [VERIFICATION.md](VERIFICATION.md).

## Apply only in the orchestrator checkout

First verify the unchanged baseline. A mismatch means review/rebase the patch; do not overwrite unrelated changes.

```sh
cd /Volumes/BAK3T/projects/kanban/zcode-orchestrator-go
python3 ../zcode-open-bridge-go/docs/handoffs/o04/verify-base.py "$PWD"
git apply --check ../zcode-open-bridge-go/docs/handoffs/o04/o04.patch
git apply ../zcode-open-bridge-go/docs/handoffs/o04/o04.patch
```

Then run in a local environment permitting signals to owned child process groups:

```sh
make test
make test-execution BRIDGE_DIR=../zcode-open-bridge-go
```

Only mark O04 done after application and successful forced-stop verification. No real model, installation, CLI execution entry point, or daemon is part of this handoff.
