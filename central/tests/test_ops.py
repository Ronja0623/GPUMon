import io
import json
import logging
import os
import re
import shutil
import subprocess
import tarfile

import pytest

from conftest import REPO, TOKEN, auth, sample
from gpumon_central import cli, config, logutil
from gpumon_central.logutil import Log
from gpumon_central.resources import TokenError, read_token

SHELLCHECK = REPO / ".venv" / "bin" / "shellcheck"


def unit(name):
    lines = (REPO / "deploy" / name).read_text().splitlines()
    return [ln.strip() for ln in lines if ln.strip() and not ln.strip().startswith("#")]


@pytest.mark.req("REQ-SEC-002")
def test_central_refuses_wide_token_permissions(tmp_path, monkeypatch):
    p = tmp_path / "token"
    p.write_text("secret-value\n")
    for mode in (0o600, 0o400):
        p.chmod(mode)
        assert read_token(str(p)) == "secret-value"
    for mode in (0o640, 0o644, 0o604, 0o700):
        p.chmod(mode)
        with pytest.raises(TokenError) as e:
            read_token(str(p))
        assert "refusing to start" in str(e.value) and "secret-value" not in str(e.value)
    conf = tmp_path / "c.yaml"
    conf.write_text(f"token_file: {p}\ndata_dir: {tmp_path / 'd'}\n")
    monkeypatch.setattr(cli.os, "geteuid", lambda: 1000)
    assert cli.main(["serve", "--config", str(conf)]) == 1, "central must refuse to start"


@pytest.mark.req("REQ-SEC-002")
@pytest.mark.req("REQ-LOG-001")
def test_token_never_logged(client, clock):
    buf = io.StringIO()
    logutil.setup("debug", 0, stream=buf)
    try:
        client.post("/api/v1/ingest", json=sample(), headers={"Authorization": "Bearer wrong-guess"})
        client.post("/api/v1/ingest", json=sample(), headers=auth())
        Log("gpumon.test").info("done")
    finally:
        logging.getLogger().handlers.clear()
    out = buf.getvalue()
    assert TOKEN not in out and "wrong-guess" not in out
    assert "ingest rejected" in out
    for line in out.splitlines():
        rec = json.loads(line)
        assert {"ts", "level", "msg"} <= set(rec)
        line.encode("ascii")


@pytest.mark.req("REQ-SEC-003")
def test_central_unit_hardening():
    u = unit("gpumon-central.service")
    for want in ("User=gpumon", "NoNewPrivileges=yes", "ProtectSystem=strict", "ReadWritePaths=/var/lib/gpumon",
                 "PrivateTmp=yes", "ProtectHome=yes"):
        assert want in u, want
    assert any(x.startswith("ExecStart=") and " serve " in x and "--allow-root" not in x for x in u)
    assert "User=root" not in u


@pytest.mark.req("REQ-SEC-004")
def test_agent_unit_hardening():
    u = unit("gpumon-agent.service")
    for want in ("User=root", "NoNewPrivileges=yes", "ProtectSystem=strict", "ProtectHome=read-only", "PrivateTmp=yes",
                 "ProtectKernelTunables=yes", "ProtectKernelModules=yes",
                 "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6"):
        assert want in u, want
    writable = [x for x in u if x.startswith(("ReadWritePaths=", "ReadWriteDirectories="))]
    assert writable == [], "no writable paths beyond the optional state directory"
    assert [x for x in u if x.startswith("StateDirectory=")] == ["StateDirectory=gpumon-agent"]


@pytest.mark.req("REQ-LOG-001")
def test_central_logs_json_lines_ascii():
    buf = io.StringIO()
    logutil.setup("info", 3600, stream=buf)
    try:
        Log("gpumon.x").warn("odd value", value="caf\u00e9 \u2603")
        logging.getLogger("uvicorn.error").info("uvicorn message")
    finally:
        logging.getLogger().handlers.clear()
    lines = buf.getvalue().splitlines()
    assert len(lines) == 2
    for line in lines:
        line.encode("ascii")
        rec = json.loads(line)
        assert re.match(r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$", rec["ts"])
    assert json.loads(lines[0])["value"] == "caf\u00e9 \u2603"


@pytest.mark.req("REQ-LOG-002")
def test_levels_and_rate_limit():
    now = [1000.0]
    buf = io.StringIO()
    logutil.setup("warn", 3600, stream=buf, clock=lambda: now[0])
    try:
        log = Log("gpumon.y")
        log.info("hidden")
        log.debug("hidden")
        for _ in range(5):
            log.warn("same warning", server_id="a")
            log.error("same error")
            now[0] += 60
        log.warn("same warning", server_id="b")
        now[0] += 3600
        log.warn("same warning", server_id="a")
    finally:
        logging.getLogger().handlers.clear()
    recs = [json.loads(x) for x in buf.getvalue().splitlines()]
    assert all(r["msg"] != "hidden" for r in recs)
    assert [(r["msg"], r.get("server_id"), r.get("suppressed")) for r in recs] == [
        ("same warning", "a", None), ("same error", None, None), ("same warning", "b", None), ("same warning", "a", 4)]
    for level in ("debug", "info", "warn", "error"):
        assert config.load_dict({"log_level": level}).log_level == level
    with pytest.raises(config.ConfigError, match='key "log_level"'):
        config.load_dict({"log_level": "trace"})


@pytest.mark.req("REQ-LOG-003")
def test_ascii_check_script(tmp_path):
    script = REPO / "scripts" / "ascii_check.py"
    r = subprocess.run(["python3", str(script), str(REPO)], capture_output=True, text=True)
    assert r.returncode == 0, r.stdout
    (tmp_path / "ok.py").write_text("print('hello')\n")
    (tmp_path / "vendor").mkdir()
    (tmp_path / "vendor" / "lib.js").write_bytes("// \u00e9\n".encode())
    assert subprocess.run(["python3", str(script), str(tmp_path)], capture_output=True).returncode == 0
    (tmp_path / "doc.md").write_bytes("Caf\u00e9\n".encode())
    r = subprocess.run(["python3", str(script), str(tmp_path)], capture_output=True, text=True)
    assert r.returncode == 1 and "doc.md:1:4" in r.stdout


@pytest.mark.req("REQ-OPS-001")
def test_makefile_targets():
    text = (REPO / "Makefile").read_text()
    targets = set(re.findall(r"^([a-z][a-z0-9-]*):", text, re.M))
    assert {"build", "test", "lint", "e2e", "verify", "package"} <= targets
    lint = text.split("\nlint:")[1].split("\n\n")[0]
    for tool in ("$(GO) vet", "gofmt -l", "ruff check"):
        assert tool in lint, tool


@pytest.mark.req("REQ-OPS-002")
def test_agent_package_contents(tmp_path):
    if not shutil.which("go") and not os.path.exists("/usr/local/go/bin/go"):
        pytest.fail("Go is required to build the agent package")
    r = subprocess.run([str(REPO / "scripts" / "package.sh"), "0.0.0-test", "--agent-only", "--outdir", str(tmp_path)],
                       capture_output=True, text=True, cwd=REPO)
    assert r.returncode == 0, r.stderr
    with tarfile.open(tmp_path / "gpumon-agent-0.0.0-test-linux-amd64.tar.gz") as tf:
        members = {m.name.split("/", 1)[1]: m for m in tf.getmembers() if "/" in m.name}
        assert set(members) == {"gpumon-agent", "gpumon-agent.service", "agent.yaml.example", "install-agent.sh"}
        assert members["gpumon-agent"].mode & 0o111 and members["install-agent.sh"].mode & 0o111
        tf.extractall(tmp_path / "x", filter="data") if hasattr(tarfile, "data_filter") else tf.extractall(tmp_path / "x")
    pkg = tmp_path / "x" / "gpumon-agent-0.0.0-test-linux-amd64"
    head = (pkg / "gpumon-agent").read_bytes()[:20]
    assert head[:4] == b"\x7fELF" and head[4] == 2 and head[18] == 0x3E, "linux/amd64 ELF binary"
    assert subprocess.run([str(pkg / "gpumon-agent"), "version"], capture_output=True, text=True).stdout.strip() == "0.0.0-test"
    # The packaged installer is idempotent (staging root, no systemd).
    tok = tmp_path / "tok"
    tok.write_text("t0k3n\n")
    args = [str(pkg / "install-agent.sh"), "--central-url", "http://192.0.2.40:8080/api/v1/ingest", "--token-file", str(tok),
            "--server-id", "srv-99", "--root", str(tmp_path / "root")]
    first = subprocess.run(args, capture_output=True, text=True)
    second = subprocess.run(args, capture_output=True, text=True)
    assert first.returncode == 0 and second.returncode == 0, first.stderr + second.stderr
    assert "nothing changed" in second.stdout and "nothing changed" not in first.stdout
    root = tmp_path / "root"
    assert oct((root / "etc/gpumon/token").stat().st_mode & 0o777) == "0o600"
    conf = (root / "etc/gpumon/agent.yaml").read_text()
    assert 'central_url: "http://192.0.2.40:8080/api/v1/ingest"' in conf and 'server_id: "srv-99"' in conf
    assert (root / "etc/systemd/system/gpumon-agent.service").exists()


@pytest.mark.req("REQ-OPS-003")
def test_install_central_lint_and_dry_run():
    scripts = [str(REPO / "deploy" / "install-central.sh"), str(REPO / "deploy" / "install-agent.sh")]
    r = subprocess.run([str(SHELLCHECK), *scripts], capture_output=True, text=True)
    assert r.returncode == 0, r.stdout
    assert subprocess.run(["bash", "-n", scripts[0]]).returncode == 0
    r = subprocess.run([scripts[0], "--dry-run", "--no-systemd", "--listen", "192.0.2.40:8080"], capture_output=True, text=True)
    assert r.returncode == 0, r.stderr
    out = r.stdout
    assert re.search(r"DRY-RUN: useradd --system .* gpumon", out) or "user gpumon exists" in out
    assert "DRY-RUN: python3 -m venv /opt/gpumon/venv" in out or "virtualenv exists" in out
    assert "DRY-RUN: install -d -m 0750 -o gpumon -g gpumon /var/lib/gpumon" in out
    assert "gpumon-central.service" in out and "/etc/gpumon/token" in out
    r = subprocess.run([scripts[0]], capture_output=True, text=True)
    if os.geteuid() != 0:
        assert r.returncode != 0 and "run as root" in r.stderr


@pytest.mark.req("REQ-OPS-005")
def test_central_config_errors_name_the_key(tmp_path):
    cases = {
        "listen: nope\n": 'key "listen"',
        "listn: 1.2.3.4:80\n": 'unknown key "listn"',
        "gpu_state:\n  idle_util_pct: 500\n": 'key "gpu_state.idle_util_pct"',
        "gpu_state:\n  free_vram: 1\n": 'unknown key "gpu_state.free_vram"',
        "raw_retention_months: three\n": 'key "raw_retention_months": invalid value',
        "timezone: Mars/Base\n": 'key "timezone"',
        "public_history_hours: 0\n": 'key "public_history_hours"',
    }
    for text, want in cases.items():
        p = tmp_path / "c.yaml"
        p.write_text(text)
        with pytest.raises(config.ConfigError) as e:
            config.load(str(p))
        assert want in str(e.value) and str(e.value).startswith("config error: "), (text, str(e.value))
    p = tmp_path / "bad.yaml"
    p.write_text("listen: [\n")
    assert cli.main(["check-config", "--config", str(p)]) == 2
    d = config.load(None)
    assert d.listen == "127.0.0.1:8080" and d.raw_max_total_bytes == 5368709120 and d.timezone == "Asia/Taipei"
    assert d.gpu["ignore_processes"] == ["Xorg", "Xwayland", "gnome-shell"] and d.session_gap_tolerance_minutes == 5


@pytest.mark.req("REQ-OPS-005")
def test_every_central_key_documented():
    example = (REPO / "deploy" / "central.yaml.example").read_text()
    reference = (REPO / "docs" / "configuration.md").read_text()
    for key in config.documented_keys():
        leaf = key.split(".")[-1]
        assert re.search(rf"^\s*{leaf}:", example, re.M), f"{key} missing in central.yaml.example"
        assert f"`{key}`" in reference, f"{key} missing in docs/configuration.md"
    loaded = config.load(str(REPO / "deploy" / "central.yaml.example"))
    assert loaded.raw == config.load(None).raw, "the example shows the defaults"


@pytest.mark.req("REQ-OPS-006")
def test_documentation_files_and_headings():
    required = {
        "architecture.md": ["# Architecture", "## Ownership attribution", "## Storage"],
        "install.md": ["# Installation", "## Install central", "## Install an agent", "## Add a server"],
        "configuration.md": ["# Configuration reference", "## Agent (agent.yaml)", "## Central (central.yaml)"],
        "firewall.md": ["# Firewall", "## Rules on the central host", "## Ingest path"],
        "operations.md": ["# Operations", "## Export", "## Retention", "## Troubleshooting"],
        "manual-verification.md": ["# Manual verification", "## Requirement status", "## Checklist"],
        "user-policy.md": ["# GPUMon user policy for lab members", "## Name multipass VMs <username>-<anything>",
                           "## Label rootful Docker containers with --label owner=<username>",
                           "## Use sudo from your own account instead of logging in as root"],
    }
    for name, headings in required.items():
        text = (REPO / "docs" / name).read_text()
        lines = text.splitlines()
        for h in headings:
            assert h in lines, f"{name}: missing heading {h!r}"
    policy = (REPO / "docs" / "user-policy.md").read_text()
    assert "--label owner=" in policy and "loginuid" in policy and "root-shared" in policy
    fw = (REPO / "docs" / "firewall.md").read_text()
    for rule in ("ufw allow from <INTERNAL_SUBNET> to any port 8080", "ufw allow from <VPN_POOL> to any port 8080",
                 "ufw deny 8080/tcp", "ufw default deny incoming"):
        assert rule in fw, rule
    manual = (REPO / "docs" / "manual-verification.md").read_text()
    for n in range(1, 11):
        assert f"### {n}. " in manual
    readme = (REPO / "README.md").read_text()
    for a in range(1, 14):
        assert f"| A{a} |" in readme
    assert len(readme.splitlines()) <= 80, "the README stays under one page"


@pytest.mark.req("REQ-OPS-003")
def test_export_timer_units():
    svc = unit("gpumon-export.service")
    for want in ("Type=oneshot", "User=gpumon", "NoNewPrivileges=yes", "ProtectSystem=strict",
                 "ReadWritePaths=/var/lib/gpumon", "PrivateTmp=yes", "ProtectHome=yes",
                 "ExecStart=/opt/gpumon/venv/bin/gpumon auto-export --config /etc/gpumon/central.yaml"):
        assert want in svc, want
    timer = unit("gpumon-export.timer")
    assert "Persistent=true" in timer and "OnCalendar=*-*-* 04:30:00 Asia/Taipei" in timer, \
        "the timer must fire at 04:30 in the dashboard time zone, not in the host's (often UTC) system zone"
    assert "WantedBy=timers.target" in timer


@pytest.mark.req("REQ-OPS-003")
def test_install_central_auto_export_choice():
    script = str(REPO / "deploy" / "install-central.sh")

    def dry(*extra, stdin=subprocess.DEVNULL):
        r = subprocess.run([script, "--dry-run", *extra], capture_output=True, text=True, stdin=stdin)
        assert r.returncode == 0, r.stderr
        return r.stdout

    out = dry("--auto-export", "yes")
    for unit_file in ("gpumon-export.service", "gpumon-export.timer"):
        # The host running the tests may already have GPUMon installed.
        path = f"/etc/systemd/system/{unit_file}"
        assert f"installed {path}" in out or f"unit is up to date: {path}" in out, unit_file
    assert "DRY-RUN: systemctl enable --now gpumon-export.timer" in out
    out = dry()
    if not os.path.exists("/etc/systemd/system/gpumon-export.timer"):
        assert "skipping the daily CSV export timer (pass --auto-export yes to install it)" in out
        assert "gpumon-export.timer" not in out.replace("pass --auto-export yes", "")
    out = dry("--auto-export", "no")
    assert "enable --now gpumon-export.timer" not in out
    r = subprocess.run([script, "--dry-run", "--auto-export", "maybe"], capture_output=True, text=True)
    assert r.returncode == 2 and "--auto-export must be yes or no" in r.stderr
    # Custom locations reach the installed units.
    text = (REPO / "deploy" / "install-central.sh").read_text()
    assert 's|/opt/gpumon|$PREFIX|g' in text and 's|/var/lib/gpumon|$DATA_DIR|g' in text
    assert 's|Asia/Taipei|$TZ_NAME|g' in text
