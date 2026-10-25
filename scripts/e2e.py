#!/usr/bin/env python3
"""End-to-end simulation (Section 8.3 of SPEC.md).

Starts the real gpumon-central and gpumon-agent binaries on localhost with
accelerated settings (interval 1 s, idle window 20 s, session gap tolerance
3 s), runs the ten steps of the scenario, and writes the step results and the
measured numbers to verification/e2e-results.json.
"""

from __future__ import annotations

import csv
import itertools
import json
import os
import re
import signal
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import traceback
import urllib.error
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
AGENT = REPO / "bin" / "gpumon-agent"
GPUMON = REPO / ".venv" / "bin" / "gpumon"
INVENTORY = REPO / "agent" / "testdata" / "sim-inventory.yaml"
OUT = REPO / "verification" / "e2e-results.json"
SERVERS = ["srv-40", "srv-41", "srv-42", "srv-43"]
GPU_NAMES = {
    "srv-40": ["NVIDIA GeForce RTX 3080"],
    "srv-41": ["NVIDIA GeForce RTX 2080 Ti", "NVIDIA GeForce RTX 2080 Ti"],
    "srv-42": ["NVIDIA GeForce RTX 3080 Ti"],
    "srv-43": ["NVIDIA GeForce RTX 5070 Ti"],
}
GAP_RE = re.compile(r"^DATA GAP: no samples from (\S+) between \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ and "
                    r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ \((agent offline or unreachable|central offline)\)\.$")
PRODUCTION_INTERVAL_S = 30
USERS = 20


class Fail(Exception):
    pass


def check(cond, msg):
    if not cond:
        raise Fail(msg)


class E2E:
    def __init__(self):
        work = REPO / "verification" / "e2e-work"
        work.mkdir(parents=True, exist_ok=True)
        self.tmp = Path(tempfile.mkdtemp(prefix="run-", dir=work))
        self.data = self.tmp / "data"
        self.port = self._free_port()
        self.base = f"http://127.0.0.1:{self.port}"
        self.token = self.tmp / "token"
        self.token.write_text(os.urandom(16).hex())
        self.token.chmod(0o600)
        self.servers_file = self.tmp / "servers.json"
        self.conf = self.tmp / "central.yaml"
        self.conf.write_text("\n".join([
            f'listen: "127.0.0.1:{self.port}"',
            f"data_dir: {self.data}",
            f"token_file: {self.token}",
            f"servers_file: {self.servers_file}",
            "expected_interval_seconds: 1",
            "session_gap_tolerance_minutes: 0.05",       # 3 s
            "gpu_state:",
            "  idle_window_minutes: 0.3333333333",        # 20 s
            "heartbeat_seconds: 1",
            "maintenance_interval_minutes: 600",
            "log_level: info",
        ]) + "\n")
        self.central: subprocess.Popen | None = None
        self.agents: dict[str, subprocess.Popen] = {}
        self.steps: list[dict] = []
        self.metrics: dict = {}
        self.t_start = time.time()

    @staticmethod
    def _free_port() -> int:
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
        s.close()
        return port

    # process control -----------------------------------------------------

    def start_central(self):
        log = open(self.tmp / "central.log", "a")
        self.central = subprocess.Popen([str(GPUMON), "serve", "--config", str(self.conf)], stdout=log, stderr=log)
        deadline = time.time() + 20
        while time.time() < deadline:
            try:
                if self.get("/api/v1/healthz")["status"] == "ok":
                    return
            except Exception:
                time.sleep(0.2)
        raise Fail("central did not start; see central.log")

    def stop_central(self):
        if self.central and self.central.poll() is None:
            self.central.send_signal(signal.SIGTERM)
            self.central.wait(timeout=20)

    def start_agent(self, sid):
        log = open(self.tmp / f"agent-{sid}.log", "a")
        self.agents[sid] = subprocess.Popen([
            str(AGENT), "simulate", "--inventory", str(INVENTORY), "--server-id", sid,
            "--central-url", f"{self.base}/api/v1/ingest", "--token-file", str(self.token),
            "--interval-seconds", "1", "--users", str(USERS)], stdout=log, stderr=log)

    def kill_agent(self, sid):
        p = self.agents[sid]
        p.send_signal(signal.SIGKILL)
        p.wait(timeout=10)

    def cleanup(self):
        for p in self.agents.values():
            if p.poll() is None:
                p.send_signal(signal.SIGTERM)
        for p in self.agents.values():
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()
        self.stop_central()

    # HTTP ----------------------------------------------------------------

    def get(self, path, expect=200):
        try:
            with urllib.request.urlopen(self.base + path, timeout=10) as r:
                code, body = r.status, r.read()
        except urllib.error.HTTPError as e:
            code, body = e.code, e.read()
        if code != expect:
            raise Fail(f"GET {path} returned {code}, expected {expect}: {body[:300]!r}")
        return json.loads(body)

    def status(self):
        return self.get("/api/v1/status")

    def wait_for(self, what, fn, timeout=60, every=1.0):
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            try:
                ok, last = fn()
                if ok:
                    return last
            except Fail as e:
                last = str(e)
            time.sleep(every)
        raise Fail(f"timed out waiting for {what}: {last}")

    # helpers ---------------------------------------------------------------

    def snapshot_bytes_and_samples(self):
        """Logical size of all raw snapshot files and the number of samples."""
        size = samples = 0
        for p in sorted(self.data.glob("snapshots-*.db")):
            conn = sqlite3.connect(f"file:{p}?mode=ro", uri=True)
            pc = conn.execute("PRAGMA page_count").fetchone()[0]
            ps = conn.execute("PRAGMA page_size").fetchone()[0]
            samples += conn.execute("SELECT COUNT(*) FROM snapshots WHERE kind = 'sample'").fetchone()[0]
            conn.close()
            size += pc * ps
        return size, samples

    def audit(self):
        return sqlite3.connect(f"file:{self.data / 'audit.db'}?mode=ro", uri=True)

    def step(self, num, name, reqs, fn):
        t = time.time()
        rec = {"step": num, "name": name, "reqs": reqs, "status": "pass", "details": ""}
        try:
            rec["details"] = fn() or ""
        except Exception as e:
            rec["status"] = "fail"
            rec["details"] = f"{e}" if isinstance(e, Fail) else traceback.format_exc()
        rec["seconds"] = round(time.time() - t, 1)
        self.steps.append(rec)
        print(f"[{rec['status'].upper()}] step {num}: {name} ({rec['seconds']} s) {rec['details'][:300]}", flush=True)
        return rec["status"] == "pass"

    # steps -------------------------------------------------------------------

    def s1_start_central(self):
        self.start_central()
        return f"central listening on {self.base} with data dir {self.data}"

    def s2_start_agents(self):
        self.servers_file.write_text(json.dumps({"servers": {s: {"display_name": s, "sort_order": i}
                                                             for i, s in enumerate(SERVERS)}}))
        for sid in SERVERS:
            self.start_agent(sid)
        self.wait_for("four servers online", lambda: (
            sum(1 for s in self.status()["servers"] if s["status"] == "online") == 4, None), timeout=30)
        self.size0 = self.snapshot_bytes_and_samples()
        self.t_size0 = time.time()
        return "four simulate agents running with interval 1 s and 20 background users each"

    def s3_assert_scenarios(self):
        def ready():
            st = self.status()
            states = {(g["server_id"], g["gpu_index"]): g["state"] for g in st["summary"]}
            return states.get(("srv-42", 0)) == "idle_occupied", st
        st = self.wait_for("the idle window (20 s) to elapse on srv-42", ready, timeout=60)
        self.size1 = self.snapshot_bytes_and_samples()
        self.t_size1 = time.time()
        servers = {s["server_id"]: s for s in st["servers"]}
        check(set(servers) == set(SERVERS), f"servers: {sorted(servers)}")
        check(all(s["status"] == "online" for s in servers.values()), "not all servers online")
        check(len(st["summary"]) == 5, f"expected 5 GPUs, got {len(st['summary'])}")
        for sid, names in GPU_NAMES.items():
            got = [g["name"] for g in servers[sid]["gpus"]]
            check(got == names, f"{sid} GPU names {got}")

        def gpu(sid, idx):
            return servers[sid]["gpus"][idx]

        def owners(sid, idx):
            return {o["owner"]: o for o in gpu(sid, idx)["owners"]}

        def users(sid):
            return {u["owner"]: u for u in servers[sid]["users"]}

        expectations = [
            ("free GPU", gpu("srv-40", 0)["state"] == "free" and owners("srv-40", 0) == {}),
            ("busy GPU", gpu("srv-41", 0)["state"] == "busy" and gpu("srv-43", 0)["state"] == "busy"),
            ("idle-occupied GPU", gpu("srv-42", 0)["state"] == "idle_occupied"),
            ("GPU shared by two users", set(owners("srv-41", 1)) == {"alice", "bob"} and gpu("srv-41", 1)["state"] == "busy"),
            ("rootful Docker with owner label", owners("srv-41", 0).get("carol", {}).get("methods") == ["docker_label"]
             and owners("srv-41", 0)["carol"]["containers"] == ["gpu0-job0"]),
            ("rootful Docker without label", owners("srv-42", 0).get("dave", {}).get("methods") == ["docker_default_owner"]),
            ("rootless Docker process (subuid)", owners("srv-43", 0).get("erin", {}).get("methods") == ["subuid"]),
            ("rootless Docker container root (uid)", "uid" in users("srv-43").get("erin", {}).get("attributions", [])),
            ("multipass VM", users("srv-40").get("frank", {}).get("attributions") == ["multipass_name"]
             and any(p.get("vm_name") == "frank-dev" for p in users("srv-40")["frank"]["top_cpu"])),
            ("multipass VM with an invalid name prefix",
             users("srv-40").get("unattributed", {}).get("attributions") == ["unattributed"]),
            ("sudo process", owners("srv-41", 1).get("alice", {}).get("methods") == ["loginuid"]),
            ("direct root login", users("srv-40").get("root-shared", {}).get("attributions") == ["root_shared"]),
            ("unknown UID", "uid:4242" in users("srv-43")),
            ("same username, different UIDs", "alice" in users("srv-41") or "alice" in owners("srv-41", 1)),
            ("disk scan shown with completion time", all(any(d["as_of"] for d in servers[s]["disk"]) for s in SERVERS)),
        ]
        failed = [name for name, ok in expectations if not ok]
        check(not failed, f"scenario checks failed: {failed}")
        order = [(g["state"], g["vram_free_mib"]) for g in st["summary"]]
        rank = {"free": 0, "idle_occupied": 1, "busy": 2, "unknown": 3}
        check(st["summary"][0]["server_id"] == "srv-40" and st["summary"][0]["state"] == "free", "free GPU is not first")
        for (a, fa), (b, fb) in itertools.pairwise(order):
            check(rank[a] < rank[b] or (rank[a] == rank[b] and (fa or 0) >= (fb or 0)), f"summary order wrong: {order}")
        # servers.json is picked up without a restart.
        self.servers_file.write_text(json.dumps({"servers": {"srv-42": {"display_name": "srv-42 renamed"}}}))
        self.wait_for("servers.json reload", lambda: (
            any(s["display_name"] == "srv-42 renamed" for s in self.status()["servers"]), None), timeout=10)
        summary = [(g["server_id"], g["gpu_index"], g["state"]) for g in st["summary"]]
        return f"{len(expectations)} scenario checks passed; summary order {summary}"

    def s4_agent_outage(self):
        self.kill_agent("srv-41")
        time.sleep(5)
        self.start_agent("srv-41")

        def gap():
            g = self.get("/api/v1/gaps?server=srv-41")["gaps"]
            hits = [x for x in g if x["reason"] == "agent_offline_or_unreachable"]
            return bool(hits), hits
        hits = self.wait_for("the agent gap", gap, timeout=20)
        check(all(GAP_RE.match(h["description"]) for h in hits), f"bad description: {hits[0]['description']}")
        ev = self.get("/api/v1/events")["events"]
        check(any(e["kind"] == "agent_restarted" and e["server_id"] == "srv-41" for e in ev), "no agent_restarted event")
        return hits[0]["description"]

    def s5_central_outage(self):
        with self.audit() as db:
            before = db.execute("SELECT COUNT(*) FROM gpu_sessions").fetchone()[0]
        self.stop_central()
        time.sleep(5)
        self.start_central()
        time.sleep(4)
        g = self.get("/api/v1/gaps")["gaps"]
        central = [x for x in g if x["reason"] == "central_offline"]
        check({x["server_id"] for x in central} >= set(SERVERS), f"central_offline gaps: {central}")
        check(all(GAP_RE.match(x["description"]) for x in central), "bad central gap description")
        sessions = self.get("/api/v1/sessions")["sessions"]
        keys = {}
        for s in sessions:
            keys.setdefault((s["server_id"], s["gpu_uuid"], s["owner"]), []).append(s)
        for k, ss in keys.items():
            check(sum(1 for s in ss if s["open"]) <= 1, f"two open sessions for {k}")
            ss.sort(key=lambda s: s["start_ts"])
            for a, b in itertools.pairwise(ss):
                check(a["end_ts"] < b["start_ts"], f"overlapping sessions for {k}")
        with self.audit() as db:
            dup = db.execute("SELECT COUNT(*) FROM (SELECT server_id, gpu_uuid, owner, start_ts, COUNT(*) c FROM gpu_sessions"
                             " GROUP BY server_id, gpu_uuid, owner, start_ts HAVING c > 1)").fetchone()[0]
        check(dup == 0, "duplicated sessions found")
        return (f"{len(central)} central_offline gaps; {len(sessions)} sessions ({before} before the restart), "
                f"no duplicates or overlaps")

    def s6_new_server(self):
        self.start_agent("srv-44")
        self.wait_for("srv-44 to register", lambda: (
            any(s["server_id"] == "srv-44" and s["status"] == "online" for s in self.status()["servers"]), None), timeout=20)
        ev = self.get("/api/v1/events")["events"]
        check(any(e["kind"] == "server_registered" and e["server_id"] == "srv-44" for e in ev), "no registration event")
        return "srv-44 registered automatically and is online"

    def s7_history_limit(self):
        old = time.time() - 73 * 3600
        for path in ("/api/v1/history", "/api/v1/gaps", "/api/v1/sessions", "/api/v1/events"):
            body = self.get(f"{path}?from={old}", expect=403)
            check("public history window" in body["error"], body)
        h = self.get("/api/v1/history")
        check(len(h["servers"]) >= 5, "history should list the servers")
        return "HTTP 403 with an English message for ranges older than 72 h"

    def s8_export(self):
        subprocess.run([str(GPUMON), "maintenance", "--config", str(self.conf)], check=True, capture_output=True)
        start = self.t_start - 3600
        end = time.time()
        expected = self.expected_counts(start, end)
        out = {}
        for kind in ("snapshots", "sessions", "rollups", "gaps", "disk"):
            path = self.tmp / f"export-{kind}.csv"
            subprocess.run([str(GPUMON), "export", "--config", str(self.conf), "--kind", kind, "--from", str(start),
                            "--to", str(end), "--out", str(path)], check=True, capture_output=True)
            with open(path, encoding="utf-8", newline="") as f:
                rows = list(csv.reader(f))
            sys.path.insert(0, str(REPO / "central"))
            from gpumon_central.export import HEADERS
            check(rows[0] == HEADERS[kind], f"{kind} header {rows[0]}")
            check(len(rows) - 1 == expected[kind], f"{kind}: {len(rows) - 1} rows, expected {expected[kind]}")
            check(len(rows) > 1, f"{kind}: no rows")
            check(rows[1][0].endswith("Z") and rows[1][1].endswith("+08:00"), f"{kind}: time columns {rows[1][:2]}")
            out[kind] = len(rows) - 1
        return f"rows per kind: {out}"

    def expected_counts(self, start, end):
        snap = 0
        for p in self.data.glob("snapshots-*.db"):
            c = sqlite3.connect(f"file:{p}?mode=ro", uri=True)
            for t in ("host_samples", "gpu_owner_samples", "user_samples"):
                snap += c.execute(f"SELECT COUNT(*) FROM {t} WHERE ts >= ? AND ts <= ?", (start, end)).fetchone()[0]
            c.close()
        with self.audit() as db:
            sessions = db.execute("SELECT COUNT(*) FROM gpu_sessions WHERE start_ts <= ? AND end_ts >= ?",
                                  (end, start)).fetchone()[0]
            gaps = db.execute("SELECT COUNT(*) FROM gaps WHERE start_ts <= ? AND end_ts >= ?", (end, start)).fetchone()[0]
            rollups = db.execute("SELECT COUNT(*) FROM daily_rollups").fetchone()[0]
            disk = sum(len(json.loads(r[0])["owners"]) for r in db.execute(
                "SELECT owners FROM disk_scans WHERE completed_ts >= ? AND completed_ts <= ?", (start, end)))
        return {"snapshots": snap, "sessions": sessions, "rollups": rollups, "gaps": gaps, "disk": disk}

    def s9_no_listening_sockets(self):
        listening = set()
        for f in ("tcp", "tcp6", "udp", "udp6"):
            p = Path("/proc/net") / f
            if not p.exists():
                continue
            for line in p.read_text().splitlines()[1:]:
                cols = line.split()
                if f.startswith("tcp") and cols[3] == "0A":
                    listening.add(cols[9])
                if f.startswith("udp"):
                    listening.add(cols[9])
        for line in Path("/proc/net/unix").read_text().splitlines()[1:]:
            cols = line.split()
            if int(cols[3], 16) & 0x10000:  # __SO_ACCEPTCON: a listening unix socket
                listening.add(cols[6])
        report = {}
        for sid, p in self.agents.items():
            check(p.poll() is None, f"agent {sid} is not running")
            inodes = set()
            fd_dir = Path(f"/proc/{p.pid}/fd")
            for fd in fd_dir.iterdir():
                try:
                    target = os.readlink(fd)
                except OSError:
                    continue
                m = re.match(r"socket:\[(\d+)\]", target)
                if m:
                    inodes.add(m.group(1))
            bad = inodes & listening
            check(not bad, f"agent {sid} has listening sockets {bad}")
            report[sid] = len(inodes)
        return f"agents checked: {sorted(report)}; none has a listening socket"

    def s10_measure(self):
        rss = {}
        for sid, p in self.agents.items():
            for line in Path(f"/proc/{p.pid}/status").read_text().splitlines():
                if line.startswith("VmRSS:"):
                    rss[sid] = int(line.split()[1]) / 1024
        max_rss = max(rss.values())
        (b0, n0), (b1, n1) = self.size0, self.size1
        check(n1 > n0, "no samples between the size measurements")
        per_sample = (b1 - b0) / (n1 - n0)
        per_day = per_sample * (86400 / PRODUCTION_INTERVAL_S) * len(SERVERS)
        self.metrics.update({
            "agent_rss_mib": {k: round(v, 1) for k, v in rss.items()},
            "agent_rss_max_mib": round(max_rss, 1),
            "raw_snapshot_bytes_per_sample": round(per_sample),
            "raw_snapshot_samples_measured": n1 - n0,
            "raw_snapshot_bytes_per_day": round(per_day),
            "raw_snapshot_projected_bytes_per_month": round(per_day * 30.44),
            "sizing_basis": f"{len(SERVERS)} servers, 5 GPUs, {USERS} simulated users per server, "
                            f"production interval {PRODUCTION_INTERVAL_S} s",
        })
        check(max_rss < 50, f"agent RSS {max_rss:.1f} MiB is not under 50 MB")
        return (f"max agent RSS {max_rss:.1f} MiB; {per_sample:.0f} bytes per sample -> {per_day / 2**20:.1f} MiB per day, "
                f"{per_day * 30.44 / 2**30:.2f} GiB per month")

    def secret_check(self):
        hits = []
        for p in list(self.data.iterdir()) + list(self.tmp.glob("*.log")):
            if b"SECRET123" in p.read_bytes():
                hits.append(p.name)
        import zlib
        for p in self.data.glob("snapshots-*.db"):
            c = sqlite3.connect(f"file:{p}?mode=ro", uri=True)
            for (blob,) in c.execute("SELECT payload FROM snapshots"):
                if b"SECRET123" in zlib.decompress(blob):
                    hits.append(p.name + " (payload)")
                    break
            c.close()
        check(not hits, f"SECRET123 found in {hits}")
        return "the --api-key=SECRET123 argv of a simulated process is absent from payloads, databases, and logs"

    def run(self) -> int:
        ok = True
        try:
            ok &= self.step(1, "start central on 127.0.0.1 with a temporary data directory", ["REQ-CEN-007"],
                            self.s1_start_central)
            if ok:
                ok &= self.step(2, "start four simulate agents (Section 1.1 inventory)", ["REQ-AGT-014", "REQ-CEN-002"],
                                self.s2_start_agents)
            if ok:
                self.step(3, "servers online, five GPUs, scenario states and owners, summary order, servers.json reload",
                          ["REQ-AGT-014", "REQ-CEN-002", "REQ-CEN-006", "REQ-DSH-002"], self.s3_assert_scenarios)
                self.step(4, "kill srv-41 for 5 s: gap with English description and agent_restarted event",
                          ["REQ-CEN-004"], self.s4_agent_outage)
                self.step(5, "stop central for 5 s: central_offline gaps and no duplicated sessions",
                          ["REQ-CEN-004", "REQ-STO-004"], self.s5_central_outage)
                self.step(6, "a fifth agent (srv-44) joins and auto-registers", ["REQ-OPS-004", "REQ-CEN-002"],
                          self.s6_new_server)
                self.step(7, "history older than the public window returns 403", ["REQ-DSH-008"], self.s7_history_limit)
                self.step(8, "gpumon export for every kind: headers and row counts", ["REQ-CEN-008"], self.s8_export)
                self.step(9, "no agent process has a listening socket", ["REQ-SEC-001"], self.s9_no_listening_sockets)
                self.step(10, "agent RSS and raw snapshot bytes per day", ["REQ-AGT-012", "REQ-STO-006"], self.s10_measure)
                self.step(11, "no argv secret in payloads, databases, or logs", ["REQ-SEC-007"], self.secret_check)
        finally:
            self.cleanup()
        OUT.parent.mkdir(parents=True, exist_ok=True)
        passed = all(s["status"] == "pass" for s in self.steps) and len(self.steps) == 11
        OUT.write_text(json.dumps({"passed": passed, "work_dir": str(self.tmp), "steps": self.steps,
                                   "metrics": self.metrics}, indent=2) + "\n")
        print(f"E2E {'PASSED' if passed else 'FAILED'}; results in {OUT}; logs in {self.tmp}")
        return 0 if passed else 1


if __name__ == "__main__":
    for p in (AGENT, GPUMON):
        if not p.exists():
            sys.exit(f"missing {p}; run 'make build' first")
    sys.exit(E2E().run())
