#!/usr/bin/env python3
"""Summarize "go test -json" output. Exits 1 when a test or package failed."""

import json
import sys


def main() -> int:
    passed, failed, skipped, out = [], [], [], {}
    pkg_fail = []
    for line in open(sys.argv[1], encoding="utf-8"):
        line = line.strip()
        if not line.startswith("{"):
            continue
        ev = json.loads(line)
        test, action = ev.get("Test"), ev.get("Action")
        if test is None:
            if action == "fail":
                pkg_fail.append(ev.get("Package"))
            continue
        key = f"{ev['Package']}.{test}"
        if action == "output":
            out.setdefault(key, []).append(ev.get("Output", ""))
        elif action == "pass":
            passed.append(key)
        elif action == "fail":
            failed.append(key)
        elif action == "skip":
            skipped.append(key)
    for k in failed:
        print(f"FAIL {k}\n{''.join(out.get(k, []))}")
    print(f"go test: {len(passed)} passed, {len(failed)} failed, {len(skipped)} skipped")
    if pkg_fail and not failed:
        print(f"package failures: {pkg_fail}")
    return 1 if failed or pkg_fail else 0


if __name__ == "__main__":
    sys.exit(main())
