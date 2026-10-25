"""GPU sessions: the continuous presence of one owner on one GPU.

All session state lives in audit.db and is updated incrementally on each
sample, so a restart of central neither loses open sessions nor creates
duplicates (a partial unique index allows one open session per key).
"""

from __future__ import annotations

import json
import sqlite3

from .config import Config

MAX_NAMES = 10


def owners_on_gpu(gpu: dict, ignore: set[str]) -> dict[str, dict]:
    """Owner -> {vram, methods, names, util} for one GPU of a sample.
    Processes named in ignore_processes are not counted as presence."""
    owners: dict[str, dict] = {}
    for p in gpu["processes"]:
        if p["exe"] in ignore:
            continue
        o = owners.setdefault(p["owner"], {"vram": 0.0, "methods": set(), "names": [], "util": None})
        o["vram"] += p.get("vram_mib") or 0
        o["methods"].add(p["attribution"])
        if p.get("utilization_pct") is not None:
            o["util"] = (o["util"] or 0) + p["utilization_pct"]
        for n in (p.get("container_name"), p.get("vm_name"), p.get("exe")):
            if n and n not in o["names"]:
                o["names"].append(n)
    return owners


def _close(db: sqlite3.Connection, session_id: int, gap: bool) -> None:
    db.execute("UPDATE gpu_sessions SET open = 0, end_ts = last_seen_ts, ended_by_data_gap = ? WHERE id = ?",
               (1 if gap else 0, session_id))


def close_all(db: sqlite3.Connection, server_id: str, gap: bool) -> int:
    cur = db.execute("UPDATE gpu_sessions SET open = 0, end_ts = last_seen_ts, ended_by_data_gap = ? "
                     "WHERE server_id = ? AND open = 1", (1 if gap else 0, server_id))
    return cur.rowcount


def close_stale(db: sqlite3.Connection, now: float, max_age_s: float) -> int:
    """Close open sessions of servers that stopped reporting (a data gap)."""
    cur = db.execute("UPDATE gpu_sessions SET open = 0, end_ts = last_seen_ts, ended_by_data_gap = 1 "
                     "WHERE open = 1 AND ? - last_seen_ts > ?", (now, max_age_s))
    return cur.rowcount


def update(db: sqlite3.Connection, cfg: Config, server_id: str, ts: float, sample: dict) -> None:
    # Presence in consecutive samples always continues a session, even when the
    # sampling interval is longer than the configured tolerance.
    tol = max(cfg.session_gap_tolerance_s, 1.5 * float(sample["interval_seconds"]))
    ignore = set(cfg.gpu["ignore_processes"])
    present: set[tuple[str, str]] = set()
    for g in sample["gpus"]:
        owners = owners_on_gpu(g, ignore)
        shared = len(owners) > 1
        for owner, info in owners.items():
            present.add((g["uuid"], owner))
            _touch(db, server_id, g, owner, info, shared, ts, tol)
    for row in db.execute("SELECT id, gpu_uuid, owner, last_seen_ts FROM gpu_sessions WHERE server_id = ? AND open = 1",
                          (server_id,)).fetchall():
        if (row["gpu_uuid"], row["owner"]) not in present and ts - row["last_seen_ts"] > tol:
            _close(db, row["id"], gap=False)


def _touch(db: sqlite3.Connection, server_id: str, g: dict, owner: str, info: dict, shared: bool,
           ts: float, tol: float) -> None:
    row = db.execute("SELECT * FROM gpu_sessions WHERE server_id = ? AND gpu_uuid = ? AND owner = ? AND open = 1",
                     (server_id, g["uuid"], owner)).fetchone()
    if row is not None and ts <= row["last_seen_ts"]:
        return  # duplicate or out-of-order sample
    if row is not None and ts - row["last_seen_ts"] > tol:
        _close(db, row["id"], gap=False)
        row = None
    util = g.get("utilization_pct")
    vram = info["vram"]
    if row is None:
        db.execute(
            "INSERT INTO gpu_sessions (server_id, gpu_uuid, gpu_index, gpu_name, owner, start_ts, end_ts, last_seen_ts,"
            " sample_count, vram_peak_mib, vram_sum_mib, vram_n, util_sum, util_n, shared, methods, names, open,"
            " ended_by_data_gap) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, 1, ?, ?, ?, ?, ?, 1, 0)",
            (server_id, g["uuid"], g["index"], g.get("name"), owner, ts, ts, ts, vram, vram,
             util or 0, 1 if util is not None else 0, 1 if shared else 0,
             json.dumps(sorted(info["methods"])), json.dumps(info["names"][:MAX_NAMES])),
        )
        return
    methods = sorted(set(json.loads(row["methods"])) | info["methods"])
    names = json.loads(row["names"])
    for n in info["names"]:
        if n not in names and len(names) < MAX_NAMES:
            names.append(n)
    db.execute(
        "UPDATE gpu_sessions SET end_ts = ?, last_seen_ts = ?, sample_count = sample_count + 1,"
        " vram_peak_mib = MAX(vram_peak_mib, ?), vram_sum_mib = vram_sum_mib + ?, vram_n = vram_n + 1,"
        " util_sum = util_sum + ?, util_n = util_n + ?, shared = MAX(shared, ?), methods = ?, names = ?,"
        " gpu_index = ?, gpu_name = COALESCE(?, gpu_name) WHERE id = ?",
        (ts, ts, vram, vram, util or 0, 1 if util is not None else 0, 1 if shared else 0,
         json.dumps(methods), json.dumps(names), g["index"], g.get("name"), row["id"]),
    )


def to_dict(row: sqlite3.Row) -> dict:
    return {
        "id": row["id"],
        "server_id": row["server_id"],
        "gpu_uuid": row["gpu_uuid"],
        "gpu_index": row["gpu_index"],
        "gpu_name": row["gpu_name"],
        "owner": row["owner"],
        "start_ts": row["start_ts"],
        "end_ts": row["end_ts"],
        "duration_s": round(row["end_ts"] - row["start_ts"], 3),
        "open": bool(row["open"]),
        "vram_peak_mib": row["vram_peak_mib"],
        "vram_avg_mib": round(row["vram_sum_mib"] / row["vram_n"], 1) if row["vram_n"] else None,
        "util_avg_pct": round(row["util_sum"] / row["util_n"], 1) if row["util_n"] else None,
        "shared": bool(row["shared"]),
        "ended_by_data_gap": bool(row["ended_by_data_gap"]),
        "methods": json.loads(row["methods"]),
        "names": json.loads(row["names"]),
        "sample_count": row["sample_count"],
    }
