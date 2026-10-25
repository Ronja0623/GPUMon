"""CSV export of any time range. This is the only way to read history older
than the public window. Every file has a UTC ISO 8601 column and a local
time column in the configured time zone."""

from __future__ import annotations

import csv
import json
import os
from datetime import date, timedelta
from pathlib import Path

from . import rollups
from .config import EXPORT_KINDS, Config
from .storage import Storage
from .timeutil import iso_local, iso_utc, local_date, local_day_bounds

KINDS = EXPORT_KINDS

HEADERS = {
    "snapshots": ["time_utc", "time_local", "server_id", "record", "owner", "gpu_uuid", "attributions", "vram_mib",
                  "gpu_util_pct", "cpu_cores", "rss_bytes", "process_count", "cpu_pct", "mem_used_bytes",
                  "mem_total_bytes"],
    "sessions": ["start_utc", "start_local", "end_utc", "end_local", "session_id", "server_id", "gpu_uuid",
                 "gpu_index", "gpu_name", "owner", "duration_s", "open", "vram_peak_mib", "vram_avg_mib",
                 "util_avg_pct", "shared", "ended_by_data_gap", "methods", "names"],
    "rollups": ["day_start_utc", "day_start_local", "local_date", "server_id", "owner", "gpu_hours", "vram_gib_hours",
                "cpu_core_hours", "mem_gib_hours", "peak_rss_bytes"],
    "gaps": ["start_utc", "start_local", "end_utc", "end_local", "server_id", "reason", "description"],
    "disk": ["completed_utc", "completed_local", "server_id", "path", "mount", "owner", "bytes", "partial"],
}


def _rows(st: Storage, cfg: Config, kind: str, start: float, end: float, server: str | None, owner: str | None):
    tz = cfg.tz
    if kind == "snapshots":
        recs = []
        for mc in st.months_for(start, end):
            filt, args = "ts >= ? AND ts <= ?", [start, end]
            if server:
                filt += " AND server_id = ?"
                args.append(server)
            ofilt, oargs = (filt + " AND owner = ?", [*args, owner]) if owner else (filt, args)
            if not owner:
                for r in mc.execute(f"SELECT * FROM host_samples WHERE {filt}", args):
                    recs.append((r["ts"], r["server_id"], 0, ["host", "", "", "", "", "", "", "", "", r["cpu_pct"],
                                                               r["mem_used"], r["mem_total"]]))
            for r in mc.execute(f"SELECT * FROM gpu_owner_samples WHERE {ofilt}", oargs):
                recs.append((r["ts"], r["server_id"], 1, ["gpu", r["owner"], r["gpu_uuid"],
                                                          " ".join(json.loads(r["methods"])), r["vram_mib"],
                                                          r["util_pct"], "", "", "", "", "", ""]))
            for r in mc.execute(f"SELECT * FROM user_samples WHERE {ofilt}", oargs):
                recs.append((r["ts"], r["server_id"], 2, ["cpu_mem", r["owner"], "", " ".join(json.loads(r["methods"])),
                                                          "", "", r["cpu_cores"], r["rss_bytes"], r["nproc"], "", "",
                                                          ""]))
        recs.sort(key=lambda x: (x[0], x[1], x[2], x[3][1], x[3][2]))
        for ts, sid, _, rest in recs:
            yield [iso_utc(ts), iso_local(ts, tz), sid, *rest]
    elif kind == "sessions":
        q, args = "SELECT * FROM gpu_sessions WHERE start_ts <= ? AND end_ts >= ?", [end, start]
        if server:
            q, args = q + " AND server_id = ?", [*args, server]
        if owner:
            q, args = q + " AND owner = ?", [*args, owner]
        for r in st.audit.execute(q + " ORDER BY start_ts, id", args):
            yield [iso_utc(r["start_ts"]), iso_local(r["start_ts"], tz), iso_utc(r["end_ts"]),
                   iso_local(r["end_ts"], tz), r["id"], r["server_id"], r["gpu_uuid"], r["gpu_index"], r["gpu_name"],
                   r["owner"], round(r["end_ts"] - r["start_ts"], 3), int(r["open"]), r["vram_peak_mib"],
                   round(r["vram_sum_mib"] / r["vram_n"], 1) if r["vram_n"] else "",
                   round(r["util_sum"] / r["util_n"], 1) if r["util_n"] else "", int(r["shared"]),
                   int(r["ended_by_data_gap"]), " ".join(json.loads(r["methods"])), " ".join(json.loads(r["names"]))]
    elif kind == "rollups":
        first = local_date(start, tz).isoformat()
        last = local_date(end - 0.001, tz).isoformat()
        q, args = "SELECT * FROM daily_rollups WHERE local_date >= ? AND local_date <= ?", [first, last]
        if server:
            q, args = q + " AND server_id = ?", [*args, server]
        if owner:
            q, args = q + " AND owner = ?", [*args, owner]
        for r in st.audit.execute(q + " ORDER BY local_date, server_id, owner", args):
            day_start = local_day_bounds(date.fromisoformat(r["local_date"]), tz)[0]
            yield [iso_utc(day_start), iso_local(day_start, tz), r["local_date"], r["server_id"], r["owner"],
                   r["gpu_hours"], r["vram_gib_hours"], r["cpu_core_hours"], r["mem_gib_hours"], r["peak_rss_bytes"]]
    elif kind == "gaps":
        q, args = "SELECT * FROM gaps WHERE start_ts <= ? AND end_ts >= ?", [end, start]
        if server:
            q, args = q + " AND server_id = ?", [*args, server]
        for r in st.audit.execute(q + " ORDER BY start_ts, id", args):
            yield [iso_utc(r["start_ts"]), iso_local(r["start_ts"], tz), iso_utc(r["end_ts"]),
                   iso_local(r["end_ts"], tz), r["server_id"], r["reason"], r["description"]]
    elif kind == "disk":
        q, args = "SELECT * FROM disk_scans WHERE completed_ts >= ? AND completed_ts <= ?", [start, end]
        if server:
            q, args = q + " AND server_id = ?", [*args, server]
        for r in st.audit.execute(q + " ORDER BY completed_ts, id", args):
            for o in json.loads(r["owners"])["owners"]:
                if owner and o["owner"] != owner:
                    continue
                yield [iso_utc(r["completed_ts"]), iso_local(r["completed_ts"], tz), r["server_id"], r["path"],
                       r["mount"], o["owner"], o["bytes"], int(r["partial"] or r["path_partial"])]
    else:
        raise ValueError(f"unknown export kind {kind!r}; use one of {', '.join(KINDS)}")


def export(st: Storage, cfg: Config, kind: str, start: float, end: float, out_path: str,
           server: str | None = None, owner: str | None = None) -> int:
    """Write a CSV file. Returns the number of data rows."""
    if kind not in KINDS:
        raise ValueError(f"unknown export kind {kind!r}; use one of {', '.join(KINDS)}")
    if end < start:
        raise ValueError("--to must not be before --from")
    n = 0
    with open(out_path, "w", encoding="utf-8", newline="") as f:
        w = csv.writer(f)
        w.writerow(HEADERS[kind])
        for row in _rows(st, cfg, kind, start, end, server, owner):
            w.writerow(row)
            n += 1
    return n


def days_to_export(st: Storage, cfg: Config, now: float, catch_up_days: int) -> list[date]:
    """Local days that lie entirely before the public window, newest last.

    The newest is the day before the one containing now - public_history_hours;
    up to catch_up_days days are considered, and days that ended before the
    first server was ever seen are skipped.
    """
    tz = cfg.tz
    last = local_date(now - cfg.public_history_s, tz) - timedelta(days=1)
    row = st.audit.execute("SELECT MIN(first_seen) AS t FROM servers").fetchone()
    if row is None or row["t"] is None:
        return []
    days = [last - timedelta(days=i) for i in range(catch_up_days - 1, -1, -1)]
    return [d for d in days if local_day_bounds(d, tz)[1] > row["t"]]


def auto_export(st: Storage, cfg: Config, now: float, catch_up_days: int | None = None,
                day: date | None = None) -> list[Path]:
    """Write one CSV file per configured kind and local day to the export
    directory, named <kind>-YYYY-MM-DD.csv. Existing files are kept, so the
    command is safe to run repeatedly; with day set, that day is rewritten."""
    out_dir = Path(cfg.export_dir)
    out_dir.mkdir(mode=0o750, parents=True, exist_ok=True)
    kinds = cfg.auto_export["kinds"]
    if day is not None:
        days, force = [day], True
    else:
        days, force = days_to_export(st, cfg, now, catch_up_days or cfg.auto_export["catch_up_days"]), False
    written = []
    for d in days:
        paths = {k: out_dir / f"{k}-{d.isoformat()}.csv" for k in kinds}
        if not force and all(p.exists() for p in paths.values()):
            continue
        # Make sure the day's rollups exist; this is a no-op when its raw data is gone.
        rollups.compute_day(st, cfg, d, now)
        start, end = local_day_bounds(d, cfg.tz)
        for kind, path in paths.items():
            if path.exists() and not force:
                continue
            tmp = path.with_name(path.name + ".tmp")
            export(st, cfg, kind, start, end - 0.001, str(tmp))
            os.chmod(tmp, 0o640)
            tmp.replace(path)
            written.append(path)
    return written
