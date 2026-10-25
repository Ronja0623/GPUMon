"""GPU state classification: unknown, free, idle_occupied, or busy."""

from __future__ import annotations

import json
from collections.abc import Sequence

from .config import Config

UNKNOWN, FREE, IDLE, BUSY = "unknown", "free", "idle_occupied", "busy"
ORDER = {FREE: 0, IDLE: 1, BUSY: 2, UNKNOWN: 3}


def compute_processes(gpu: dict, ignore: set[str]) -> list[dict]:
    return [p for p in gpu["processes"] if p["type"] == "compute" and p["exe"] not in ignore]


def is_idle_window(rows: Sequence, latest_ts: float, window_s: float, interval_s: float, idle_util_pct: float) -> bool:
    """True when one compute process was present in every sample covering the
    whole window and the mean device utilization stayed below the limit.

    rows: (ts, util, compute_pids_json) for one GPU, ascending, within
    [latest_ts - window_s, latest_ts].
    """
    if not rows:
        return False
    if rows[0][0] > latest_ts - window_s + 1.5 * interval_s:
        return False  # not enough history to cover the window
    common: set | None = None
    utils = []
    prev = None
    for ts, util, pids_json in rows:
        if prev is not None and ts - prev > 3 * interval_s:
            return False  # a gap inside the window
        prev = ts
        pids = set(json.loads(pids_json))
        common = pids if common is None else common & pids
        if not common:
            return False
        if util is not None:
            utils.append(util)
    if not utils:
        return False
    return sum(utils) / len(utils) < idle_util_pct


def classify(gpu: dict | None, online: bool, window_rows: Sequence, latest_ts: float, interval_s: float,
             cfg: Config) -> str:
    """Exactly one state per GPU.

    unknown: no recent data. free: no compute process (ignoring
    ignore_processes) and used VRAM <= free_vram_mib. idle_occupied: a compute
    process present for the whole idle window with mean utilization below
    idle_util_pct. busy: every other case, including VRAM in use with no
    visible compute process.
    """
    if not online or gpu is None:
        return UNKNOWN
    g = cfg.gpu
    compute = compute_processes(gpu, set(g["ignore_processes"]))
    vram = gpu.get("vram_used_mib")
    if not compute:
        if vram is None:
            return UNKNOWN
        return FREE if vram <= g["free_vram_mib"] else BUSY
    if is_idle_window(window_rows, latest_ts, cfg.idle_window_s, interval_s, g["idle_util_pct"]):
        return IDLE
    return BUSY
