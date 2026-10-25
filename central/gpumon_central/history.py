"""History for the dashboard: per-GPU utilization and VRAM by owner, and
per-server CPU and memory by owner, averaged into time buckets, plus gaps."""

from __future__ import annotations

import json
import math

from . import gaps, servers_file
from .config import Config
from .storage import Storage
from .timeutil import iso_utc

BUCKETS = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200]
MAX_POINTS = 360


def choose_bucket(span_s: float) -> int:
    for b in BUCKETS:
        if span_s / b <= MAX_POINTS:
            return b
    return int(math.ceil(span_s / MAX_POINTS / 3600) * 3600)


def _avg(sums: dict, counts: list[int], n: int, digits: int) -> dict:
    out = {}
    for key, vals in sums.items():
        out[key] = [round(vals[i] / counts[i], digits) if counts[i] else None for i in range(n)]
    return out


def _ordered(series: dict) -> dict:
    """Largest total first, so stacked charts are stable."""
    return dict(sorted(series.items(), key=lambda kv: (-sum(v or 0 for v in kv[1]), kv[0])))


def build_history(st: Storage, cfg: Config, overrides: dict, start: float, end: float, now: float,
                  server: str | None = None) -> dict:
    b = choose_bucket(end - start)
    first = math.floor(start / b) * b
    n = max(1, int(math.ceil((end - first) / b)))
    servers_out = []
    gaps_out = []
    for row in st.servers():
        sid = row["server_id"]
        meta = servers_file.apply(sid, overrides)
        if meta["retired"] or (server and sid != server):
            continue
        counts = [0] * n
        cpu: dict[str, list[float]] = {}
        mem: dict[str, list[float]] = {}
        gdev: dict[str, dict] = {}
        gvram: dict[str, dict[str, list[float]]] = {}
        gutil: dict[str, dict[str, list[float]]] = {}
        args = (first, b, sid, start, end)
        for mc in st.months_for(start, end):
            for r in mc.execute("SELECT CAST((ts - ?) / ? AS INTEGER) bi, COUNT(*) c FROM host_samples"
                                " WHERE server_id = ? AND ts >= ? AND ts < ? GROUP BY bi", args):
                counts[r["bi"]] += r["c"]
            for r in mc.execute("SELECT CAST((ts - ?) / ? AS INTEGER) bi, owner, SUM(cpu_cores) c, SUM(rss_bytes) m"
                                " FROM user_samples WHERE server_id = ? AND ts >= ? AND ts < ? GROUP BY bi, owner", args):
                cpu.setdefault(r["owner"], [0.0] * n)[r["bi"]] += r["c"]
                mem.setdefault(r["owner"], [0.0] * n)[r["bi"]] += r["m"]
            for r in mc.execute("SELECT gpu_uuid, CAST((ts - ?) / ? AS INTEGER) bi, COUNT(*) c, SUM(util) u,"
                                " COUNT(util) un, MAX(vram_total) vt, MAX(gpu_index) gi FROM gpu_samples"
                                " WHERE server_id = ? AND ts >= ? AND ts < ? GROUP BY gpu_uuid, bi", args):
                d = gdev.setdefault(r["gpu_uuid"], {"count": [0] * n, "usum": [0.0] * n, "un": [0] * n,
                                                    "vram_total": None, "index": r["gi"]})
                d["count"][r["bi"]] += r["c"]
                d["usum"][r["bi"]] += r["u"] or 0
                d["un"][r["bi"]] += r["un"]
                d["vram_total"] = r["vt"] if r["vt"] is not None else d["vram_total"]
            for r in mc.execute("SELECT gpu_uuid, CAST((ts - ?) / ? AS INTEGER) bi, owner, SUM(vram_mib) v,"
                                " SUM(util_pct) u, COUNT(util_pct) un FROM gpu_owner_samples"
                                " WHERE server_id = ? AND ts >= ? AND ts < ? GROUP BY gpu_uuid, bi, owner", args):
                gvram.setdefault(r["gpu_uuid"], {}).setdefault(r["owner"], [0.0] * n)[r["bi"]] += r["v"]
                if r["un"]:
                    gutil.setdefault(r["gpu_uuid"], {}).setdefault(r["owner"], [0.0] * n)[r["bi"]] += r["u"]
        names = {}
        if row["latest_sample"]:
            for g in json.loads(row["latest_sample"])["gpus"]:
                names[g["uuid"]] = g.get("name")
        gpus = []
        for uuid, d in sorted(gdev.items(), key=lambda kv: (kv[1]["index"] is None, kv[1]["index"])):
            gpus.append({
                "uuid": uuid,
                "index": d["index"],
                "name": names.get(uuid),
                "vram_total_mib": d["vram_total"],
                # Device utilization only; it is never split across owners.
                "util": [round(d["usum"][i] / d["un"][i], 1) if d["un"][i] else None for i in range(n)],
                "vram_by_owner": _ordered(_avg(gvram.get(uuid, {}), d["count"], n, 1)),
                # Present only when NVML reported per-process utilization.
                "util_by_owner": _ordered(_avg(gutil.get(uuid, {}), d["count"], n, 1)),
            })
        cores = None
        if row["latest_sample"]:
            cores = json.loads(row["latest_sample"])["host"]["logical_cores"]
        servers_out.append({
            "server_id": sid,
            "display_name": meta["display_name"],
            "sort_order": meta["sort_order"],
            "logical_cores": cores,
            "samples": counts,
            "cpu_by_owner": _ordered(_avg(cpu, counts, n, 3)),
            "mem_by_owner": _ordered(_avg(mem, counts, n, 0)),
            "gpus": gpus,
        })
        for g in gaps.overlapping(st, sid, start, end):
            gaps_out.append({"server_id": sid, "start": g["start_ts"], "end": g["end_ts"], "reason": g["reason"],
                             "description": g["description"], "ongoing": False})
        interval = row["interval_s"] or cfg.expected_interval_seconds
        if row["last_seen"] is not None and now - row["last_seen"] > cfg.offline_after_intervals * interval:
            gaps_out.append({"server_id": sid, "start": max(row["last_seen"], start), "end": now,
                             "reason": gaps.AGENT_OFFLINE, "ongoing": True,
                             "description": f"DATA GAP: no samples from {sid} since {iso_utc(row['last_seen'], True)}"
                                            f" ({gaps.REASON_TEXT[gaps.AGENT_OFFLINE]}, ongoing)."})
    servers_out.sort(key=lambda s: (s["sort_order"], s["display_name"]))
    return {
        "from": start,
        "to": end,
        "from_utc": iso_utc(start),
        "to_utc": iso_utc(end),
        "bucket_seconds": b,
        "timestamps": [first + i * b for i in range(n)],
        "servers": servers_out,
        "gaps": gaps_out,
    }
