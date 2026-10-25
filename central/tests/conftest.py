"""Shared fixtures. Results are also written to verification/pytest-results.json
with the requirement IDs from @pytest.mark.req, for scripts/verify.py."""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest
from starlette.testclient import TestClient

from gpumon_central import config as cfgmod
from gpumon_central import resources
from gpumon_central.api import create_app
from gpumon_central.ingest import Ingestor
from gpumon_central.servers_file import ServersFile
from gpumon_central.storage import Storage
from gpumon_central.timeutil import iso_utc

REPO = Path(__file__).resolve().parents[2]
EXAMPLES = REPO / "schema" / "examples"
TOKEN = "test-token-0123456789"
T0 = 1793000000.0  # 2026-10-26T07:33:20Z

_results: list[dict] = []


def pytest_runtest_logreport(report):
    if report.when == "call" or (report.when == "setup" and report.outcome != "passed"):
        _results.append({"nodeid": report.nodeid, "outcome": report.outcome,
                         "reqs": getattr(report, "req_ids", []), "duration": report.duration})


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item, call):
    outcome = yield
    rep = outcome.get_result()
    rep.req_ids = [m.args[0] for m in item.iter_markers("req")]


def pytest_sessionfinish(session, exitstatus):
    out = os.environ.get("GPUMON_PYTEST_RESULTS")
    if out:
        Path(out).parent.mkdir(parents=True, exist_ok=True)
        Path(out).write_text(json.dumps({"results": _results}, indent=1))


class Clock:
    def __init__(self, t: float = T0):
        self.t = t

    def __call__(self) -> float:
        return self.t

    def advance(self, s: float) -> None:
        self.t += s


@pytest.fixture
def clock():
    return Clock()


def make_cfg(tmp_path: Path, **overrides) -> cfgmod.Config:
    data = {"data_dir": str(tmp_path / "data"), "token_file": str(tmp_path / "token"),
            "servers_file": str(tmp_path / "servers.json")}
    data.update(overrides)
    return cfgmod.load_dict(data)


@pytest.fixture
def cfg(tmp_path):
    return make_cfg(tmp_path)


@pytest.fixture
def storage(cfg):
    st = Storage(cfg.data_dir)
    yield st
    st.close()


@pytest.fixture
def validator():
    return resources.load_validator()


@pytest.fixture
def ingestor(storage, cfg):
    return Ingestor(storage, cfg)


@pytest.fixture
def app_factory(cfg, storage, clock, validator):
    def make(c=None, **kw):
        c = c or cfg
        return create_app(c, storage, TOKEN, clock=clock, validator=validator,
                          servers=ServersFile(c.servers_file), **kw)
    return make


@pytest.fixture
def client(app_factory):
    with TestClient(app_factory()) as c:
        yield c


def auth():
    return {"Authorization": f"Bearer {TOKEN}"}


def load_example(name: str) -> dict:
    return json.loads((EXAMPLES / f"{name}.json").read_text())


def proc(pid, owner, attribution="uid", vram=1000, exe="python3", typ="compute", **ctx):
    p = {"pid": pid, "type": typ, "vram_mib": vram, "exe": exe, "owner": owner, "attribution": attribution}
    p.update(ctx)
    return p


def gpu(index=0, uuid=None, name="Test GPU", total=10240, used=None, util=50, procs=()):
    procs = list(procs)
    if used is None:
        used = 250 + sum(p["vram_mib"] or 0 for p in procs)
    return {"index": index, "uuid": uuid or f"GPU-test-{index}", "name": name, "vram_total_mib": total,
            "vram_used_mib": used, "utilization_pct": util, "temperature_c": 50, "power_draw_w": 100.0,
            "power_limit_w": 300.0, "fan_pct": None, "processes": procs}


def user(owner, cores=1.0, rss=1 << 30, n=1, methods=("uid",)):
    top = [{"pid": 100, "exe": "python3", "cpu_cores": cores, "rss_bytes": rss}]
    return {"owner": owner, "attributions": list(methods), "cpu_cores": cores, "rss_bytes": rss,
            "process_count": n, "top_cpu": top, "top_rss": top}


def sample(server="srv-a", seq=1, ts=T0, run_id="run1", gpus=None, users=None, interval=30.0,
           filesystems=None, errors=None):
    return {
        "schema_version": 1, "type": "sample", "server_id": server, "run_id": run_id,
        "timestamp": iso_utc(ts), "agent_version": "test", "seq": seq, "interval_seconds": interval,
        "host": {"cpu_percent": 25.0, "logical_cores": 16, "load_avg": [1.0, 1.0, 1.0],
                 "mem_total_bytes": 64 << 30, "mem_used_bytes": 16 << 30, "mem_available_bytes": 48 << 30,
                 "swap_total_bytes": 8 << 30, "swap_used_bytes": 0},
        "gpus": gpus if gpus is not None else [gpu()],
        "users": users if users is not None else [user("alice")],
        "filesystems": filesystems if filesystems is not None else [
            {"mount": "/", "size_bytes": 1000 << 30, "used_bytes": 400 << 30, "free_bytes": 550 << 30}],
        "errors": errors or [],
    }


def inventory(server="srv-a", ts=T0, run_id="run1", driver="570.86.15", gpus=None):
    return {
        "schema_version": 1, "type": "inventory", "server_id": server, "run_id": run_id, "timestamp": iso_utc(ts),
        "agent_version": "test", "errors": [],
        "inventory": {"hostname": server, "os_release": "Ubuntu 24.04.1 LTS", "kernel": "6.8.0", "cpu_model": "CPU",
                      "logical_cores": 16, "mem_total_bytes": 64 << 30, "driver_version": driver,
                      "cuda_driver_version": "12.8",
                      "gpus": gpus if gpus is not None else [{"index": 0, "uuid": "GPU-test-0", "name": "Test GPU",
                                                              "vram_total_mib": 10240}],
                      "filesystems": [{"mount": "/", "size_bytes": 1000 << 30}]},
    }


def disk_scan(server="srv-a", ts=T0, owners=None, mount="/", partial=False):
    owners = owners if owners is not None else [{"owner": "alice", "bytes": 100 << 30}]
    return {
        "schema_version": 1, "type": "disk_scan", "server_id": server, "run_id": "run1", "timestamp": iso_utc(ts),
        "agent_version": "test", "errors": [],
        "scan": {"started_at": iso_utc(ts - 60), "completed_at": iso_utc(ts), "partial": partial,
                 "paths": [{"path": "/home", "mount": mount, "total_bytes": sum(o["bytes"] for o in owners),
                            "partial": partial, "owners": owners}]},
    }


@pytest.fixture
def post(client, clock):
    def do(msg, received=None):
        if received is not None:
            clock.t = received
        r = client.post("/api/v1/ingest", json=msg, headers=auth())
        assert r.status_code == 200, r.text
        return r.json()
    return do
