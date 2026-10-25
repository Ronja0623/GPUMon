"""Processing of validated agent messages.

Attribution is computed on the agent: owner, attribution method, and context
fields are stored exactly as received.
"""

from __future__ import annotations

import hashlib
import json

from . import gaps, sessions
from .config import Config
from .logutil import Log
from .sessions import owners_on_gpu
from .storage import Storage, compress
from .timeutil import parse_iso, utc_month


def _compact(obj: dict) -> str:
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=True)


class Ingestor:
    def __init__(self, storage: Storage, cfg: Config, log: Log | None = None):
        self.st = storage
        self.cfg = cfg
        self.log = log or Log("gpumon.ingest")

    def ingest(self, msg: dict, received: float) -> str:
        """Store one schema-valid message. Returns "stored" or "duplicate"."""
        sid = msg["server_id"]
        agent_ts = parse_iso(msg["timestamp"])
        self._ensure_server(sid, received)
        kind = msg["type"]
        if kind == "sample":
            result = self._sample(msg, sid, agent_ts, received)
        elif kind == "inventory":
            result = self._inventory(msg, sid, agent_ts, received)
        else:
            result = self._disk_scan(msg, sid, agent_ts, received)
        self.st.commit()
        return result

    def _ensure_server(self, sid: str, now: float) -> None:
        cur = self.st.audit.execute("INSERT OR IGNORE INTO servers (server_id, first_seen) VALUES (?, ?)", (sid, now))
        if cur.rowcount:
            self.st.add_event(now, sid, "server_registered", f"Server {sid} registered automatically on its first message.")
            self.log.info("new server registered", server_id=sid)

    def _snapshot(self, msg: dict, sid: str, agent_ts: float, received: float):
        mc = self.st.month(utc_month(received))
        mc.execute(
            "INSERT INTO snapshots (server_id, kind, run_id, seq, agent_ts, received_ts, payload) VALUES (?, ?, ?, ?, ?, ?, ?)",
            (sid, msg["type"], msg["run_id"], msg.get("seq"), agent_ts, received, compress(msg)),
        )
        return mc

    def _sample(self, msg: dict, sid: str, agent_ts: float, received: float) -> str:
        db = self.st.audit
        row = self.st.server(sid)
        run_id, seq = msg["run_id"], msg["seq"]
        if row["last_run_id"] == run_id and row["last_seq"] is not None and seq <= row["last_seq"]:
            return "duplicate"
        if row["last_run_id"] and row["last_run_id"] != run_id:
            self.st.add_event(received, sid, "agent_restarted",
                              f"Agent on {sid} restarted: run_id changed from {row['last_run_id']} to {run_id}.")
            self.log.info("agent restarted", server_id=sid, run_id=run_id)

        interval = float(msg["interval_seconds"])
        limit = self.cfg.offline_after_intervals * interval
        prev = row["last_seen"]
        data_gap = False
        if prev is not None and received - prev > limit:
            offline = gaps.overlapping(self.st, sid, prev, received, gaps.CENTRAL_OFFLINE)
            data_gap = bool(offline)
            for a, b in gaps.running_segments(prev, received, offline):
                if b - a > limit:
                    text = gaps.add_gap(self.st, sid, a, b, gaps.AGENT_OFFLINE)
                    self.log.warn("data gap recorded", server_id=sid, description=text)
                    data_gap = True
        if data_gap:
            sessions.close_all(db, sid, gap=True)

        skew = agent_ts - received
        if abs(skew) > self.cfg.clock_skew_warn_seconds:
            self.log.warn("clock skew between agent and central exceeds the limit", _key=f"skew:{sid}",
                          server_id=sid, skew_seconds=round(skew, 1))

        mc = self._snapshot(msg, sid, agent_ts, received)
        self._normalized(mc, msg, sid, received, interval)
        db.execute(
            "UPDATE servers SET last_seen = ?, last_agent_ts = ?, last_run_id = ?, last_seq = ?, interval_s = ?,"
            " last_skew_s = ?, latest_sample = ? WHERE server_id = ?",
            (received, agent_ts, run_id, seq, interval, skew, _compact(msg), sid),
        )
        sessions.update(db, self.cfg, sid, received, msg)
        return "stored"

    def _normalized(self, mc, msg: dict, sid: str, ts: float, interval: float) -> None:
        h = msg["host"]
        mc.execute(
            "INSERT INTO host_samples VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (sid, ts, interval, h["cpu_percent"], h["logical_cores"], h["load_avg"][0], h["mem_used_bytes"],
             h["mem_total_bytes"], h["swap_used_bytes"], h["swap_total_bytes"]),
        )
        ignore = set(self.cfg.gpu["ignore_processes"])
        for g in msg["gpus"]:
            pids = sorted({p["pid"] for p in g["processes"] if p["type"] == "compute" and p["exe"] not in ignore})
            mc.execute("INSERT INTO gpu_samples VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                       (sid, ts, g["uuid"], g["index"], g["utilization_pct"], g["vram_used_mib"], g["vram_total_mib"],
                        json.dumps(pids)))
            for owner, o in owners_on_gpu(g, ignore).items():
                mc.execute("INSERT INTO gpu_owner_samples VALUES (?, ?, ?, ?, ?, ?, ?)",
                           (sid, ts, g["uuid"], owner, o["vram"], o["util"], json.dumps(sorted(o["methods"]))))
        for u in msg["users"]:
            mc.execute("INSERT INTO user_samples VALUES (?, ?, ?, ?, ?, ?, ?)",
                       (sid, ts, u["owner"], u["cpu_cores"], u["rss_bytes"], u["process_count"],
                        json.dumps(u["attributions"])))

    def _inventory(self, msg: dict, sid: str, agent_ts: float, received: float) -> str:
        inv = msg["inventory"]
        text = json.dumps(inv, sort_keys=True, separators=(",", ":"), ensure_ascii=True)
        digest = hashlib.sha256(text.encode("ascii")).hexdigest()
        last = self.st.audit.execute(
            "SELECT hash FROM inventory_history WHERE server_id = ? ORDER BY id DESC LIMIT 1", (sid,)).fetchone()
        if last is None or last["hash"] != digest:
            self.st.audit.execute(
                "INSERT INTO inventory_history (server_id, received_ts, agent_ts, hash, inventory) VALUES (?, ?, ?, ?, ?)",
                (sid, received, agent_ts, digest, text))
            if last is not None:
                self.st.add_event(received, sid, "inventory_changed", f"The hardware inventory of {sid} changed.")
        self.st.audit.execute("UPDATE servers SET latest_inventory = ?, hostname = ? WHERE server_id = ?",
                              (text, inv["hostname"], sid))
        self._snapshot(msg, sid, agent_ts, received)
        return "stored"

    def _disk_scan(self, msg: dict, sid: str, agent_ts: float, received: float) -> str:
        scan = msg["scan"]
        completed = parse_iso(scan["completed_at"])
        dup = self.st.audit.execute("SELECT 1 FROM disk_scans WHERE server_id = ? AND completed_ts = ? LIMIT 1",
                                    (sid, completed)).fetchone()
        if dup:
            return "duplicate"
        for p in scan["paths"]:
            self.st.audit.execute(
                "INSERT INTO disk_scans (server_id, received_ts, started_ts, completed_ts, partial, path, mount,"
                " total_bytes, path_partial, owners) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (sid, received, parse_iso(scan["started_at"]), completed, 1 if scan["partial"] else 0, p["path"],
                 p["mount"], p["total_bytes"], 1 if p["partial"] else 0, _compact({"owners": p["owners"]})))
        self.st.audit.execute("UPDATE servers SET latest_disk_scan = ?, disk_scan_received = ? WHERE server_id = ?",
                              (_compact(scan), received, sid))
        self._snapshot(msg, sid, agent_ts, received)
        return "stored"
