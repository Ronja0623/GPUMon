"""Time helpers. All stored times are UTC epoch seconds."""

from __future__ import annotations

import time
from datetime import date, datetime, timedelta, timezone
from zoneinfo import ZoneInfo


def parse_iso(s: str) -> float:
    """Parse an RFC 3339 timestamp ("...Z" or with an offset) to epoch seconds."""
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    dt = datetime.fromisoformat(s)
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.timestamp()


def parse_user_time(s: str, tz: ZoneInfo) -> float:
    """Parse a CLI or API time. Accepts epoch seconds, a date (YYYY-MM-DD,
    local midnight), or ISO 8601; naive values are in the configured zone."""
    s = s.strip()
    try:
        return float(s)
    except ValueError:
        pass
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    try:
        dt = datetime.fromisoformat(s)
    except ValueError as e:
        raise ValueError(f"invalid time {s!r}: use ISO 8601 such as 2026-10-25T14:00:00Z or 2026-10-25") from e
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=tz)
    return dt.timestamp()


def iso_utc(ts: float | None, seconds_only: bool = False) -> str | None:
    if ts is None:
        return None
    dt = datetime.fromtimestamp(ts, tz=timezone.utc)
    if seconds_only:
        return dt.strftime("%Y-%m-%dT%H:%M:%SZ")
    return dt.strftime("%Y-%m-%dT%H:%M:%S.") + f"{dt.microsecond // 1000:03d}Z"


def iso_local(ts: float | None, tz: ZoneInfo) -> str | None:
    if ts is None:
        return None
    return datetime.fromtimestamp(ts, tz=tz).isoformat(timespec="seconds")


def zone_label(tz: ZoneInfo, ts: float | None = None) -> str:
    """For example "Asia/Taipei (UTC+08:00)"."""
    dt = datetime.fromtimestamp(ts if ts is not None else time.time(), tz=tz)
    off = dt.utcoffset() or timedelta(0)
    minutes = int(off.total_seconds() // 60)
    sign = "+" if minutes >= 0 else "-"
    minutes = abs(minutes)
    return f"{tz.key} (UTC{sign}{minutes // 60:02d}:{minutes % 60:02d})"


def local_day_bounds(d: date, tz: ZoneInfo) -> tuple[float, float]:
    start = datetime(d.year, d.month, d.day, tzinfo=tz)
    nxt = d + timedelta(days=1)
    end = datetime(nxt.year, nxt.month, nxt.day, tzinfo=tz)
    return start.timestamp(), end.timestamp()


def local_date(ts: float, tz: ZoneInfo) -> date:
    return datetime.fromtimestamp(ts, tz=tz).date()


def utc_month(ts: float) -> str:
    return datetime.fromtimestamp(ts, tz=timezone.utc).strftime("%Y-%m")


def months_between(start_ts: float, end_ts: float) -> list[str]:
    """UTC months ("YYYY-MM") overlapping [start_ts, end_ts]."""
    out = []
    d = datetime.fromtimestamp(start_ts, tz=timezone.utc).replace(day=1, hour=0, minute=0, second=0, microsecond=0)
    end = datetime.fromtimestamp(end_ts, tz=timezone.utc)
    while d <= end:
        out.append(d.strftime("%Y-%m"))
        d = (d + timedelta(days=32)).replace(day=1)
    return out


def month_index(ym: str) -> int:
    y, m = ym.split("-")
    return int(y) * 12 + int(m) - 1
