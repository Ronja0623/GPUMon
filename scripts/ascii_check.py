#!/usr/bin/env python3
"""Fail if any non-vendored source file, template, or doc contains non-ASCII
characters. Usage: ascii_check.py [ROOT]"""

from __future__ import annotations

import sys
from pathlib import Path

SKIP_DIRS = {".git", ".venv", "vendor", "node_modules", "__pycache__", ".pytest_cache", ".ruff_cache", "dist", "bin",
             "verification", "e2e-work"}
TEXT_SUFFIXES = {".go", ".mod", ".sum", ".py", ".js", ".css", ".html", ".md", ".yaml", ".yml", ".json", ".sh",
                 ".service", ".example", ".txt", ".toml", ".cfg", ".ini", ".tmpl"}
TEXT_NAMES = {"Makefile", "README", "LICENSE", ".gitignore"}


def candidates(root: Path):
    for p in sorted(root.rglob("*")):
        if any(part in SKIP_DIRS for part in p.relative_to(root).parts[:-1]) or not p.is_file():
            continue
        if p.suffix in TEXT_SUFFIXES or p.name in TEXT_NAMES:
            yield p


def check(root: Path) -> list[str]:
    problems = []
    for p in candidates(root):
        data = p.read_bytes()
        for lineno, line in enumerate(data.split(b"\n"), 1):
            for col, byte in enumerate(line, 1):
                if byte > 127:
                    problems.append(f"{p.relative_to(root)}:{lineno}:{col}: non-ASCII byte 0x{byte:02x}")
                    break
    return problems


def main() -> int:
    root = Path(sys.argv[1] if len(sys.argv) > 1 else Path(__file__).resolve().parents[1])
    problems = check(root)
    for line in problems:
        print(line)
    n = sum(1 for _ in candidates(root))
    if problems:
        print(f"ascii check FAILED: {len(problems)} file(s) contain non-ASCII characters")
        return 1
    print(f"ascii check passed: {n} files are ASCII-only")
    return 0


if __name__ == "__main__":
    sys.exit(main())
