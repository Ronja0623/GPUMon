import csv
import io
import os
from datetime import date
from pathlib import Path

import pytest

from conftest import T0, disk_scan, gpu, inventory, proc, sample, user
from gpumon_central import export, gaps, maintenance, retention, rollups
from gpumon_central.cli import main as cli_main
from gpumon_central.logutil import Log
from gpumon_central.timeutil import local_day_bounds, parse_iso


@pytest.mark.req("REQ-STO-001")
def test_monthly_snapshot_files_with_wal(ingestor, storage, cfg):
    oct31 = parse_iso("2026-10-31T23:59:59Z")
    nov1 = parse_iso("2026-11-01T00:00:01Z")
    ingestor.ingest(sample(seq=1, ts=oct31), oct31)
    ingestor.ingest(sample(seq=2, ts=nov1), nov1)
    names = sorted(p.name for p in Path(cfg.data_dir).glob("snapshots-*.db"))
    assert names == ["snapshots-2026-10.db", "snapshots-2026-11.db"]
    for ym in ("2026-10", "2026-11"):
        assert storage.month(ym).execute("PRAGMA journal_mode").fetchone()[0] == "wal"
        assert storage.month(ym).execute("SELECT COUNT(*) FROM snapshots").fetchone()[0] == 1
    assert storage.audit.execute("PRAGMA journal_mode").fetchone()[0] == "wal"


class FakeFiles:
    def __init__(self, sizes):
        self.sizes = dict(sizes)
        self.removed = []

    def listdir(self, d):
        return list(self.sizes) + ["audit.db", "notes.txt"]

    def size(self, p):
        return self.sizes[Path(p).name]

    def remove(self, p):
        self.removed.append(Path(p).name)
        del self.sizes[Path(p).name]


@pytest.mark.req("REQ-STO-002")
def test_retention_by_age(tmp_path):
    now = parse_iso("2026-10-15T00:00:00Z")
    files = FakeFiles({f"snapshots-2026-{m:02d}.db": 100 for m in range(5, 11)})
    events = []
    deleted = retention.apply(tmp_path, now, 3, 10**9, Log(), events.append, files=files)
    assert deleted == ["snapshots-2026-05.db", "snapshots-2026-06.db", "snapshots-2026-07.db"]
    assert sorted(files.sizes) == ["snapshots-2026-08.db", "snapshots-2026-09.db", "snapshots-2026-10.db"]
    assert len(events) == 3 and all(e.startswith("Deleted raw snapshot file snapshots-2026-0") for e in events)
    assert "older than the last 3 months" in events[0]


@pytest.mark.req("REQ-STO-002")
def test_retention_by_size_never_deletes_current(tmp_path, caplog):
    now = parse_iso("2026-10-15T00:00:00Z")
    files = FakeFiles({"snapshots-2026-08.db": 3000, "snapshots-2026-09.db": 3000, "snapshots-2026-10.db": 3000})
    events = []
    deleted = retention.apply(tmp_path, now, 3, 6500, Log(), events.append, files=files)
    assert deleted == ["snapshots-2026-08.db"]
    assert "above the limit of 6500 bytes" in events[0]
    # The current month alone is above the limit: everything else goes, the current file stays, an error is logged.
    files = FakeFiles({"snapshots-2026-09.db": 10, "snapshots-2026-10.db": 9000})
    with caplog.at_level("ERROR"):
        deleted = retention.apply(tmp_path, now, 3, 5000, Log(), events.append, files=files)
    assert deleted == ["snapshots-2026-09.db"] and "snapshots-2026-10.db" in files.sizes
    assert any("current month's raw snapshot file alone exceeds" in r.getMessage() for r in caplog.records)


@pytest.mark.req("REQ-STO-002")
def test_retention_real_files_and_events(storage, cfg, ingestor):
    old = parse_iso("2026-01-10T00:00:00Z")
    now = parse_iso("2026-10-10T00:00:00Z")
    ingestor.ingest(sample(seq=1, ts=old), old)
    ingestor.ingest(sample(seq=2, ts=now), now)
    res = maintenance.run(storage, cfg, now)
    assert res["deleted"] == ["snapshots-2026-01.db"]
    assert not (Path(cfg.data_dir) / "snapshots-2026-01.db").exists()
    ev = storage.audit.execute("SELECT * FROM events WHERE kind = 'raw_snapshot_deleted'").fetchall()
    assert len(ev) == 1 and "snapshots-2026-01.db" in ev[0]["detail"]


@pytest.mark.req("REQ-STO-003")
def test_audit_database_is_permanent(storage, cfg, ingestor):
    tables = {r[0] for r in storage.audit.execute("SELECT name FROM sqlite_master WHERE type = 'table'")}
    assert {"servers", "inventory_history", "gpu_sessions", "daily_rollups", "disk_scans", "gaps", "events"} <= tables
    old = parse_iso("2025-01-10T00:00:00Z")
    ingestor.ingest(sample(seq=1, ts=old, gpus=[gpu(procs=[proc(1, "alice")])]), old)
    ingestor.ingest(inventory(ts=old), old)
    ingestor.ingest(disk_scan(ts=old), old)
    gaps.add_gap(storage, "srv-a", old, old + 100, gaps.AGENT_OFFLINE)
    rollups.compute_day(storage, cfg, date(2025, 1, 10), old)
    storage.commit()
    counts = {t: storage.audit.execute(f"SELECT COUNT(*) FROM {t}").fetchone()[0]
              for t in ("inventory_history", "gpu_sessions", "daily_rollups", "disk_scans", "gaps", "servers")}
    maintenance.run(storage, cfg, parse_iso("2026-10-10T00:00:00Z"), rollup_days=400)
    for t, n in counts.items():
        assert storage.audit.execute(f"SELECT COUNT(*) FROM {t}").fetchone()[0] >= n, t
    assert storage.audit.execute("SELECT COUNT(*) FROM daily_rollups WHERE local_date = '2025-01-10'").fetchone()[0] > 0, \
        "rollups survive after their raw snapshots were deleted"


@pytest.mark.req("REQ-STO-005")
def test_daily_rollups_values_and_idempotence(storage, cfg, ingestor):
    day = date(2026, 10, 20)
    start, _ = local_day_bounds(day, cfg.tz)  # Asia/Taipei midnight
    for i in range(120):  # one hour at 30 s
        ts = start + 30 * i
        users = [user("alice", cores=2.0, rss=2 << 30), user("bob", cores=0.5, rss=1 << 30)]
        gpus = [gpu(0, procs=[proc(1, "alice", vram=2048)]),
                gpu(1, procs=[proc(2, "alice", vram=1024), proc(3, "bob", vram=512)])]
        ingestor.ingest(sample(seq=i + 1, ts=ts, users=users, gpus=gpus), ts)
    ingestor.ingest(sample(seq=999, ts=start - 30, users=[user("alice", cores=64)]), start - 30)  # previous local day
    assert rollups.compute_day(storage, cfg, day, start + 7200) == 2
    rows = {r["owner"]: dict(r) for r in storage.audit.execute("SELECT * FROM daily_rollups WHERE local_date = '2026-10-20'")}
    a, b = rows["alice"], rows["bob"]
    assert a["gpu_hours"] == pytest.approx(2.0) and b["gpu_hours"] == pytest.approx(1.0)
    assert a["vram_gib_hours"] == pytest.approx(3.0) and b["vram_gib_hours"] == pytest.approx(0.5)
    assert a["cpu_core_hours"] == pytest.approx(2.0) and b["cpu_core_hours"] == pytest.approx(0.5)
    assert a["mem_gib_hours"] == pytest.approx(2.0) and a["peak_rss_bytes"] == 2 << 30
    before = [tuple(r)[:8] for r in storage.audit.execute("SELECT * FROM daily_rollups ORDER BY owner")]
    rollups.compute_day(storage, cfg, day, start + 9000)
    rollups.compute_day(storage, cfg, day, start + 9999)
    after = [tuple(r)[:8] for r in storage.audit.execute("SELECT * FROM daily_rollups ORDER BY owner")]
    assert before == after, "recomputing the same day is idempotent"


def read_csv(path):
    with open(path, encoding="utf-8", newline="") as f:
        return list(csv.reader(f))


@pytest.mark.req("REQ-CEN-008")
def test_export_every_kind(storage, cfg, ingestor, tmp_path):
    t = parse_iso("2026-01-05T02:00:00Z")  # far outside the public window
    for i in range(4):
        ingestor.ingest(sample(seq=i + 1, ts=t + 30 * i, gpus=[gpu(procs=[proc(1, "alice"), proc(2, "bob")])],
                               users=[user("alice"), user("bob")]), t + 30 * i)
    ingestor.ingest(sample(seq=10, ts=t + 1000, gpus=[gpu(procs=[proc(1, "alice")])]), t + 1000)
    ingestor.ingest(disk_scan(ts=t + 5, owners=[{"owner": "alice", "bytes": 10}, {"owner": "bob", "bytes": 20}]), t + 5)
    rollups.compute_day(storage, cfg, date(2026, 1, 5), t + 2000)
    storage.commit()
    expect = {"snapshots": 5 + 9 + 9,  # host rows + GPU-owner rows + user rows
              "sessions": 3, "rollups": 2, "gaps": 1, "disk": 2}
    for kind in export.KINDS:
        out = tmp_path / f"{kind}.csv"
        n = export.export(storage, cfg, kind, t - 10, t + 3000, str(out))
        rows = read_csv(out)
        assert rows[0] == export.HEADERS[kind]
        assert len(rows) - 1 == n == expect[kind], kind
        utc_col, local_col = rows[0][0], rows[0][1]
        assert utc_col.endswith("_utc") and local_col.endswith("_local")
        assert rows[1][0].endswith("Z") and rows[1][1].endswith("+08:00"), "local time is Asia/Taipei"
    # Filters.
    out = tmp_path / "f.csv"
    assert export.export(storage, cfg, "sessions", t - 10, t + 3000, str(out), owner="bob") == 1
    assert export.export(storage, cfg, "snapshots", t - 10, t + 3000, str(out), owner="bob", server="srv-a") == 4 + 4
    assert export.export(storage, cfg, "disk", t - 10, t + 3000, str(out), server="other") == 0


@pytest.mark.req("REQ-CEN-008")
def test_export_cli(tmp_path, cfg, ingestor, storage):
    t = parse_iso("2026-02-01T00:00:00Z")
    ingestor.ingest(sample(ts=t), t)
    storage.commit()
    conf = tmp_path / "central.yaml"
    conf.write_text(f"data_dir: {cfg.data_dir}\ntimezone: Asia/Taipei\n")
    out = tmp_path / "out.csv"
    rc = cli_main(["export", "--config", str(conf), "--kind", "snapshots", "--from", "2026-01-31",
                   "--to", "2026-02-02T00:00:00Z", "--out", str(out)])
    assert rc == 0
    text = out.read_bytes().decode("utf-8")
    assert text.splitlines()[0] == ",".join(export.HEADERS["snapshots"])
    assert len(list(csv.reader(io.StringIO(text)))) == 1 + 2  # header, host row, alice cpu_mem row
    assert cli_main(["export", "--config", str(conf), "--kind", "gaps", "--from", "yesterday", "--to", "now",
                     "--out", str(out)]) == 2


@pytest.mark.req("REQ-STO-004")
def test_stale_sessions_closed_as_data_gap(storage, cfg, ingestor):
    ingestor.ingest(sample(gpus=[gpu(procs=[proc(1, "alice")])]), T0)
    maintenance.run(storage, cfg, T0 + 3600)
    r = storage.audit.execute("SELECT open, ended_by_data_gap FROM gpu_sessions").fetchone()
    assert r["open"] == 0 and r["ended_by_data_gap"] == 1
    assert os.path.exists(cfg.data_dir)


def ingest_days(ingestor, first_day, n_days, cfg):
    """One sample per local hour on n_days consecutive local days, with a GPU session and a disk scan."""
    from datetime import timedelta
    seq = 0
    for k in range(n_days):
        start, _ = local_day_bounds(first_day + timedelta(days=k), cfg.tz)
        for h in range(24):
            seq += 1
            ts = start + 3600 * h + 60
            ingestor.ingest(sample(seq=seq, ts=ts, interval=3600, gpus=[gpu(procs=[proc(1, "alice")])],
                                   users=[user("alice"), user("bob")]), ts)
        ingestor.ingest(disk_scan(ts=start + 7200, owners=[{"owner": "alice", "bytes": 10}]), start + 7200)


@pytest.mark.req("REQ-CEN-008")
def test_auto_export_writes_days_that_left_the_window(storage, cfg, ingestor, tmp_path):
    from datetime import timedelta
    ingest_days(ingestor, date(2026, 10, 15), 10, cfg)  # Oct 15 .. Oct 24 (Asia/Taipei)
    now = local_day_bounds(date(2026, 10, 25), cfg.tz)[0] + 4.5 * 3600  # Oct 25 04:30 local
    written = export.auto_export(storage, cfg, now)
    out = Path(cfg.export_dir)
    assert out == Path(cfg.data_dir) / "exports" and oct(out.stat().st_mode & 0o777) == "0o750"
    days = sorted({p.name.split("-", 1)[1][:-4] for p in written})
    # now - 72 h is Oct 22 04:30, so Oct 21 is the newest complete day; 7 days of catch-up.
    assert days == [(date(2026, 10, 21) - timedelta(days=i)).isoformat() for i in range(6, -1, -1)]
    assert sorted({p.name.split("-")[0] for p in written}) == ["disk", "gaps", "rollups", "sessions"]
    assert len(written) == 7 * 4
    f = out / "rollups-2026-10-20.csv"
    rows = read_csv(f)
    assert rows[0] == export.HEADERS["rollups"] and {r[2] for r in rows[1:]} == {"2026-10-20"}
    assert {r[4] for r in rows[1:]} == {"alice", "bob"}
    assert oct(f.stat().st_mode & 0o777) == "0o640"
    disk_rows = read_csv(out / "disk-2026-10-20.csv")
    assert len(disk_rows) == 2 and disk_rows[1][1].startswith("2026-10-20T")
    sess = read_csv(out / "sessions-2026-10-20.csv")
    assert len(sess) == 2, "the one long session overlaps the day"
    assert not list(out.glob("*.tmp"))
    # Idempotent: a second run writes nothing; a missing file is written again.
    assert export.auto_export(storage, cfg, now) == []
    (out / "gaps-2026-10-18.csv").unlink()
    assert [p.name for p in export.auto_export(storage, cfg, now)] == ["gaps-2026-10-18.csv"]
    # The next day, only the newly expired day is written.
    assert sorted(p.name for p in export.auto_export(storage, cfg, now + 86400)) == [
        f"{k}-2026-10-22.csv" for k in ("disk", "gaps", "rollups", "sessions")]
    # Days before the first server registered are skipped.
    assert all(p.name >= "" for p in written)
    assert not (out / "sessions-2026-10-14.csv").exists()


@pytest.mark.req("REQ-CEN-008")
def test_auto_export_options(tmp_path, ingestor, storage, cfg):
    from conftest import make_cfg
    ingest_days(ingestor, date(2026, 10, 1), 3, cfg)
    c = make_cfg(tmp_path, data_dir=cfg.data_dir,
                 auto_export={"dir": str(tmp_path / "out"), "kinds": ["snapshots", "gaps"], "catch_up_days": 2})
    now = local_day_bounds(date(2026, 10, 10), c.tz)[0]
    # The last two days (Oct 5 and 6) have no records but still get header-only files.
    empty = export.auto_export(storage, c, now)
    assert sorted(p.name for p in empty) == sorted(f"{k}-2026-10-0{d}.csv" for k in ("snapshots", "gaps") for d in (5, 6))
    assert all(len(read_csv(p)) == 1 for p in empty)
    written = export.auto_export(storage, c, now, catch_up_days=30)
    assert sorted(p.name for p in written) == sorted(f"{k}-2026-10-0{d}.csv" for k in ("snapshots", "gaps") for d in (1, 2, 3, 4))
    snap = read_csv(tmp_path / "out" / "snapshots-2026-10-02.csv")
    assert len(snap) == 1 + 24 * 4, "host, gpu, and two cpu_mem rows per hourly sample"
    (tmp_path / "out" / "gaps-2026-10-02.csv").write_text("stale\n")
    assert [p.name for p in export.auto_export(storage, c, now, day=date(2026, 10, 2))] == [
        "snapshots-2026-10-02.csv", "gaps-2026-10-02.csv"]
    assert read_csv(tmp_path / "out" / "gaps-2026-10-02.csv")[0] == export.HEADERS["gaps"]
    from gpumon_central import config as cfgmod
    for bad in ({"kinds": ["everything"]}, {"kinds": []}, {"dir": "relative"}, {"catch_up_days": 0}):
        with pytest.raises(cfgmod.ConfigError, match='key "auto_export.'):
            cfgmod.load_dict({"auto_export": bad})


@pytest.mark.req("REQ-CEN-008")
def test_auto_export_cli(tmp_path, ingestor, storage, cfg, capsys):
    ingest_days(ingestor, date(2026, 10, 1), 2, cfg)
    storage.commit()
    conf = tmp_path / "central.yaml"
    conf.write_text(f"data_dir: {cfg.data_dir}\nauto_export:\n  dir: {tmp_path / 'exp'}\n")
    assert cli_main(["auto-export", "--config", str(conf), "--date", "2026-10-02"]) == 0
    assert "wrote 4 files" in capsys.readouterr().out
    assert sorted(p.name for p in (tmp_path / "exp").iterdir()) == [
        f"{k}-2026-10-02.csv" for k in ("disk", "gaps", "rollups", "sessions")]
    assert cli_main(["auto-export", "--config", str(conf), "--date", "2026-13-01"]) == 2
    assert cli_main(["auto-export", "--config", str(conf), "--catch-up-days", "0"]) == 2
