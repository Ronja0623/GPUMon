"""Retention of the monthly raw snapshot files.

Keep the files of the last raw_retention_months months (including the
current month) and delete older ones. If the total size is still above
raw_max_total_bytes, delete the oldest non-current files until it is not.
The current month's file is never deleted. Every deletion is logged and
stored as an event.
"""

from __future__ import annotations

import os
from collections.abc import Callable
from pathlib import Path

from .logutil import Log
from .storage import MONTH_FILE
from .timeutil import month_index, utc_month

SIDE_FILES = ("", "-wal", "-shm")


class OsFiles:
    """Real filesystem access (tests pass a fake with the same methods)."""

    def listdir(self, d: Path) -> list[str]:
        return os.listdir(d)

    def size(self, p: Path) -> int:
        total = 0
        for suffix in SIDE_FILES:
            try:
                total += os.path.getsize(f"{p}{suffix}")
            except OSError:
                pass
        return total

    def remove(self, p: Path) -> None:
        for suffix in SIDE_FILES:
            try:
                os.remove(f"{p}{suffix}")
            except FileNotFoundError:
                pass


def apply(data_dir: Path, now: float, months: int, max_bytes: int, log: Log,
          record_event: Callable[[str], None], close: Callable[[str], None] = lambda ym: None,
          files: OsFiles | None = None) -> list[str]:
    fs = files or OsFiles()
    current = utc_month(now)
    cur = month_index(current)
    found = sorted((m.group(1), name) for name in fs.listdir(data_dir) if (m := MONTH_FILE.match(name)))
    deleted: list[str] = []

    def delete(ym: str, name: str, size: int, reason: str) -> None:
        close(ym)
        fs.remove(Path(data_dir) / name)
        text = f"Deleted raw snapshot file {name} ({size} bytes): {reason}."
        log.warn("raw snapshot file deleted by retention", file=name, size_bytes=size, reason=reason)
        record_event(text)
        deleted.append(name)

    kept = []
    for ym, name in found:
        size = fs.size(Path(data_dir) / name)
        if cur - month_index(ym) >= months:
            delete(ym, name, size, f"older than the last {months} months")
        else:
            kept.append((ym, name, size))
    total = sum(s for _, _, s in kept)
    for ym, name, size in kept:
        if total <= max_bytes:
            break
        if ym >= current:
            continue  # never delete the current (or a future) month
        delete(ym, name, size, f"total raw snapshot size above the limit of {max_bytes} bytes")
        total -= size
    if total > max_bytes:
        log.error("the current month's raw snapshot file alone exceeds raw_max_total_bytes; it is kept",
                  current_month=current, total_bytes=total, limit_bytes=max_bytes)
    return deleted
