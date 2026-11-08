#!/usr/bin/env bash
# 用法: scripts/upstream-check.sh            # 只看
#       scripts/upstream-check.sh --bump     # 审阅完毕后把 upstream.lock 推到 origin/main
set -euo pipefail
cd "$(dirname "$0")/.."
git -C upstream fetch origin --quiet
last=$(cut -d' ' -f1 upstream.lock)
head=$(git -C upstream rev-parse origin/main)
if [ "$last" = "$head" ]; then echo "上游无新提交 ($last)"; exit 0; fi
echo "== 上游新提交 $last..$head"
git -C upstream log --date=short --format='%h %ad %s' "$last..$head"
echo; echo "== 变更文件（按优先级）"
git -C upstream diff --stat "$last..$head" -- packages/acp-bridge tests/fakes tests/test_event_translator.py tests/test_app_server_methods.py tests/test_ask_user_question*.py
echo "-- 其他"
git -C upstream diff --stat "$last..$head" -- . ':!packages/acp-bridge' ':!tests'
echo; echo "== 提示：ACP 桥/测试改动逐条填入 docs/upstream-sync.md"
if [ "${1:-}" = "--bump" ]; then
  echo "$head $(date +%F)" > upstream.lock
  git -C upstream checkout --quiet "$head"
  echo "upstream.lock 已更新为 $head"
fi
