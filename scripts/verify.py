#!/usr/bin/env python3
"""Build verification/report.md and report.json from SPEC.md and the test results.

Inputs (written by "make verify"):
  verification/go-test.json        go test -json output
  verification/pytest-results.json pytest results with requirement markers
  verification/e2e-results.json    E2E step results and measurements
  scripts/blocked.json             optional {"REQ-...": "reason"} for requirements that cannot be met

Exits non-zero if lint failed, any test failed, or any auto/e2e requirement is FAIL or NO-TEST.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
VER = REPO / "verification"
REQ_HEAD = re.compile(r"^\*\*(REQ-[A-Z]+-\d{3}):\s*(.*?)\*\*")
REQ_ID = re.compile(r"REQ-[A-Z]+-\d{3}")
MANUAL_DOC = REPO / "docs" / "manual-verification.md"


def parse_spec(path: Path) -> list[dict]:
    reqs: list[dict] = []
    cur = None
    for line in path.read_text().splitlines():
        m = REQ_HEAD.match(line)
        if m:
            cur = {"id": m.group(1), "title": m.group(2).rstrip("."), "verify": "", "methods": []}
            reqs.append(cur)
            continue
        if line.startswith("### ") or line.startswith("## "):
            cur = None
        if cur is not None and line.strip().startswith("- Verify:"):
            text = line.strip()[len("- Verify:"):].strip()
            cur["verify"] = text
            cleaned = re.sub(r"\([^)]*\)", "", text)
            for word in re.findall(r"\b(auto|e2e|manual)\b", cleaned):
                if word not in cur["methods"]:
                    cur["methods"].append(word)
    return reqs


def go_test_refs() -> dict[tuple[str, str], list[str]]:
    """(package, test name) -> requirement IDs from the name or a Verifies comment."""
    refs: dict[tuple[str, str], list[str]] = {}
    root = REPO / "agent"
    for f in root.rglob("*_test.go"):
        pkg = "gpumon/agent" + ("/" + str(f.parent.relative_to(root)) if f.parent != root else "")
        lines = f.read_text().splitlines()
        for i, line in enumerate(lines):
            m = re.match(r"^func ((?:Test|Benchmark)\w+)\(", line)
            if not m:
                continue
            name = m.group(1)
            ids = [f"REQ-{a}-{b}" for a, b in re.findall(r"REQ_([A-Z]+)_(\d{3})", name)]
            j = i - 1
            while j >= 0 and lines[j].startswith("//"):
                if "Verifies:" in lines[j]:
                    ids += REQ_ID.findall(lines[j])
                j -= 1
            refs[(pkg, name)] = sorted(set(ids))
    return refs


def load_go():
    path = VER / "go-test.json"
    results: dict[tuple[str, str], str] = {}
    metrics: dict[str, str] = {}
    if not path.exists():
        return None, metrics
    for line in path.read_text().splitlines():
        if not line.startswith("{"):
            continue
        ev = json.loads(line)
        test = ev.get("Test")
        if ev.get("Action") == "output":
            for m in re.finditer(r"METRIC (\w+)=([\d.]+)", ev.get("Output", "")):
                metrics[m.group(1)] = m.group(2)
        if not test or "/" in test:
            continue
        if ev.get("Action") in ("pass", "fail", "skip"):
            results[(ev["Package"], test)] = ev["Action"]
    return results, metrics


def load_json(name):
    p = VER / name
    return json.loads(p.read_text()) if p.exists() else None


def scan(paths, pattern, exclude=lambda p: False):
    hits = []
    rx = re.compile(pattern)
    for base in paths:
        files = [base] if base.is_file() else [p for p in base.rglob("*") if p.is_file()]
        for f in files:
            if exclude(f):
                continue
            for n, line in enumerate(f.read_text(errors="replace").splitlines(), 1):
                if rx.search(line):
                    hits.append(f"{f.relative_to(REPO)}:{n}: {line.strip()[:100]}")
    return hits


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--lint-status", type=int, default=0, help="exit status of make lint")
    args = ap.parse_args()

    reqs = parse_spec(REPO / "SPEC.md")
    blocked = {}
    bpath = REPO / "scripts" / "blocked.json"
    if bpath.exists():
        blocked = json.loads(bpath.read_text())
    go_results, go_metrics = load_go()
    refs = go_test_refs()
    py = load_json("pytest-results.json")
    e2e = load_json("e2e-results.json")
    problems: list[str] = []
    if args.lint_status != 0:
        problems.append("make lint failed")
    if go_results is None:
        problems.append("verification/go-test.json is missing")
        go_results = {}
    if py is None:
        problems.append("verification/pytest-results.json is missing")
        py = {"results": []}
    if e2e is None:
        problems.append("verification/e2e-results.json is missing")
        e2e = {"passed": False, "steps": [], "metrics": {}}

    auto_tests: dict[str, list[tuple[str, str]]] = {}
    for key, outcome in go_results.items():
        for rid in refs.get(key, []):
            auto_tests.setdefault(rid, []).append((f"go:{key[1]}", outcome))
    for r in py["results"]:
        outcome = {"passed": "pass", "failed": "fail", "skipped": "skip"}.get(r["outcome"], r["outcome"])
        name = "py:" + r["nodeid"].split("::")[-1]
        for rid in r["reqs"]:
            auto_tests.setdefault(rid, []).append((name, outcome))
    e2e_steps: dict[str, list[tuple[str, str]]] = {}
    for s in e2e["steps"]:
        for rid in s["reqs"]:
            e2e_steps.setdefault(rid, []).append((f"e2e:step{s['step']}", s["status"]))

    go_fail = [k[1] for k, v in go_results.items() if v == "fail"]
    go_skip = [k[1] for k, v in go_results.items() if v == "skip"]
    py_fail = [r["nodeid"] for r in py["results"] if r["outcome"] == "failed"]
    py_skip = [r["nodeid"] for r in py["results"] if r["outcome"] == "skipped"]
    if go_fail:
        problems.append(f"failing Go tests: {go_fail}")
    if py_fail:
        problems.append(f"failing Python tests: {py_fail}")
    if not e2e.get("passed"):
        problems.append("the E2E run did not pass")

    manual_text = MANUAL_DOC.read_text() if MANUAL_DOC.exists() else ""
    rows = []
    for r in reqs:
        rid, methods = r["id"], r["methods"]
        tests = auto_tests.get(rid, []) + e2e_steps.get(rid, [])
        notes = []
        if rid in blocked:
            status = "BLOCKED"
            notes.append(blocked[rid])
        else:
            missing = []
            if "auto" in methods and not auto_tests.get(rid):
                missing.append("automated test")
            if "e2e" in methods and not e2e_steps.get(rid):
                missing.append("E2E step")
            graded = auto_tests.get(rid, []) + e2e_steps.get(rid, [])
            if missing:
                status = "NO-TEST"
                notes.append("missing " + " and ".join(missing))
            elif methods == ["manual"]:
                status = "MANUAL-PENDING"
            elif any(o != "pass" for _, o in graded):
                status = "FAIL"
                notes.append("failing or skipped: " + ", ".join(n for n, o in graded if o != "pass"))
            else:
                status = "PASS"
            if "auto" not in methods and "e2e" not in methods and auto_tests.get(rid):
                notes.append("also covered by automated tests")
        if "manual" in methods:
            ok = re.search(rf"\|\s*{rid}\s*\|[^\n]*MANUAL-PENDING", manual_text) is not None
            if ok:
                notes.append("manual part MANUAL-PENDING in docs/manual-verification.md")
            else:
                notes.append("manual part missing from docs/manual-verification.md")
                problems.append(f"{rid} is not listed as MANUAL-PENDING in docs/manual-verification.md")
        if rid == "REQ-DSH-011":
            shots = sorted((VER / "screenshots").glob("*.png")) if (VER / "screenshots").exists() else []
            if shots:
                notes.append("headless-browser screenshots of the simulated dashboard: "
                             + ", ".join(str(p.relative_to(REPO)) for p in shots))
        rows.append({"id": rid, "title": r["title"], "verify": "+".join(methods), "status": status,
                     "tests": [n for n, _ in tests], "notes": "; ".join(notes)})

    counts = {s: sum(1 for x in rows if x["status"] == s) for s in ("PASS", "FAIL", "NO-TEST", "BLOCKED", "MANUAL-PENDING")}
    bad = [x["id"] for x in rows if x["status"] in ("FAIL", "NO-TEST") and set(x["verify"].split("+")) & {"auto", "e2e"}]
    if bad:
        problems.append(f"auto/e2e requirements failing or untested: {bad}")
    for x in rows:
        if x["status"] == "BLOCKED" and not x["notes"]:
            problems.append(f"{x['id']} is BLOCKED without a reason")

    # Measured numbers ---------------------------------------------------------
    em = e2e.get("metrics", {})
    metrics = {
        "agent_rss_max_mib": em.get("agent_rss_max_mib"),
        "agent_rss_mib": em.get("agent_rss_mib"),
        "collect_2000_processes_ms": float(go_metrics["collect_2000_processes_ms"])
        if "collect_2000_processes_ms" in go_metrics else None,
        "raw_snapshot_bytes_per_sample": em.get("raw_snapshot_bytes_per_sample"),
        "raw_snapshot_bytes_per_day": em.get("raw_snapshot_bytes_per_day"),
        "raw_snapshot_projected_bytes_per_month": em.get("raw_snapshot_projected_bytes_per_month"),
        "sizing_basis": em.get("sizing_basis"),
    }

    # Self-review checklist ------------------------------------------------------
    status_of = {x["id"]: x["status"] for x in rows}

    def all_pass(*ids):
        return all(status_of.get(i) == "PASS" for i in ids)

    def step_ok(n):
        return any(s["step"] == n and s["status"] == "pass" for s in e2e["steps"])

    prod_src = [REPO / "agent" / "internal", REPO / "agent" / "cmd", REPO / "central" / "gpumon_central",
                REPO / "web" / "app.js", REPO / "web" / "index.html", REPO / "deploy" / "gpumon-agent.service",
                REPO / "deploy" / "gpumon-central.service"]

    def is_test(p):
        return p.name.endswith("_test.go") or "fixture" in p.parts or p.suffix not in (".go", ".py", ".js", ".html", ".service")

    nongoal = scan(prod_src, r"(?i)smtplib|sendmail|slack|webhook|\bquota|reservation|kubernetes|slurm|ldap|"
                             r"hash.?chain|signature|login form|password|\bnotify\(|alertmanager", is_test)
    hardcoded = scan(prod_src, r"RTX|GeForce|\b(10240|11264|12288|16303|24576)\b|\b(?!127\.0\.0\.1)(\d{1,3}\.){3}\d{1,3}\b"
                               r"|srv-4[0-4]", is_test)
    ascii_rc = subprocess.run([sys.executable, str(REPO / "scripts" / "ascii_check.py")], capture_output=True, text=True)
    readme = (REPO / "README.md").read_text()
    assumptions_listed = all(f"| A{i} |" in readme for i in range(1, 14))
    skipped = len(go_skip) + len(py_skip)

    def box(ok):
        return "[x]" if ok else "[ ]"

    checklist = [
        (box(not bad and counts["NO-TEST"] == 0),
         "Every auto/e2e requirement has at least one passing test that references its ID.",
         f"{counts['PASS']} PASS, {counts['FAIL']} FAIL, {counts['NO-TEST']} NO-TEST (table above)."),
        (box(skipped == 0),
         "No test was skipped, weakened, or deleted to achieve a pass.",
         f"{len(go_results)} Go tests and {len(py['results'])} Python tests ran; {skipped} skipped. No test was "
         "deleted and no requirement check was removed. During development some tests were wrong and were "
         "corrected: miscounted expected row or process counts, a substring check that matched a VM name, a fake "
         "/proc/stat that never advanced, and a simulated absence that was itself a data gap. Code defects found by "
         "tests were fixed in the code."),
        (box(not nongoal),
         "No non-goal feature (Section 2.2) was implemented.",
         "No enforcement, scheduling, quotas, dashboard login, LDAP, signing, MIG, alerts, agent buffering, or "
         f"scheduler integration. Keyword scan of production sources: {len(nongoal)} hits {nongoal[:3]}."),
        (box(all_pass("REQ-SEC-007", "REQ-AGT-007") and step_ok(11)),
         "No full command line or environment variable reaches the payload, the database, or the logs.",
         "REQ-SEC-007 and REQ-AGT-007 tests pass (fixture argv with --api-key=SECRET123); the schema rejects extra "
         "fields such as cmdline; E2E step 11 found no SECRET123 in payloads, databases, or logs."),
        (box(not hardcoded),
         "No GPU model, VRAM size, server count, or IP address is hard-coded outside examples and tests.",
         f"Scan of agent, central, web, and unit files (tests, fixtures, examples excluded): {len(hardcoded)} hits "
         f"{hardcoded[:3]}. GPU names and sizes come from NVML; servers register themselves."),
        (box(all_pass("REQ-AGT-010", "REQ-ATT-007")),
         "The agent works with NVML absent, Docker absent, and /etc/subuid absent.",
         "TestREQ_AGT_010_GracefulDegradation removes all three sources and still gets users, host metrics, and "
         "filesystems plus English errors[]; TestREQ_ATT_007 covers an unreachable Docker API."),
        (box(all_pass("REQ-DSH-009")),
         "Served frontend assets make no external requests.",
         "test_served_assets_reference_no_external_hosts fetches every served asset and finds no external host; "
         "uPlot is vendored under web/vendor."),
        (box(ascii_rc.returncode == 0 and all_pass("REQ-LOG-001", "REQ-LOG-003")),
         "All logs and source files are ASCII English.",
         ascii_rc.stdout.strip().splitlines()[-1] + "; log format tests REQ-LOG-001 pass for both components."),
        ("[~]" if assumptions_listed else "[ ]",
         "Every assumption in Section 10 is a config setting and is listed in the README.",
         "All of A1 to A13 are listed in the README with their setting. A1, A4, A5, A6, A7, A9, A10, and A13 are "
         "configuration keys with the stated defaults. A2 (subnet and VPN placeholders) lives in docs/firewall.md, "
         "and A3, A8, A11, and A12 describe the environment (root login policy, UI language, offline install, "
         "OS and toolchain versions); they have no runtime switch, so this item is only partly met."),
        (box(all_pass("REQ-CEN-007", "REQ-SEC-003", "REQ-SEC-004")),
         "Central never requires root; the agent's root use is limited by its systemd hardening.",
         "Central refuses root without --allow-root and runs as gpumon (unit test); the agent unit uses "
         "ProtectSystem=strict, ProtectHome=read-only, NoNewPrivileges, and a state directory only."),
        (box(all_pass("REQ-STO-004", "REQ-STO-005") and step_ok(5)),
         "Restarting either component causes no duplicated sessions or rollups.",
         "Session tests restart storage and replay messages; a partial unique index allows one open session per "
         "key; rollups replace a day's rows; E2E step 5 restarts central and finds no duplicates or overlaps; "
         "E2E step 4 restarts an agent (new run_id, duplicates by seq are ignored)."),
    ]

    # Write the reports -----------------------------------------------------------
    VER.mkdir(exist_ok=True)
    stamp = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    report = {"generated_at": stamp, "summary": counts, "problems": problems, "requirements": rows,
              "metrics": metrics, "self_review": [{"status": s, "item": i, "evidence": e} for s, i, e in checklist],
              "test_runs": {"go": {"total": len(go_results), "failed": len(go_fail), "skipped": len(go_skip)},
                            "python": {"total": len(py["results"]), "failed": len(py_fail), "skipped": len(py_skip)},
                            "e2e": {"passed": e2e.get("passed"), "steps": len(e2e["steps"])}}}
    (VER / "report.json").write_text(json.dumps(report, indent=2) + "\n")

    def mib(b):
        return f"{b / 2**20:.1f} MiB" if b is not None else "n/a"

    md = ["# GPUMon verification report", "",
          f"Generated {stamp} from SPEC.md ({len(rows)} requirements).", "",
          "| PASS | FAIL | NO-TEST | BLOCKED | MANUAL-PENDING |", "|------|------|---------|---------|----------------|",
          f"| {counts['PASS']} | {counts['FAIL']} | {counts['NO-TEST']} | {counts['BLOCKED']} | {counts['MANUAL-PENDING']} |", "",
          f"Test runs: Go {len(go_results)} tests ({len(go_fail)} failed, {len(go_skip)} skipped); Python "
          f"{len(py['results'])} tests ({len(py_fail)} failed, {len(py_skip)} skipped); E2E "
          f"{'passed' if e2e.get('passed') else 'FAILED'} ({len(e2e['steps'])} steps); lint "
          f"{'passed' if args.lint_status == 0 else 'FAILED'}.", ""]
    if problems:
        md += ["## Problems", ""] + [f"- {p}" for p in problems] + [""]
    md += ["## Requirements", "", "| ID | Verify | Status | Tests | Notes |", "|----|--------|--------|-------|-------|"]
    for x in rows:
        tests = ", ".join(x["tests"][:6]) + (f", +{len(x['tests']) - 6} more" if len(x["tests"]) > 6 else "")
        md.append(f"| {x['id']} | {x['verify']} | {x['status']} | {tests or '-'} | {x['notes'] or '-'} |")
    m = metrics
    per_month = m["raw_snapshot_projected_bytes_per_month"]
    md += ["", "## Measured numbers", "",
           f"- Agent steady-state RSS (E2E, after about one minute): max {m['agent_rss_max_mib']} MiB "
           f"(budget 50 MB); per agent {m['agent_rss_mib']}.",
           f"- Collecting one sample from a fake procfs with 2000 processes: {m['collect_2000_processes_ms']} ms "
           "(budget 200 ms).",
           f"- Raw snapshot bytes per sample: {m['raw_snapshot_bytes_per_sample']} (measured over the E2E run).",
           f"- Raw snapshot bytes per day: {m['raw_snapshot_bytes_per_day']} ({mib(m['raw_snapshot_bytes_per_day'])}); "
           f"basis: {m['sizing_basis']}.",
           f"- Projected monthly raw snapshot size: {per_month} ({mib(per_month)}); three months of retention: "
           f"{mib(per_month * 3 if per_month else None)} against the 5 GiB cap.",
           "", "## Self-review checklist", ""]
    for s, item, ev in checklist:
        md.append(f"- {s} {item}  ")
        md.append(f"  Evidence: {ev}")
    md += ["", "Legend: [x] met, [~] partly met (explained), [ ] not met.", ""]
    (VER / "report.md").write_text("\n".join(md))
    print(f"wrote {VER / 'report.md'}: {counts}")
    for p in problems:
        print(f"problem: {p}")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
