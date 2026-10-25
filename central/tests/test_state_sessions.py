import json

import pytest

from conftest import T0, gpu, make_cfg, proc, sample
from gpumon_central import gpustate, sessions
from gpumon_central.ingest import Ingestor
from gpumon_central.storage import Storage


def states(client):
    return {(g["server_id"], g["gpu_index"]): g["state"] for g in client.get("/api/v1/status").json()["summary"]}


@pytest.mark.req("REQ-CEN-006")
def test_free_busy_and_ignored_processes(post, client):
    post(sample(server="free-x", gpus=[gpu(procs=[proc(5, "system", "system", vram=200, exe="Xorg", typ="graphics")])]))
    post(sample(server="free-ignored", gpus=[gpu(procs=[proc(6, "system", "system", vram=100, exe="Xorg")])]))
    post(sample(server="busy-c", gpus=[gpu(procs=[proc(7, "alice", vram=100)], util=0)]))
    post(sample(server="busy-vram", gpus=[gpu(used=600, procs=[])]))
    post(sample(server="free-edge", gpus=[gpu(used=512, procs=[])]))
    s = states(client)
    assert s[("free-x", 0)] == "free"
    assert s[("free-ignored", 0)] == "free", "compute processes named in ignore_processes do not count"
    assert s[("busy-c", 0)] == "busy", "idle_occupied needs the whole idle window"
    assert s[("busy-vram", 0)] == "busy"
    assert s[("free-edge", 0)] == "free"


@pytest.mark.req("REQ-CEN-006")
def test_idle_occupied_needs_full_window_and_low_mean(post, client, clock):
    t = clock.t
    # 31 minutes of samples with the same compute process at 2 % utilization.
    for i in range(63):
        g = [gpu(util=2, procs=[proc(42, "dave", "docker_default_owner")])]
        post(sample(server="idle", seq=i + 1, ts=t + 30 * i, gpus=g), received=t + 30 * i)
    assert states(client)[("idle", 0)] == "idle_occupied"
    # A busy burst raises the mean above 5 %.
    post(sample(server="idle", seq=100, ts=clock.t + 30, gpus=[gpu(util=100, procs=[proc(42, "dave")])]), received=clock.t + 30)
    post(sample(server="idle", seq=101, ts=clock.t + 30, gpus=[gpu(util=100, procs=[proc(42, "dave")])]), received=clock.t + 30)
    assert states(client)[("idle", 0)] == "busy"


@pytest.mark.req("REQ-CEN-006")
def test_idle_window_requires_the_same_process(post, client, clock):
    t = clock.t
    for i in range(63):
        pid = 1000 + i  # a different process every sample
        post(sample(server="churn", seq=i + 1, ts=t + 30 * i, gpus=[gpu(util=0, procs=[proc(pid, "erin")])]), received=t + 30 * i)
    assert states(client)[("churn", 0)] == "busy"


@pytest.mark.req("REQ-CEN-006")
def test_thresholds_are_configurable(tmp_path):
    cfg = make_cfg(tmp_path, gpu_state={"free_vram_mib": 100, "idle_window_minutes": 1, "idle_util_pct": 50,
                                        "ignore_processes": ["mywatcher"]})
    g = gpu(used=200, procs=[])
    assert gpustate.classify(g, True, [], T0, 30, cfg) == "busy"
    g = gpu(procs=[proc(1, "a", exe="mywatcher")], used=50)
    assert gpustate.classify(g, True, [], T0, 30, cfg) == "free"
    rows = [(T0 - 60 + 10 * i, 40, "[7]") for i in range(7)]
    g = gpu(procs=[proc(7, "a")])
    assert gpustate.classify(g, True, rows, T0, 10, cfg) == "idle_occupied"
    assert gpustate.classify(g, False, rows, T0, 10, cfg) == "unknown"
    assert gpustate.classify(None, True, rows, T0, 10, cfg) == "unknown"
    rows_gap = rows[:2] + rows[5:]
    assert gpustate.classify(g, True, rows_gap, T0, 10, cfg) == "busy", "a gap inside the window is not idle"


def open_sessions(storage):
    return storage.audit.execute("SELECT * FROM gpu_sessions WHERE open = 1").fetchall()


def all_sessions(storage):
    return [sessions.to_dict(r) for r in storage.audit.execute("SELECT * FROM gpu_sessions ORDER BY id")]


@pytest.mark.req("REQ-STO-004")
def test_session_statistics_and_shared(ingestor, storage, clock):
    t = clock.t
    for i, (va, vb, util) in enumerate([(1000, 0, 40), (3000, 2000, 60), (2000, 0, 80)]):
        procs = [proc(10, "alice", "loginuid", vram=va, container_name="exp1")]
        if vb:
            procs.append(proc(11, "bob", vram=vb))
        ingestor.ingest(sample(seq=i + 1, ts=t + 30 * i, gpus=[gpu(util=util, procs=procs)]), t + 30 * i)
    by = {s["owner"]: s for s in all_sessions(storage)}
    a = by["alice"]
    assert a["start_ts"] == t and a["end_ts"] == t + 60 and a["duration_s"] == 60 and a["open"]
    assert a["vram_peak_mib"] == 3000 and a["vram_avg_mib"] == 2000 and a["util_avg_pct"] == 60
    assert a["shared"] is True and a["methods"] == ["loginuid"] and a["names"] == ["exp1", "python3"]
    assert by["bob"]["shared"] is True and by["bob"]["util_avg_pct"] == 60


@pytest.mark.req("REQ-STO-004")
def test_session_closes_after_tolerance(ingestor, storage, clock):
    t = clock.t
    g_on = [gpu(procs=[proc(10, "alice")])]
    seq = iter(range(1, 100))
    ingestor.ingest(sample(seq=next(seq), ts=t, gpus=g_on), t)
    for k in range(30, 270, 30):  # absent for 4 minutes, samples keep arriving
        ingestor.ingest(sample(seq=next(seq), ts=t + k, gpus=[gpu(procs=[])]), t + k)
    assert len(open_sessions(storage)) == 1
    ingestor.ingest(sample(seq=next(seq), ts=t + 270, gpus=g_on), t + 270)
    assert len(all_sessions(storage)) == 1, "absence within the tolerance continues the session"
    for k in range(300, 600, 30):
        ingestor.ingest(sample(seq=next(seq), ts=t + k, gpus=[gpu(procs=[])]), t + k)
    assert len(open_sessions(storage)) == 1, "absent for 300 s: not yet longer than the tolerance"
    ingestor.ingest(sample(seq=next(seq), ts=t + 600, gpus=[gpu(procs=[])]), t + 600)
    s = all_sessions(storage)
    assert len(s) == 1 and not s[0]["open"] and s[0]["end_ts"] == t + 270 and not s[0]["ended_by_data_gap"]


@pytest.mark.req("REQ-STO-004")
def test_session_closed_by_data_gap(ingestor, storage, clock):
    t = clock.t
    g_on = [gpu(procs=[proc(10, "alice")])]
    ingestor.ingest(sample(seq=1, ts=t, gpus=g_on), t)
    ingestor.ingest(sample(seq=2, ts=t + 30, gpus=g_on), t + 30)
    ingestor.ingest(sample(seq=3, ts=t + 200, gpus=g_on), t + 200)  # 170 s > 3 x 30 s: data gap
    s = all_sessions(storage)
    assert len(s) == 2
    assert s[0]["ended_by_data_gap"] and s[0]["end_ts"] == t + 30 and not s[0]["open"]
    assert s[1]["open"] and s[1]["start_ts"] == t + 200


@pytest.mark.req("REQ-STO-004")
def test_long_interval_keeps_one_session(ingestor, storage, clock):
    t = clock.t
    for i in range(5):  # hourly samples: longer than the 5 min tolerance, but consecutive
        ingestor.ingest(sample(seq=i + 1, ts=t + 3600 * i, interval=3600, gpus=[gpu(procs=[proc(1, "alice")])]), t + 3600 * i)
    s = all_sessions(storage)
    assert len(s) == 1 and s[0]["duration_s"] == 4 * 3600


@pytest.mark.req("REQ-STO-004")
def test_names_limited_to_ten(ingestor, storage, clock):
    t = clock.t
    for i in range(15):
        ingestor.ingest(sample(seq=i + 1, ts=t + 30 * i, gpus=[gpu(procs=[proc(10, "alice", exe=f"job{i}")])]), t + 30 * i)
    s = all_sessions(storage)
    assert len(s) == 1 and len(s[0]["names"]) == 10


@pytest.mark.req("REQ-STO-004")
def test_restart_keeps_open_sessions_without_duplicates(cfg, clock):
    t = clock.t
    g_on = [gpu(procs=[proc(10, "alice")])]
    st = Storage(cfg.data_dir)
    Ingestor(st, cfg).ingest(sample(seq=1, ts=t, gpus=g_on), t)
    Ingestor(st, cfg).ingest(sample(seq=2, ts=t + 30, gpus=g_on), t + 30)
    st.close()
    st = Storage(cfg.data_dir)  # central restarted quickly (no heartbeat gap recorded in this test)
    ing = Ingestor(st, cfg)
    ing.ingest(sample(seq=2, ts=t + 30, gpus=g_on), t + 30)  # a replayed message is a duplicate
    ing.ingest(sample(seq=3, ts=t + 60, gpus=g_on), t + 60)
    rows = [sessions.to_dict(r) for r in st.audit.execute("SELECT * FROM gpu_sessions")]
    assert len(rows) == 1 and rows[0]["open"] and rows[0]["sample_count"] == 3 and rows[0]["end_ts"] == t + 60
    # The database itself forbids two open sessions for one key.
    import sqlite3
    with pytest.raises(sqlite3.IntegrityError):
        st.audit.execute("INSERT INTO gpu_sessions (server_id, gpu_uuid, owner, start_ts, end_ts, last_seen_ts,"
                         " sample_count, methods, names, open)"
                         " VALUES ('srv-a', 'GPU-test-0', 'alice', 0, 0, 0, 1, '[]', '[]', 1)")
    st.close()


@pytest.mark.req("REQ-STO-004")
def test_restart_after_long_outage_closes_with_data_gap(cfg, clock):
    from gpumon_central import gaps
    t = clock.t
    g_on = [gpu(procs=[proc(10, "alice")])]
    st = Storage(cfg.data_dir)
    Ingestor(st, cfg).ingest(sample(seq=1, ts=t, gpus=g_on), t)
    st.write_heartbeat(t + 10)
    st.close()
    st = Storage(cfg.data_dir)
    gaps.record_central_offline(st, t + 900)
    Ingestor(st, cfg).ingest(sample(seq=2, ts=t + 905, gpus=g_on), t + 905)
    rows = [sessions.to_dict(r) for r in st.audit.execute("SELECT * FROM gpu_sessions ORDER BY id")]
    assert len(rows) == 2
    assert rows[0]["ended_by_data_gap"] and rows[0]["end_ts"] == t
    assert rows[1]["open"] and rows[1]["start_ts"] == t + 905
    assert json.loads(json.dumps(rows))  # serializable
    st.close()
