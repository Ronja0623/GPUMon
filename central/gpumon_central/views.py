"""Live view: server cards, GPU states, owners, and the disk chart."""

from __future__ import annotations

import json

from . import gpustate, servers_file
from .config import Config
from .sessions import owners_on_gpu
from .storage import Storage
from .timeutil import iso_local, iso_utc, parse_iso, zone_label

SPECIAL_OWNERS = {"root-shared", "system", "unattributed"}


def is_special(owner: str) -> bool:
    return owner in SPECIAL_OWNERS or owner.startswith("uid:")


def server_status(row, cfg: Config, now: float) -> tuple[str, float]:
    interval = row["interval_s"] or cfg.expected_interval_seconds
    if row["last_seen"] is None:
        return "offline", interval
    online = now - row["last_seen"] <= cfg.offline_after_intervals * interval
    return ("online" if online else "offline"), interval


def _window_rows(st: Storage, sid: str, uuid: str, start: float, end: float) -> list:
    rows: list = []
    for mc in st.months_for(start, end):
        rows.extend(tuple(r) for r in mc.execute(
            "SELECT ts, util, compute_pids FROM gpu_samples WHERE server_id = ? AND gpu_uuid = ? AND ts >= ? AND ts <= ?"
            " ORDER BY ts", (sid, uuid, start, end)))
    rows.sort(key=lambda r: r[0])
    return rows


def _gpu_view(st: Storage, cfg: Config, sid: str, g: dict, online: bool, latest_ts: float, interval: float) -> dict:
    window = _window_rows(st, sid, g["uuid"], latest_ts - cfg.idle_window_s, latest_ts) if online else []
    state = gpustate.classify(g, online, window, latest_ts, interval, cfg)
    owners = []
    for owner, o in sorted(owners_on_gpu(g, set(cfg.gpu["ignore_processes"])).items()):
        procs = [p for p in g["processes"] if p["owner"] == owner and p["exe"] not in cfg.gpu["ignore_processes"]]
        owners.append({
            "owner": owner,
            "special": is_special(owner),
            "vram_mib": o["vram"],
            "methods": sorted(o["methods"]),
            "containers": sorted({p["container_name"] for p in procs if p.get("container_name")}),
            "vms": sorted({p["vm_name"] for p in procs if p.get("vm_name")}),
            "exes": sorted({p["exe"] for p in procs}),
        })
    total, used = g.get("vram_total_mib"), g.get("vram_used_mib")
    return {
        "index": g["index"],
        "uuid": g["uuid"],
        "name": g.get("name"),
        "state": state,
        "utilization_pct": g.get("utilization_pct"),
        "vram_total_mib": total,
        "vram_used_mib": used,
        "vram_free_mib": (total - used) if total is not None and used is not None else None,
        "temperature_c": g.get("temperature_c"),
        "power_draw_w": g.get("power_draw_w"),
        "power_limit_w": g.get("power_limit_w"),
        "fan_pct": g.get("fan_pct"),
        "owners": owners,
        "processes": g["processes"],
    }


def disk_view(sample: dict | None, scan: dict | None, cfg: Config) -> list[dict]:
    """One stacked bar per monitored filesystem."""
    if not sample:
        return []
    tz = cfg.tz
    out = []
    for fs in sample["filesystems"]:
        owners: dict[str, int] = {}
        partial = False
        scanned = False
        if scan:
            for p in scan["paths"]:
                if p["mount"] != fs["mount"]:
                    continue
                scanned = True
                partial = partial or p["partial"] or scan["partial"]
                for o in p["owners"]:
                    owners[o["owner"]] = owners.get(o["owner"], 0) + o["bytes"]
        ranked = sorted(owners.items(), key=lambda kv: (-kv[1], kv[0]))
        top = ranked[: cfg.disk_top_owners]
        others = sum(b for _, b in ranked[cfg.disk_top_owners:])
        scanned_total = sum(owners.values())
        segments = [{"kind": "owner", "label": o, "bytes": b, "special": is_special(o)} for o, b in top]
        if others:
            segments.append({"kind": "others", "label": "others", "bytes": others})
        segments.append({"kind": "unscanned", "label": "unscanned/other usage",
                         "bytes": max(0, fs["used_bytes"] - scanned_total)})
        segments.append({"kind": "free", "label": "free", "bytes": fs["free_bytes"]})
        completed = parse_iso(scan["completed_at"]) if scan and scanned else None
        out.append({
            "mount": fs["mount"],
            "size_bytes": fs["size_bytes"],
            "used_bytes": fs["used_bytes"],
            "free_bytes": fs["free_bytes"],
            "scanned_bytes": scanned_total,
            "segments": segments,
            "as_of": iso_utc(completed),
            "as_of_local": iso_local(completed, tz),
            "partial": partial,
        })
    return out


def build_status(st: Storage, cfg: Config, overrides: dict, now: float) -> dict:
    tz = cfg.tz
    servers = []
    summary = []
    for row in st.servers():
        meta = servers_file.apply(row["server_id"], overrides)
        if meta["retired"]:
            continue
        status, interval = server_status(row, cfg, now)
        online = status == "online"
        sample = json.loads(row["latest_sample"]) if row["latest_sample"] else None
        inventory = json.loads(row["latest_inventory"]) if row["latest_inventory"] else None
        scan = json.loads(row["latest_disk_scan"]) if row["latest_disk_scan"] else None
        skew = row["last_skew_s"]
        gpus = []
        if sample:
            gpus = [_gpu_view(st, cfg, row["server_id"], g, online, row["last_seen"], interval) for g in sample["gpus"]]
        elif inventory:
            gpus = [{"index": g["index"], "uuid": g["uuid"], "name": g["name"], "state": gpustate.UNKNOWN,
                     "utilization_pct": None, "vram_total_mib": g["vram_total_mib"], "vram_used_mib": None,
                     "vram_free_mib": None, "owners": [], "processes": []} for g in inventory["gpus"]]
        users = []
        if sample:
            users = [{**u, "special": is_special(u["owner"])} for u in sample["users"]]
            users.sort(key=lambda u: (-u["cpu_cores"], -u["rss_bytes"], u["owner"]))
        servers.append({
            "server_id": row["server_id"],
            **meta,
            "hostname": row["hostname"],
            "status": status,
            "interval_s": interval,
            "last_seen": iso_utc(row["last_seen"]),
            "last_seen_local": iso_local(row["last_seen"], tz),
            "clock_skew_s": round(skew, 1) if skew is not None else None,
            "clock_skew_warning": skew is not None and abs(skew) > cfg.clock_skew_warn_seconds,
            "host": sample["host"] if sample else None,
            "inventory": inventory,
            "errors": sample["errors"] if sample else [],
            "gpus": gpus,
            "users": users,
            "disk": disk_view(sample, scan, cfg),
        })
        for g in gpus:
            summary.append({
                "server_id": row["server_id"],
                "display_name": meta["display_name"],
                "gpu_index": g["index"],
                "gpu_uuid": g["uuid"],
                "gpu_name": g["name"],
                "state": g["state"],
                "vram_total_mib": g["vram_total_mib"],
                "vram_used_mib": g["vram_used_mib"],
                "vram_free_mib": g["vram_free_mib"],
                "utilization_pct": g["utilization_pct"],
                "owners": [o["owner"] for o in g["owners"]],
            })
    servers.sort(key=lambda s: (s["sort_order"], s["display_name"]))
    summary.sort(key=lambda g: (gpustate.ORDER[g["state"]], -(g["vram_free_mib"] or 0), g["display_name"], g["gpu_index"]))
    return {
        "generated_at": iso_utc(now),
        "generated_at_local": iso_local(now, tz),
        "timezone": cfg.timezone,
        "timezone_label": zone_label(tz, now),
        "refresh_seconds": cfg.refresh_seconds,
        "public_history_hours": cfg.public_history_hours,
        "summary": summary,
        "servers": servers,
    }
