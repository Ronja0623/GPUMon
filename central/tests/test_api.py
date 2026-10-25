import re

import pytest
from starlette.testclient import TestClient

from conftest import REPO, disk_scan, gpu, make_cfg, proc, sample, user
from gpumon_central import cli
from gpumon_central import config as cfgmod


def two_servers(post, clock):
    post(sample(server="srv-x", gpus=[
        gpu(0, uuid="GPU-x0", name="Card A", total=10240, procs=[proc(1, "alice", "loginuid", vram=6000)], util=90),
        gpu(1, uuid="GPU-x1", name="Card A", total=10240, procs=[]),
    ], users=[user("alice", 3.0, methods=["loginuid"]), user("system", 0.2, methods=["system"]),
              user("root-shared", 0.1, methods=["root_shared"]), user("unattributed", 0.5, methods=["unattributed"])]))
    post(sample(server="srv-y", gpus=[
        gpu(0, uuid="GPU-y0", name="Card B", total=24576, procs=[]),
        gpu(1, uuid="GPU-y1", name="Card B", total=24576, procs=[
            proc(2, "carol", "docker_label", vram=2000, container_name="train", container_image="pytorch"),
            proc(3, "frank", "multipass_name", vram=1000, exe="qemu-system-x86_64", vm_name="frank-dev")]),
    ]))


@pytest.mark.req("REQ-DSH-001")
def test_single_page_without_login(client):
    r = client.get("/")
    assert r.status_code == 200 and r.headers["content-type"].startswith("text/html")
    html = r.text
    assert "<title>GPUMon</title>" in html and 'id="summary"' in html and 'id="servers"' in html
    js = client.get("/static/app.js").text
    assert "setInterval(refresh, CFG.refresh_seconds * 1000)" in js, "the page refreshes itself without reloading"
    assert "location.reload" not in js
    cfg = client.get("/api/v1/config").json()
    assert cfg["refresh_seconds"] == 15
    for path in ("/api/v1/status", "/api/v1/history", "/api/v1/gaps", "/api/v1/sessions", "/api/v1/events"):
        assert client.get(path).status_code == 200, path


@pytest.mark.req("REQ-DSH-001")
def test_refresh_interval_is_configurable(tmp_path, storage, clock, validator):
    from gpumon_central.api import create_app
    c = make_cfg(tmp_path, refresh_seconds=5)
    with TestClient(create_app(c, storage, "t", clock=clock, validator=validator)) as tc:
        assert tc.get("/api/v1/config").json()["refresh_seconds"] == 5


@pytest.mark.req("REQ-DSH-002")
def test_summary_order(client, post, clock):
    two_servers(post, clock)
    s = client.get("/api/v1/status").json()["summary"]
    got = [(g["server_id"], g["gpu_index"], g["state"]) for g in s]
    assert got == [("srv-y", 0, "free"), ("srv-x", 1, "free"), ("srv-y", 1, "busy"), ("srv-x", 0, "busy")]
    first = s[0]
    for k in ("server_id", "display_name", "gpu_name", "vram_free_mib", "vram_total_mib", "owners", "state"):
        assert k in first
    assert s[2]["owners"] == ["carol", "frank"] and s[2]["vram_free_mib"] == 24576 - 3250


@pytest.mark.req("REQ-DSH-003")
def test_server_card_fields(client, post, clock):
    post(sample(server="srv-x", ts=clock.t + 9))
    s = client.get("/api/v1/status").json()["servers"][0]
    for k in ("display_name", "status", "last_seen", "last_seen_local", "clock_skew_s", "clock_skew_warning", "host", "gpus"):
        assert k in s, k
    h = s["host"]
    for k in ("cpu_percent", "load_avg", "mem_used_bytes", "mem_total_bytes", "swap_used_bytes", "swap_total_bytes"):
        assert k in h, k
    assert s["status"] == "online" and s["clock_skew_warning"] is True


@pytest.mark.req("REQ-DSH-004")
def test_gpu_detail_fields(client, post, clock):
    two_servers(post, clock)
    srv = {x["server_id"]: x for x in client.get("/api/v1/status").json()["servers"]}
    g = srv["srv-y"]["gpus"][1]
    for k in ("name", "state", "utilization_pct", "vram_used_mib", "vram_total_mib", "owners"):
        assert k in g
    owners = {o["owner"]: o for o in g["owners"]}
    assert owners["carol"]["vram_mib"] == 2000 and owners["carol"]["methods"] == ["docker_label"]
    assert owners["carol"]["containers"] == ["train"] and owners["frank"]["vms"] == ["frank-dev"]
    assert owners["frank"]["exes"] == ["qemu-system-x86_64"]
    js = (REPO / "web" / "app.js").read_text()
    assert 'badge(g.state)' in js and 'text: label' in js, "the state badge shows text, not only color"


@pytest.mark.req("REQ-DSH-005")
def test_per_user_table(client, post, clock):
    two_servers(post, clock)
    srv = {x["server_id"]: x for x in client.get("/api/v1/status").json()["servers"]}
    users = {u["owner"]: u for u in srv["srv-x"]["users"]}
    assert users["alice"]["cpu_cores"] == 3.0 and users["alice"]["process_count"] == 1
    assert users["alice"]["attributions"] == ["loginuid"] and users["alice"]["rss_bytes"] == 1 << 30
    assert not users["alice"]["special"]
    for special in ("system", "root-shared", "unattributed"):
        assert users[special]["special"] is True
    css = (REPO / "web" / "style.css").read_text()
    assert ".owner.special" in css


@pytest.mark.req("REQ-DSH-006")
def test_disk_chart_segments(client, post, clock, tmp_path, storage, validator):
    from gpumon_central.api import create_app
    c = make_cfg(tmp_path, disk_top_owners=2)
    owners = [{"owner": "alice", "bytes": 500}, {"owner": "bob", "bytes": 300}, {"owner": "carol", "bytes": 100},
              {"owner": "uid:4242", "bytes": 50}]
    with TestClient(create_app(c, storage, "tok", clock=clock, validator=validator)) as tc:
        h = {"Authorization": "Bearer tok"}
        fs = [{"mount": "/home", "size_bytes": 2000, "used_bytes": 1200, "free_bytes": 700}]
        assert tc.post("/api/v1/ingest", json=sample(filesystems=fs), headers=h).status_code == 200
        d = tc.get("/api/v1/status").json()["servers"][0]["disk"][0]
        assert d["as_of"] is None and [s["kind"] for s in d["segments"]] == ["unscanned", "free"]
        assert tc.post("/api/v1/ingest", json=disk_scan(owners=owners, mount="/home", partial=True), headers=h).status_code == 200
        d = tc.get("/api/v1/status").json()["servers"][0]["disk"][0]
    segs = [(s["kind"], s["label"], s["bytes"]) for s in d["segments"]]
    assert segs == [("owner", "alice", 500), ("owner", "bob", 300), ("others", "others", 150),
                    ("unscanned", "unscanned/other usage", 250), ("free", "free", 700)]
    assert d["as_of"].endswith("Z") and d["as_of_local"].endswith("+08:00") and d["partial"] is True
    js = (REPO / "web" / "app.js").read_text()
    assert "per-owner scan as of" in js and "partial" in js and "fmtBytes(s.bytes)" in js


@pytest.mark.req("REQ-DSH-007")
def test_history_stacked_by_owner_with_gaps(client, post, clock):
    t = clock.t - 3600
    for i in range(10):
        post(sample(seq=i + 1, ts=t + 30 * i, users=[user("alice", 2.0), user("bob", 1.0)],
                    gpus=[gpu(procs=[proc(1, "alice", vram=1000), proc(2, "bob", vram=500)], util=80)]), received=t + 30 * i)
    post(sample(seq=50, ts=t + 2000, users=[user("alice", 2.0)]), received=t + 2000)
    clock.t = t + 2010
    h = client.get("/api/v1/history").json()
    s = h["servers"][0]
    assert set(s["cpu_by_owner"]) == {"alice", "bob"} and set(s["mem_by_owner"]) == {"alice", "bob"}
    g = s["gpus"][0]
    assert set(g["vram_by_owner"]) == {"alice", "bob"}
    assert g["util_by_owner"] == {}, "device utilization is never split across owners"
    assert any(v == 80 for v in g["util"] if v is not None)
    vals = [v for v in s["cpu_by_owner"]["alice"] if v is not None]
    assert vals and all(v == pytest.approx(2.0) for v in vals)
    assert len(h["gaps"]) == 1 and h["gaps"][0]["description"].startswith("DATA GAP: no samples from srv-a between")
    js = (REPO / "web" / "app.js").read_text()
    assert "gapPlugin" in js and "hit.description" in js


@pytest.mark.req("REQ-DSH-008")
def test_public_endpoints_reject_old_ranges(client, clock):
    old = clock.t - 73 * 3600
    for path in ("/api/v1/history", "/api/v1/gaps", "/api/v1/sessions", "/api/v1/events"):
        r = client.get(path, params={"from": str(old)})
        assert r.status_code == 403, path
        assert "public history window of the last 72 hours" in r.json()["error"]
        assert client.get(path, params={"from": str(clock.t - 71 * 3600)}).status_code == 200
        assert client.get(path, params={"from": "2020-01-01T00:00:00Z"}).status_code == 403


@pytest.mark.req("REQ-DSH-009")
def test_served_assets_reference_no_external_hosts(client):
    html = client.get("/").text
    assets = re.findall(r'(?:src|href)="([^"]+)"', html)
    assert len(assets) >= 4
    bodies = {"/": html}
    for a in assets:
        assert not re.match(r"^(https?:)?//", a), a
        r = client.get("/" + a.lstrip("/"))
        assert r.status_code == 200, a
        bodies[a] = r.text
    for name, body in bodies.items():
        found = re.findall(r"(?:https?:)?//[A-Za-z0-9.-]+\.[A-Za-z]{2,}", body)
        assert found == [], f"{name} references external hosts: {found[:5]}"


@pytest.mark.req("REQ-DSH-010")
def test_english_ui_and_timezone(client, post):
    post(sample())
    html = client.get("/").text
    assert '<html lang="en">' in html
    for f in ("index.html", "app.js", "style.css"):
        (REPO / "web" / f).read_bytes().decode("ascii")
    st = client.get("/api/v1/status").json()
    assert st["timezone"] == "Asia/Taipei" and st["timezone_label"] == "Asia/Taipei (UTC+08:00)"
    assert st["servers"][0]["last_seen_local"].endswith("+08:00")
    assert client.get("/api/v1/config").json()["timezone_label"] == "Asia/Taipei (UTC+08:00)"
    js = (REPO / "web" / "app.js").read_text()
    assert 'timeZone: CFG.timezone' in js and '"Times in " + CFG.timezone_label' in js


@pytest.mark.req("REQ-CEN-007")
def test_bind_address_and_root_refusal(tmp_path, monkeypatch, caplog):
    assert cfgmod.DEFAULTS["listen"] == "127.0.0.1:8080"
    c = cfgmod.load_dict({"listen": "10.0.0.40:9000"})
    assert (c.host, c.port) == ("10.0.0.40", 9000)
    tok = tmp_path / "token"
    tok.write_text("abc")
    tok.chmod(0o600)
    conf = tmp_path / "c.yaml"
    conf.write_text(f"listen: 127.0.0.1:18181\ndata_dir: {tmp_path / 'd'}\ntoken_file: {tok}\n")
    captured = {}

    class FakeServer:
        def __init__(self, config):
            captured["host"], captured["port"] = config.host, config.port

        def run(self):
            captured["ran"] = True

    import uvicorn
    monkeypatch.setattr(uvicorn, "Server", FakeServer)
    monkeypatch.setattr(cli.os, "geteuid", lambda: 0)
    assert cli.main(["serve", "--config", str(conf)]) == 1
    assert "ran" not in captured
    assert cli.main(["serve", "--config", str(conf), "--allow-root"]) == 0
    assert captured == {"host": "127.0.0.1", "port": 18181, "ran": True}
    monkeypatch.setattr(cli.os, "geteuid", lambda: 1000)
    captured.clear()
    assert cli.main(["serve", "--config", str(conf)]) == 0 and captured["port"] == 18181
