"""Data gaps: periods without samples, each with an English description."""

from __future__ import annotations

from .storage import Storage
from .timeutil import iso_utc

AGENT_OFFLINE = "agent_offline_or_unreachable"
CENTRAL_OFFLINE = "central_offline"

REASON_TEXT = {
    AGENT_OFFLINE: "agent offline or unreachable",
    CENTRAL_OFFLINE: "central offline",
}


def describe(server_id: str, start: float, end: float, reason: str) -> str:
    return (f"DATA GAP: no samples from {server_id} between {iso_utc(start, True)} and "
            f"{iso_utc(end, True)} ({REASON_TEXT[reason]}).")


def add_gap(st: Storage, server_id: str, start: float, end: float, reason: str) -> str:
    text = describe(server_id, start, end, reason)
    st.audit.execute(
        "INSERT INTO gaps (server_id, start_ts, end_ts, reason, description) VALUES (?, ?, ?, ?, ?)",
        (server_id, start, end, reason, text),
    )
    return text


def overlapping(st: Storage, server_id: str | None, start: float, end: float, reason: str | None = None) -> list:
    q = "SELECT * FROM gaps WHERE start_ts < ? AND end_ts > ?"
    args: list = [end, start]
    if server_id is not None:
        q += " AND server_id = ?"
        args.append(server_id)
    if reason is not None:
        q += " AND reason = ?"
        args.append(reason)
    return st.audit.execute(q + " ORDER BY start_ts", args).fetchall()


def running_segments(start: float, end: float, offline: list) -> list[tuple[float, float]]:
    """Split [start, end] into the parts during which central was running."""
    segs = []
    cur = start
    for g in sorted(offline, key=lambda r: r["start_ts"]):
        if g["start_ts"] > cur:
            segs.append((cur, min(g["start_ts"], end)))
        cur = max(cur, g["end_ts"])
    if cur < end:
        segs.append((cur, end))
    return segs


def record_central_offline(st: Storage, now: float) -> list[str]:
    """On startup, add a central_offline gap for every known server from the
    later of the last heartbeat and its last sample to now."""
    hb = st.heartbeat()
    if hb is None:
        return []
    out = []
    for s in st.servers():
        start = max(hb, s["last_seen"] or 0)
        if start < now:
            out.append(add_gap(st, s["server_id"], start, now, CENTRAL_OFFLINE))
    st.audit.commit()
    return out
