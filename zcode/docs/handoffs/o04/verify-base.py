#!/usr/bin/env python3
"""Read-only guard before applying the O04 patch."""
import hashlib
import json
from pathlib import Path
import sys

if len(sys.argv) != 2:
    raise SystemExit("Usage: python3 verify-base.py /path/to/zcode-orchestrator-go")
root = Path(sys.argv[1]).resolve()
manifest = json.loads((Path(__file__).parent / "base.json").read_text())
mismatches = []
for name, versions in manifest["files"].items():
    file = root / name
    actual = hashlib.sha256(file.read_bytes()).hexdigest() if file.is_file() else None
    if actual != versions["before"]:
        mismatches.append(name)
if mismatches:
    raise SystemExit("Do not apply: target differs from reviewed baseline:\n" + "\n".join(mismatches))
print(f"Baseline matches for {len(manifest['files'])} changed paths.")
