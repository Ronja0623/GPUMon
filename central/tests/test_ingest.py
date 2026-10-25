import hmac
import json
import logging
import os

import pytest

from conftest import EXAMPLES, TOKEN, auth, gpu, inventory, load_example, proc, sample
from gpumon_central import gaps, logutil
from gpumon_central.timeutil import parse_iso, utc_month


@pytest.mark.req("REQ-CEN-001")
def test_ingest_rejects_missing_or_wrong_token(client, monkeypatch):
    calls = []
    real = hmac.compare_digest
    monkeypatch.setattr(hmac, "compare_digest", lambda a, b: calls.append(1) or real(a, b))
    msg = sample()
    for headers in ({}, {"Authorization": "Bearer wrong"}, {"Authorization": TOKEN}, {"Authorization": "Basic x"}):
        r = client.post("/api/v1/ingest", json=msg, headers=headers)
        assert r.status_code == 401
        assert r.json()["error"] == "Missing or invalid bearer token."
    assert len(calls) >= 4, "the token must be compared with hmac.compare_digest (constant time)"
    assert client.post("/api/v1/ingest", json=msg, headers=auth()).status_code == 200


@pytest.mark.req("REQ-CEN-001")
def test_ingest_rejects_malformed_payload_with_english_error(client):
    r = client.post("/api/v1/ingest", content=b"{not json", headers=auth())
    assert r.status_code == 400 and r.json()["error"] == "The request body is not valid JSON."
    bad = sample()
    del bad["host"]
    r = client.post("/api/v1/ingest", json=bad, headers=auth())
    assert r.status_code == 400
    assert r.json()["error"].startswith("The payload does not match the schema:")
    assert "host" in r.json()["error"]
    bad = sample()
    bad["schema_version"] = 99
    assert client.post("/api/v1/ingest", json=bad, headers=auth()).status_code == 400


@pytest.mark.req("REQ-CEN-001")
@pytest.mark.req("REQ-CEN-005")
def test_receive_time_stored_next_to_agent_time(client, clock, storage):
    msg = sample(ts=clock.t - 12.5)
    r = client.post("/api/v1/ingest", json=msg, headers=auth())
    assert r.status_code == 200
    mc = storage.month(utc_month(clock.t))
    row = mc.execute("SELECT agent_ts, received_ts FROM snapshots").fetchone()
    assert row["received_ts"] == clock.t
    assert row["agent_ts"] == pytest.approx(parse_iso(msg["timestamp"]))
    srv = storage.server("srv-a")
    assert srv["last_agent_ts"] == pytest.approx(clock.t - 12.5)
    assert srv["last_skew_s"] == pytest.approx(-12.5)


@pytest.mark.req("REQ-CEN-002")
def test_auto_registration_and_inventory_history(post, storage, clock):
    post(sample(server="srv-new"))
    assert storage.server("srv-new") is not None
    ev = storage.audit.execute("SELECT * FROM events WHERE kind = 'server_registered'").fetchall()
    assert [e["server_id"] for e in ev] == ["srv-new"]
    post(inventory(server="srv-new"))
    post(inventory(server="srv-new", ts=clock.t + 10), received=clock.t + 10)  # unchanged
    post(inventory(server="srv-new", ts=clock.t + 20, driver="575.57.08"), received=clock.t + 20)
    hist = storage.audit.execute("SELECT inventory FROM inventory_history WHERE server_id = 'srv-new' ORDER BY id").fetchall()
    assert len(hist) == 2
    assert json.loads(hist[-1]["inventory"])["driver_version"] == "575.57.08"
    assert json.loads(storage.server("srv-new")["latest_inventory"])["driver_version"] == "575.57.08"


@pytest.mark.req("REQ-CEN-002")
def test_servers_file_overrides_reload_without_restart(client, post, cfg, app_factory):
    post(sample(server="srv-a"))
    post(sample(server="srv-b"))
    path = cfg.servers_file
    with open(path, "w") as f:
        json.dump({"servers": {"srv-a": {"display_name": "Alpha", "notes": "rack 2", "sort_order": 2},
                               "srv-b": {"display_name": "Bravo", "sort_order": 1}}}, f)
    s = client.get("/api/v1/status").json()
    assert [x["display_name"] for x in s["servers"]] == ["Bravo", "Alpha"]
    assert s["servers"][1]["notes"] == "rack 2"
    with open(path, "w") as f:
        json.dump({"servers": {"srv-a": {"display_name": "Alpha2"}, "srv-b": {"retired": True}}}, f)
    os.utime(path, ns=(os.stat(path).st_mtime_ns + 10**9, os.stat(path).st_mtime_ns + 10**9))
    s = client.get("/api/v1/status").json()
    assert [x["display_name"] for x in s["servers"]] == ["Alpha2"]


@pytest.mark.req("REQ-CEN-002")
def test_servers_file_sighup_reload(tmp_path):
    from gpumon_central.servers_file import ServersFile
    p = tmp_path / "servers.json"
    p.write_text(json.dumps({"servers": {"x": {"display_name": "One"}}}))
    sf = ServersFile(str(p))
    assert sf.get()["x"]["display_name"] == "One"
    st = os.stat(p)
    p.write_text(json.dumps({"servers": {"x": {"display_name": "Two"}}}))
    os.utime(p, ns=(st.st_atime_ns, st.st_mtime_ns))  # same mtime: only SIGHUP can notice
    assert sf.get()["x"]["display_name"] == "One"
    sf.request_reload()
    assert sf.get()["x"]["display_name"] == "Two"
    p.write_text("{broken")
    sf.request_reload()
    assert sf.get()["x"]["display_name"] == "Two", "an invalid file keeps the previous overrides"


@pytest.mark.req("REQ-CEN-003")
def test_online_offline_and_retired(client, post, clock, cfg, storage):
    post(sample(server="srv-a", interval=30))
    s = client.get("/api/v1/status").json()["servers"][0]
    assert s["status"] == "online"
    clock.advance(89)
    assert client.get("/api/v1/status").json()["servers"][0]["status"] == "online"
    clock.advance(2)  # 91 s > 3 x 30 s
    s = client.get("/api/v1/status").json()
    assert s["servers"][0]["status"] == "offline"
    assert s["summary"][0]["state"] == "unknown"
    with open(cfg.servers_file, "w") as f:
        json.dump({"servers": {"srv-a": {"retired": True}}}, f)
    assert client.get("/api/v1/status").json()["servers"] == []
    assert storage.server("srv-a") is not None, "history of a retired server is kept"


@pytest.mark.req("REQ-CEN-004")
def test_agent_gap_with_english_description_and_restart_event(post, storage, clock):
    t = clock.t
    post(sample(seq=1, ts=t), received=t)
    post(sample(seq=2, ts=t + 30), received=t + 30)
    post(sample(seq=3, ts=t + 60 + 2100, run_id="run2"), received=t + 60 + 2100)
    g = storage.audit.execute("SELECT * FROM gaps").fetchall()
    assert len(g) == 1
    assert g[0]["reason"] == "agent_offline_or_unreachable"
    assert g[0]["description"] == gaps.describe("srv-a", t + 30, t + 2160, gaps.AGENT_OFFLINE)
    assert g[0]["description"].startswith("DATA GAP: no samples from srv-a between ")
    assert g[0]["description"].endswith("(agent offline or unreachable).")
    ev = storage.audit.execute("SELECT * FROM events WHERE kind = 'agent_restarted'").fetchall()
    assert len(ev) == 1 and "run1" in ev[0]["detail"] and "run2" in ev[0]["detail"]


@pytest.mark.req("REQ-CEN-004")
def test_gap_description_format():
    text = gaps.describe("srv-41", parse_iso("2026-10-25T14:02:00Z"), parse_iso("2026-10-25T14:37:30Z"),
                         gaps.AGENT_OFFLINE)
    assert text == ("DATA GAP: no samples from srv-41 between 2026-10-25T14:02:00Z and 2026-10-25T14:37:30Z "
                    "(agent offline or unreachable).")


@pytest.mark.req("REQ-CEN-004")
def test_central_offline_gap_on_startup_and_no_double_counting(storage, ingestor, clock, cfg):
    t = clock.t
    ingestor.ingest(sample(seq=1, ts=t), t)
    storage.write_heartbeat(t + 20)
    # central stops at t + 20 and starts again at t + 600
    texts = gaps.record_central_offline(storage, t + 600)
    assert texts == [gaps.describe("srv-a", t + 20, t + 600, gaps.CENTRAL_OFFLINE)]
    ingestor.ingest(sample(seq=2, ts=t + 610), t + 610)
    rows = storage.audit.execute("SELECT reason FROM gaps ORDER BY start_ts").fetchall()
    assert [r["reason"] for r in rows] == ["central_offline"], "time while central was down is not an agent gap"
    # A real agent outage after the restart is still detected.
    ingestor.ingest(sample(seq=3, ts=t + 1000), t + 1000)
    rows = storage.audit.execute("SELECT reason FROM gaps ORDER BY start_ts").fetchall()
    assert [r["reason"] for r in rows] == ["central_offline", "agent_offline_or_unreachable"]


@pytest.mark.req("REQ-CEN-004")
def test_heartbeat_written_periodically(tmp_path, storage, clock, validator):
    import time as _time

    from starlette.testclient import TestClient

    from conftest import make_cfg
    from gpumon_central.api import create_app
    c = make_cfg(tmp_path, heartbeat_seconds=0.05, maintenance_interval_minutes=60)
    assert make_cfg(tmp_path).heartbeat_seconds == 60
    app = create_app(c, storage, TOKEN, clock=clock, validator=validator, background=True)
    with TestClient(app):
        first = storage.heartbeat()
        clock.advance(5)
        _time.sleep(0.3)
        assert storage.heartbeat() == clock.t > first


@pytest.mark.req("REQ-CEN-005")
def test_clock_skew_warning_and_dashboard(client, clock, cfg):
    stream = []

    class W:
        def write(self, s):
            stream.append(s)

        def flush(self):
            pass

    logutil.setup("info", 3600, stream=W(), clock=clock)
    try:
        for i in range(3):
            clock.advance(30)
            client.post("/api/v1/ingest", json=sample(seq=i + 1, ts=clock.t + 8.0 + i), headers=auth())
    finally:
        logging.getLogger().handlers.clear()
    lines = [json.loads(s) for s in "".join(stream).splitlines() if "clock skew" in s]
    assert len(lines) == 1, "the skew warning is rate-limited"
    assert lines[0]["level"] == "warn" and lines[0]["server_id"] == "srv-a" and lines[0]["skew_seconds"] == 8.0
    s = client.get("/api/v1/status").json()["servers"][0]
    assert s["clock_skew_warning"] is True and s["clock_skew_s"] == 10.0
    clock.advance(30)
    client.post("/api/v1/ingest", json=sample(seq=9, ts=clock.t + 1), headers=auth())
    assert client.get("/api/v1/status").json()["servers"][0]["clock_skew_warning"] is False


@pytest.mark.req("REQ-ATT-006")
def test_attribution_stored_unchanged(post, storage, clock):
    for path in sorted(EXAMPLES.glob("sample-*.json")):
        msg = json.loads(path.read_text())
        msg["timestamp"] = sample(ts=clock.t)["timestamp"]
        post(msg)
        stored = json.loads(storage.server(msg["server_id"])["latest_sample"])
        assert stored["gpus"] == msg["gpus"], "owner, method, and context must be stored exactly as received"
        assert stored["users"] == msg["users"]
    rows = storage.audit.execute("SELECT owner, methods, names FROM gpu_sessions WHERE server_id = 'srv-41'").fetchall()
    got = {r["owner"]: (json.loads(r["methods"]), json.loads(r["names"])) for r in rows}
    assert got["carol"][0] == ["docker_label"] and "gpu0-job0" in got["carol"][1]
    assert got["alice"][0] == ["loginuid"]


@pytest.mark.req("REQ-ATT-006")
def test_examples_validate_against_schema(validator):
    files = sorted(EXAMPLES.glob("*.json"))
    assert len(files) >= 6
    kinds = set()
    for f in files:
        msg = json.loads(f.read_text())
        validator(msg)
        assert msg["schema_version"] == 1
        kinds.add(msg["type"])
    assert kinds == {"sample", "inventory", "disk_scan"}


@pytest.mark.req("REQ-SEC-007")
def test_schema_refuses_command_lines(client):
    msg = sample(gpus=[gpu(procs=[proc(10, "bob")])])
    msg["gpus"][0]["processes"][0]["cmdline"] = "python train.py --api-key=SECRET123"
    assert client.post("/api/v1/ingest", json=msg, headers=auth()).status_code == 400
    msg = sample()
    msg["users"][0]["top_cpu"][0]["environ"] = "API_KEY=SECRET123"
    assert client.post("/api/v1/ingest", json=msg, headers=auth()).status_code == 400


@pytest.mark.req("REQ-SEC-007")
def test_secret_argv_never_reaches_database(post, storage, cfg):
    msg = load_example("sample-srv-40")
    # The simulator gives bob a process whose argv contains --api-key=SECRET123.
    assert any(u["owner"] == "bob" for u in msg["users"])
    post(msg)
    storage.commit()
    for f in os.listdir(cfg.data_dir):
        data = open(os.path.join(cfg.data_dir, f), "rb").read()
        assert b"SECRET123" not in data and b"api-key" not in data
    import zlib
    for (blob,) in storage.month(utc_month(storage.server("srv-40")["last_seen"])).execute("SELECT payload FROM snapshots"):
        assert b"SECRET123" not in zlib.decompress(blob)


@pytest.mark.req("REQ-CEN-001")
def test_duplicate_sample_is_ignored(post, storage):
    assert post(sample(seq=1))["status"] == "stored"
    assert post(sample(seq=1))["status"] == "duplicate"
