#!/usr/bin/env bash
# 终端里演示 AskUserQuestion (ACP elicitation) 的可运行例子。
#
# 用法:
#   scripts/demo-ask.sh            # 多选 (推荐先看这个)
#   scripts/demo-ask.sh single     # 单选
#   scripts/demo-ask.sh multi      # 多选
#   scripts/demo-ask.sh two        # 同一 turn 连问两次
#   PROMPT="你的问题" scripts/demo-ask.sh   # 覆盖提示词
#
# 人在终端里跑时: 看到 🔷 人工应答 表格, 输入选项编号回车即可。
#   - 单选: 输编号或直接输 value; 直接回车 = 作废
#   - 多选: 1,3 / 1 3 / 1、3; 留空 = 都不选
# 选完会打印 zcode 实际收到的应答, 并断言「你选的」与「zcode 收到的」一致。
#
# 不在真终端里跑 (管道 / CI / 后台任务) 时: 自动用 script(1) 分配一个 pty,
# 于是提问照样打出来 —— 但**没有人在输入**, 所以需要你预先把选择喂进来:
#
#   printf '1,2\n' | scripts/demo-ask.sh multi
#
# 前提: 项目根的 env 已按 env.example 配好 (ZCODE_BIN / provider / model)。
set -euo pipefail

cd "$(dirname "$0")/.."

# 提示词优先级: 已有的 ZCODE_SMOKE_ASK_PROMPT > 本次传入的 PROMPT > 按模式给的默认。
# (别把已设好的变量覆盖掉 —— 早先就因为这里写成 "${PROMPT:-默认}" 而让
#  外面 export 的 ZCODE_SMOKE_ASK_PROMPT 被静默吞掉, A/B 对照跑的根本不是
#  同一个 prompt。)
default_prompt() {
  case "$1" in
    single)
      echo "不要直接回答。请先用 AskUserQuestion 工具向我提一个**单选**问题（2 个选项），问我希望听简短结论还是详细解释。等我作答后，用一句话说明我选了什么，不要再重复提问。" ;;
    multi)
      echo "不要直接回答。请先用 AskUserQuestion 工具向我提一个**多选题**（multiSelect=true，4 个候选选项），问我想让回答偏哪种风格。等我作答后，用一句话说明我最终勾了哪些，不要再重复提问。" ;;
    two)
      echo "请按顺序做两件事：第一，用 AskUserQuestion 工具提一个单选问题（2 个选项）并等我作答；拿到答案后，再用 AskUserQuestion 工具提第二个单选问题（2 个选项）并等我作答。最后用一句话把两个答案都复述一遍，不要再重复提问。" ;;
    *)
      echo "用法: $0 [single|multi|two]" >&2
      return 2 ;;
  esac
}

mode="${1:-multi}"
ZCODE_SMOKE_ASK_PROMPT="${ZCODE_SMOKE_ASK_PROMPT:-${PROMPT:-$(default_prompt "$mode")}}"
export ZCODE_SMOKE_ASK_PROMPT

# 开场先报版本。这个项目里「正在被测的是哪份代码」不是显然的: smoke 用例默认
# 从工作树现编, 而日常入口 (ZCODE_ACP_BRIDGE_BIN) 用的是已安装那份 —— 两者常常
# 不是同一份代码。所以先说清版本, 后面的行为才有归属。
#
# 优先级与 test/smoke 的 bridgeBinary 一致 (env 指定的先用, 否则现编), 免得这里
# 报的版本和实际被测的对不上 —— 那比不报更糟。
print_version() {
  local bin="${ZCODE_ACP_BRIDGE_BIN:-}"
  if [ -z "$bin" ]; then
    bin="$(mktemp -d)/zcode-acp-bridge"
    # 构建失败不中断: 版本只是开场信息, 下面的测试自己还会构建一次, 到时会报出
    # 真正的错误。这里把构建输出透出来, 免得版本行缺席得毫无线索。
    if ! go build -o "$bin" ./cmd/zcode-acp-bridge >&2; then
      echo "⚠ 构建失败, 无法读取版本 (下面的测试会再报一次)" >&2
      return 0
    fi
  fi
  echo "── zcode-acp-bridge 版本 ─────────────────────────"
  "$bin" version 2>&1 | sed 's/^/  /'
  echo "  路径: $bin"
  echo
}

print_version

run() {
  ZCODE_SMOKE_INTERACTIVE=1 go test -tags=real -count=1 -timeout 12m \
    ./test/smoke -run '^TestSmokeRealAsk$' -v
}

# 真终端: 直接跑。没有终端: 借 pty, 否则 /dev/tty 打不开、提问无处可显示。
if [ -t 0 ] && [ -t 1 ]; then
  run
else
  echo "(没有控制终端, 用 script(1) 分配 pty; 交互请勿在此模式下使用)" >&2
  script -q /dev/null bash -c 'run'
fi
