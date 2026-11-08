#!/usr/bin/env python3
"""生成真机冒烟用的 personal provider 配置。

背景
----
ZCode CLI **只**读两个 provider 配置来源:

  * ``ZCODE_PERSONAL_PROVIDER_CONFIG_FILE`` → ``~/.zcode/v2/provider_config.json``
    的 ``config.providerConfigRules.providerRules``
  * ``ZCODE_BUILTIN_PROVIDER_CONFIG_FILE`` → App 自带的 ``zcode-builtin.json``

它**不读** Electron App 的 ``~/.zcode/v2/config.json`` 里的 ``provider`` map。
所以「在 App 里登录过」不等于「CLI 的会话模型目录里���这个模型」—— App 的登录
把凭证写进 ``config.json``, 而 CLI 需要一条 personal rule 才认。

本脚本生成一份**独立的** personal provider 配置（默认写到仓库外的
``/tmp``), 追加一条指向国内 Coding Plan 端点的 personal rule。**不修改**
``~/.zcode/v2/provider_config.json``, 凭证从 ``config.json`` 读、只写进输出文件、
从不打印, 输出文件权限 0600。

用法
----
    python3 scripts/make-smoke-provider-config.py            # 写 /tmp/zcode-smoke-provider.json
    python3 scripts/make-smoke-provider-config.py --out ~/x.json

然后::

    export ZCODE_PERSONAL_PROVIDER_CONFIG_FILE=/tmp/zcode-smoke-provider.json
    export ZCODE_ACP_MODEL=<providerId>/<modelId>
    go test -tags=real ./test/smoke -run TestSmokeRealPermission -v

providerId 由脚本生成后打印, 同时写进输出文件 —— 它必须是合法 ``uuid4``:
手编的假 id（哪怕只差一个字符）会让整个 personal 模型目录塌成 0 个。
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import sys
import uuid

# App 里登录国内 Coding Plan 后, 凭证落在 config.json 的这个键下。
CONFIG_PATH = "~/.zcode/v2/config.json"
# 桥/CLI 默认的个人 provider 配置; 本脚本读它是为了保留用户已有的规则,
# 但**不会**写回这个文件。
PERSONAL_PATH = "~/.zcode/v2/provider_config.json"

# 国内 Coding Plan 的 anthropic 兼容端点。open.bigmodel.cn 上**没有** start-plan,
# start-plan / off-peak 一律走 zcode.z.ai —— 别把 start-plan 指到这里。
BASE_URL = "https://open.bigmodel.cn/api/anthropic"

# CLI 的个人 provider 规则必须落在 standard-personal 组。用别的 group
# (例如 bigmodel-family) 会让整份 personal 目录被丢弃, 同样是 0 个模型。
GROUP = "standard-personal"

PROVIDER_KEY = "builtin:bigmodel-coding-plan"
PROVIDER_NAME = "bigmodel-coding-plan"


def die(msg: str) -> "NoReturn":  # type: ignore[valid-type]
    print(f"错误: {msg}", file=sys.stderr)
    raise SystemExit(1)


def load_json(path: str, what: str) -> dict:
    p = pathlib.Path(path).expanduser()
    if not p.exists():
        die(f"{what} 不存在: {p}\n  先在 ZCode App 里登录对应套餐, 或换 --config 指定路径。")
    with p.open(encoding="utf-8") as fh:
        return json.load(fh)


def resolve_api_key(config: dict) -> str:
    provider = (config.get("provider") or {}).get(PROVIDER_KEY)
    if not isinstance(provider, dict):
        die(f"config.json 里没有 provider[{PROVIDER_KEY!r}] —— App 侧还没登录国内 Coding Plan。")
    if provider.get("enabled") is False:
        die(f"provider[{PROVIDER_KEY!r}] 是 enabled=false, 用不了。")
    key = (provider.get("options") or {}).get("apiKey") or ""
    if not key:
        die(f"provider[{PROVIDER_KEY!r}].options.apiKey 是空的 —— 凭证没落盘。")
    return key


def resolve_models(provider: dict) -> list[str]:
    """用户在 App 里没删掉的模型。deletedModels 里的不能再进 personalModelIds,
    否则后端会回 `Model "x" is not supported by any configured account in this group`。"""
    deleted = set((provider.get("zcode") or {}).get("deletedModels") or [])
    models = [m for m in (provider.get("models") or {}) if m not in deleted]
    if not models:
        die("该 provider 下没有可用模型（全部被 deletedModels 删掉了）。")
    return sorted(models)


def build_rule(api_key: str, models: list[str], provider_id: str | None) -> dict:
    return {
        # 复用已有 rule 的 id, 让重复运行保持稳定; 否则生成新的 uuid4。
        "providerId": provider_id or str(uuid.uuid4()),
        "providerName": PROVIDER_NAME,
        "config": {
            "group": GROUP,
            "access": {"type": "api-key", "apiKey": api_key},
            "api": {"type": "anthropic-messages", "baseUrl": BASE_URL},
            "personalModelIds": models,
            "modelOrder": models,
        },
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", default="/tmp/zcode-smoke-provider.json",
                    help="输出路径 (默认 /tmp/zcode-smoke-provider.json, 权限 0600)")
    ap.add_argument("--config", default=CONFIG_PATH,
                    help=f"ZCode App 的 config.json (默认 {CONFIG_PATH})")
    ap.add_argument("--personal", default=PERSONAL_PATH,
                    help=f"已有的 personal provider 配置, 读而不改 (默认 {PERSONAL_PATH})")
    args = ap.parse_args()

    config = load_json(args.config, "ZCode config.json")
    personal = load_json(args.personal, "personal provider 配置")

    app_provider = (config.get("provider") or {}).get(PROVIDER_KEY) or {}
    api_key = resolve_api_key(config)
    models = resolve_models(app_provider)

    rules = personal.setdefault("config", {}).setdefault("providerConfigRules", {}).setdefault("providerRules", [])
    existing = next((r for r in rules if r.get("providerName") == PROVIDER_NAME), None)
    rule = build_rule(api_key, models, (existing or {}).get("providerId"))
    if existing is not None:
        rules[rules.index(existing)] = rule
    else:
        rules.append(rule)

    out = pathlib.Path(args.out).expanduser()
    # 重复运行时保持 providerId 稳定: 上一次的输出才是真正持有这条 rule 的文件
    # (--personal 通常是用户自己的配置, 里面没有这条)。id 每次都变的话,
    # 照着打印结果导出的 ZCODE_ACP_MODEL 下次就失效了。
    if existing is None and out.exists():
        try:
            prev = json.loads(out.read_text(encoding="utf-8"))
            prev_rules = (prev.get("config", {}).get("providerConfigRules", {}) or {}).get("providerRules", [])
            prev_rule = next((r for r in prev_rules if r.get("providerName") == PROVIDER_NAME), None)
            if prev_rule and isinstance(prev_rule.get("providerId"), str):
                rule["providerId"] = prev_rule["providerId"]
        except (OSError, ValueError):
            pass  # 上次输出坏了就当没有, 重新生成 id
    # 先以 0600 建再写, 避免 apiKey 落盘前有一瞬间是 world-readable。
    fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        json.dump(personal, fh, ensure_ascii=False, indent=2)
        fh.write("\n")
    os.chmod(out, 0o600)

    # 只打印非敏感信息。apiKey 一个字符都不落到 stdout。
    print(f"✓ 写入 {out} (0600), 共 {len(rules)} 条 personal rule")
    print(f"  providerId = {rule['providerId']}")
    print(f"  group      = {GROUP}   baseUrl = {BASE_URL}")
    print(f"  models     = {', '.join(models)}")
    print()
    print("接着:")
    print(f"  export ZCODE_PERSONAL_PROVIDER_CONFIG_FILE={out}")
    print(f"  export ZCODE_ACP_MODEL={rule['providerId']}/{models[0]}")
    print("  go test -tags=real ./test/smoke -run TestSmokeRealPermission -v")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
