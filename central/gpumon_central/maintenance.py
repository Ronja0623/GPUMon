"""Periodic maintenance: retention, stale sessions, and rollups."""

from __future__ import annotations

from datetime import timedelta

from . import retention, rollups, sessions
from .config import Config
from .logutil import Log
from .storage import Storage
from .timeutil import local_date


def run(st: Storage, cfg: Config, now: float, log: Log | None = None, rollup_days: int = 2) -> dict:
    log = log or Log("gpumon.maintenance")
    deleted = retention.apply(
        st.dir, now, cfg.raw_retention_months, cfg.raw_max_total_bytes, log,
        record_event=lambda text: st.add_event(now, None, "raw_snapshot_deleted", text),
        close=st.close_month,
    )
    stale = sessions.close_stale(st.audit, now, cfg.session_gap_tolerance_s
                                 + cfg.offline_after_intervals * cfg.expected_interval_seconds)
    st.commit()
    today = local_date(now, cfg.tz)
    rows = 0
    for i in range(rollup_days):
        rows += rollups.compute_day(st, cfg, today - timedelta(days=i), now)
    log.info("maintenance finished", deleted_files=len(deleted), closed_stale_sessions=stale, rollup_rows=rows)
    return {"deleted": deleted, "stale_sessions": stale, "rollup_rows": rows}
