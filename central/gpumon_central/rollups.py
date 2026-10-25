"""Daily rollups per (local date, server, owner). Recomputing a day replaces
its rows, so the computation is idempotent. A day without raw samples is
left untouched, so rollups survive the deletion of old raw snapshot files."""

from __future__ import annotations

from datetime import date

from .config import Config
from .storage import Storage
from .timeutil import local_day_bounds

GIB = 1024.0 ** 3


def compute_day(st: Storage, cfg: Config, day: date, now: float) -> int:
    start, end = local_day_bounds(day, cfg.tz)
    acc: dict[tuple[str, str], dict] = {}
    servers_with_data: set[str] = set()

    def row(sid: str, owner: str) -> dict:
        return acc.setdefault((sid, owner), {"gpu_s": 0.0, "vram_mib_s": 0.0, "cpu_s": 0.0, "rss_s": 0.0, "peak": 0})

    for mc in st.months_for(start, end - 0.001):
        for r in mc.execute("SELECT DISTINCT server_id FROM host_samples WHERE ts >= ? AND ts < ?", (start, end)):
            servers_with_data.add(r["server_id"])
        for r in mc.execute(
            "SELECT u.server_id sid, u.owner owner, SUM(u.cpu_cores * h.interval_s) cpu_s,"
            " SUM(u.rss_bytes * h.interval_s) rss_s, MAX(u.rss_bytes) peak FROM user_samples u"
            " JOIN host_samples h ON h.server_id = u.server_id AND h.ts = u.ts"
            " WHERE u.ts >= ? AND u.ts < ? GROUP BY u.server_id, u.owner", (start, end)):
            a = row(r["sid"], r["owner"])
            a["cpu_s"] += r["cpu_s"] or 0
            a["rss_s"] += r["rss_s"] or 0
            a["peak"] = max(a["peak"], r["peak"] or 0)
        for r in mc.execute(
            "SELECT g.server_id sid, g.owner owner, SUM(h.interval_s) gpu_s, SUM(g.vram_mib * h.interval_s) vram_s"
            " FROM gpu_owner_samples g JOIN host_samples h ON h.server_id = g.server_id AND h.ts = g.ts"
            " WHERE g.ts >= ? AND g.ts < ? GROUP BY g.server_id, g.owner", (start, end)):
            a = row(r["sid"], r["owner"])
            a["gpu_s"] += r["gpu_s"] or 0
            a["vram_mib_s"] += r["vram_s"] or 0
    if not servers_with_data:
        return 0
    key = day.isoformat()
    db = st.audit
    with db:
        for sid in servers_with_data:
            db.execute("DELETE FROM daily_rollups WHERE local_date = ? AND server_id = ?", (key, sid))
        for (sid, owner), a in acc.items():
            db.execute(
                "INSERT INTO daily_rollups (local_date, server_id, owner, gpu_hours, vram_gib_hours, cpu_core_hours,"
                " mem_gib_hours, peak_rss_bytes, computed_ts) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (key, sid, owner, round(a["gpu_s"] / 3600, 6), round(a["vram_mib_s"] / 1024 / 3600, 6),
                 round(a["cpu_s"] / 3600, 6), round(a["rss_s"] / GIB / 3600, 6), int(a["peak"]), now))
    return len(acc)
