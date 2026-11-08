#!/usr/bin/env bash
# ZCode App/CLI 升级后的固定动作 (PORTING_PLAN §5.3):
#   1. 读 App 版本与 zcode --version
#   2. 用 record 子命令对真实 zcode 录制协议样本 → testdata/zcode-<ver>/
#   3. 提示做结构 diff、补 docs/zcode-protocol.md 章节、跑全部测试
#
# 前置: ZCODE_BIN 指向 App 内置 CLI (默认探测 /Applications/ZCode.app/...),
#       且模型可用 (ZCODE_ACP_MODEL 可选指定 providerId/modelId)。
set -euo pipefail
cd "$(dirname "$0")/.."

APP_PLIST=/Applications/ZCode.app/Contents/Info.plist
if [ -z "${ZCODE_BIN:-}" ] && [ -x /Applications/ZCode.app/Contents/Resources/glm/zcode.cjs ]; then
  export ZCODE_BIN=/Applications/ZCode.app/Contents/Resources/glm/zcode.cjs
fi

echo "== App 版本"
if [ -f "$APP_PLIST" ]; then
  defaults read "$APP_PLIST" CFBundleShortVersionString || true
else
  echo "(未找到 $APP_PLIST)"
fi
echo "== CLI 版本"
if [ -n "${ZCODE_BIN:-}" ]; then
  # 冷启动可能 20s+, 给足窗口
  ZCODE_APP_VERSION="${ZCODE_APP_VERSION:-}" "$ZCODE_BIN" --version || true
else
  echo "ZCODE_BIN 未设置且未找到默认 App CLI"
  exit 1
fi

echo "== 桥版本"
# 录的是「当前这份代码」对「当前这版 zcode」的协议样本, 所以两边版本都得在场:
# 样本进了 testdata/zcode-<ver>/ 之后, 要能回头说清它是哪份桥录的。
go run ./cmd/zcode-acp-bridge version

echo "== 录制协议样本"
go run ./cmd/zcode-acp-bridge record ${ZCODE_ACP_MODEL:+--model "$ZCODE_ACP_MODEL"}

cat <<'EOF'

== 后续动作 (PORTING_PLAN §5.3)
  1. 与上一版样本做结构 diff (字段增删、事件 type 集合变化)
  2. 在 docs/zcode-protocol.md 增加新版本章节草稿
  3. go test ./... —— 失败项即新的兼容规则待办
  4. doctor 全绿后把新版本写进 PORTING_PLAN §5.1 支持矩阵
EOF
