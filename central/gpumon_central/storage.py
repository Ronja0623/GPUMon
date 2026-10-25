"""SQLite storage.

audit.db is permanent: servers, inventory history, GPU sessions, daily
rollups, disk scans, gaps, events, and the heartbeat. It is never pruned.

Raw snapshots live in monthly files snapshots-YYYY-MM.db (UTC month of the
receive time), with WAL mode. Each file holds the compressed raw messages and
small normalized time-series tables used by the dashboard history, the GPU
state window, and the rollups.
"""

from __future__ import annotations

import json
import os
import re
import sqlite3
import zlib
from collections.abc import Iterator
from pathlib import Path

from . import timeutil

AUDIT_SCHEMA = """
CREATE TABLE IF NOT EXISTS heartbeat (id INTEGER PRIMARY KEY CHECK (id = 1), ts REAL NOT NULL);
CREATE TABLE IF NOT EXISTS servers (
    server_id TEXT PRIMARY KEY,
    first_seen REAL NOT NULL,
    last_seen REAL,
    last_agent_ts REAL,
    last_run_id TEXT,
    last_seq INTEGER,
    interval_s REAL,
    last_skew_s REAL,
    hostname TEXT,
    latest_sample TEXT,
    latest_inventory TEXT,
    latest_disk_scan TEXT,
    disk_scan_received REAL
);
CREATE TABLE IF NOT EXISTS inventory_history (
    id INTEGER PRIMARY KEY,
    server_id TEXT NOT NULL,
    received_ts REAL NOT NULL,
    agent_ts REAL NOT NULL,
    hash TEXT NOT NULL,
    inventory TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS gpu_sessions (
    id INTEGER PRIMARY KEY,
    server_id TEXT NOT NULL,
    gpu_uuid TEXT NOT NULL,
    gpu_index INTEGER,
    gpu_name TEXT,
    owner TEXT NOT NULL,
    start_ts REAL NOT NULL,
    end_ts REAL NOT NULL,
    last_seen_ts REAL NOT NULL,
    sample_count INTEGER NOT NULL,
    vram_peak_mib REAL NOT NULL DEFAULT 0,
    vram_sum_mib REAL NOT NULL DEFAULT 0,
    vram_n INTEGER NOT NULL DEFAULT 0,
    util_sum REAL NOT NULL DEFAULT 0,
    util_n INTEGER NOT NULL DEFAULT 0,
    shared INTEGER NOT NULL DEFAULT 0,
    methods TEXT NOT NULL,
    names TEXT NOT NULL,
    open INTEGER NOT NULL,
    ended_by_data_gap INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS gpu_sessions_one_open ON gpu_sessions (server_id, gpu_uuid, owner) WHERE open = 1;
CREATE INDEX IF NOT EXISTS gpu_sessions_time ON gpu_sessions (start_ts, end_ts);
CREATE TABLE IF NOT EXISTS daily_rollups (
    local_date TEXT NOT NULL,
    server_id TEXT NOT NULL,
    owner TEXT NOT NULL,
    gpu_hours REAL NOT NULL,
    vram_gib_hours REAL NOT NULL,
    cpu_core_hours REAL NOT NULL,
    mem_gib_hours REAL NOT NULL,
    peak_rss_bytes INTEGER NOT NULL,
    computed_ts REAL NOT NULL,
    PRIMARY KEY (local_date, server_id, owner)
);
CREATE TABLE IF NOT EXISTS disk_scans (
    id INTEGER PRIMARY KEY,
    server_id TEXT NOT NULL,
    received_ts REAL NOT NULL,
    started_ts REAL NOT NULL,
    completed_ts REAL NOT NULL,
    partial INTEGER NOT NULL,
    path TEXT NOT NULL,
    mount TEXT NOT NULL,
    total_bytes INTEGER NOT NULL,
    path_partial INTEGER NOT NULL,
    owners TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS gaps (
    id INTEGER PRIMARY KEY,
    server_id TEXT NOT NULL,
    start_ts REAL NOT NULL,
    end_ts REAL NOT NULL,
    reason TEXT NOT NULL,
    description TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS gaps_time ON gaps (server_id, start_ts, end_ts);
CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY,
    ts REAL NOT NULL,
    server_id TEXT,
    kind TEXT NOT NULL,
    detail TEXT NOT NULL
);
"""

MONTH_SCHEMA = """
CREATE TABLE IF NOT EXISTS snapshots (
    id INTEGER PRIMARY KEY,
    server_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    run_id TEXT NOT NULL,
    seq INTEGER,
    agent_ts REAL NOT NULL,
    received_ts REAL NOT NULL,
    payload BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS snapshots_time ON snapshots (server_id, received_ts);
CREATE TABLE IF NOT EXISTS host_samples (
    server_id TEXT NOT NULL,
    ts REAL NOT NULL,
    interval_s REAL NOT NULL,
    cpu_pct REAL,
    logical_cores INTEGER,
    load1 REAL,
    mem_used INTEGER,
    mem_total INTEGER,
    swap_used INTEGER,
    swap_total INTEGER
);
CREATE INDEX IF NOT EXISTS host_samples_time ON host_samples (server_id, ts);
CREATE TABLE IF NOT EXISTS gpu_samples (
    server_id TEXT NOT NULL,
    ts REAL NOT NULL,
    gpu_uuid TEXT NOT NULL,
    gpu_index INTEGER,
    util REAL,
    vram_used REAL,
    vram_total REAL,
    compute_pids TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS gpu_samples_time ON gpu_samples (server_id, gpu_uuid, ts);
CREATE TABLE IF NOT EXISTS gpu_owner_samples (
    server_id TEXT NOT NULL,
    ts REAL NOT NULL,
    gpu_uuid TEXT NOT NULL,
    owner TEXT NOT NULL,
    vram_mib REAL NOT NULL,
    util_pct REAL,
    methods TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS gpu_owner_samples_time ON gpu_owner_samples (server_id, ts);
CREATE TABLE IF NOT EXISTS user_samples (
    server_id TEXT NOT NULL,
    ts REAL NOT NULL,
    owner TEXT NOT NULL,
    cpu_cores REAL NOT NULL,
    rss_bytes INTEGER NOT NULL,
    nproc INTEGER NOT NULL,
    methods TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS user_samples_time ON user_samples (server_id, ts);
"""

MONTH_FILE = re.compile(r"^snapshots-(\d{4}-\d{2})\.db$")


def _connect(path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(str(path), timeout=30, check_same_thread=False)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")
    return conn


def compress(obj: dict) -> bytes:
    return zlib.compress(json.dumps(obj, separators=(",", ":"), ensure_ascii=True).encode("ascii"), 6)


def decompress(blob: bytes) -> dict:
    return json.loads(zlib.decompress(blob).decode("ascii"))


class Storage:
    def __init__(self, data_dir: str | os.PathLike):
        self.dir = Path(data_dir)
        self.dir.mkdir(parents=True, exist_ok=True)
        self.audit = _connect(self.dir / "audit.db")
        self.audit.executescript(AUDIT_SCHEMA)
        self.audit.commit()
        self._months: dict[str, sqlite3.Connection] = {}

    def close(self) -> None:
        for c in self._months.values():
            c.close()
        self._months.clear()
        self.audit.close()

    # Monthly snapshot files -------------------------------------------------

    def month_path(self, ym: str) -> Path:
        return self.dir / f"snapshots-{ym}.db"

    def month(self, ym: str, create: bool = True) -> sqlite3.Connection | None:
        conn = self._months.get(ym)
        if conn is not None:
            return conn
        path = self.month_path(ym)
        if not create and not path.exists():
            return None
        conn = _connect(path)
        conn.executescript(MONTH_SCHEMA)
        conn.commit()
        self._months[ym] = conn
        return conn

    def close_month(self, ym: str) -> None:
        conn = self._months.pop(ym, None)
        if conn is not None:
            conn.close()

    def month_files(self) -> list[tuple[str, Path]]:
        out = []
        for p in self.dir.iterdir():
            m = MONTH_FILE.match(p.name)
            if m:
                out.append((m.group(1), p))
        return sorted(out)

    def months_for(self, start_ts: float, end_ts: float) -> Iterator[sqlite3.Connection]:
        """Existing monthly connections overlapping the range."""
        for ym in timeutil.months_between(start_ts, end_ts):
            conn = self.month(ym, create=False)
            if conn is not None:
                yield conn

    def commit(self) -> None:
        self.audit.commit()
        for c in self._months.values():
            c.commit()

    # Audit helpers ------------------------------------------------------------

    def heartbeat(self) -> float | None:
        row = self.audit.execute("SELECT ts FROM heartbeat WHERE id = 1").fetchone()
        return row["ts"] if row else None

    def write_heartbeat(self, ts: float) -> None:
        self.audit.execute("INSERT INTO heartbeat (id, ts) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET ts = excluded.ts", (ts,))
        self.audit.commit()

    def add_event(self, ts: float, server_id: str | None, kind: str, detail: str) -> None:
        self.audit.execute("INSERT INTO events (ts, server_id, kind, detail) VALUES (?, ?, ?, ?)", (ts, server_id, kind, detail))

    def servers(self) -> list[sqlite3.Row]:
        return self.audit.execute("SELECT * FROM servers ORDER BY server_id").fetchall()

    def server(self, server_id: str) -> sqlite3.Row | None:
        return self.audit.execute("SELECT * FROM servers WHERE server_id = ?", (server_id,)).fetchone()
