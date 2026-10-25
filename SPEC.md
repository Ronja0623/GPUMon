# GPUMon: Build Specification and Self-Verification Prompt

> Give this whole document to a coding agent as its task. It is written so that the agent can build the system, test it without real hardware, and produce a machine-checkable verification report.

---

## 0. Instructions to the coding agent

You are an autonomous coding agent. Build the system described in this document inside the current repository, then prove that it meets every requirement.

Working rules:

1. **Traceability.** Every requirement has an ID (`REQ-XXX-NNN`). Every requirement marked `Verify: auto` MUST be covered by at least one automated test that references its ID:
   - Go: put the ID in the test name (`TestREQ_ATT_002_SudoLoginuid`) or in a `// Verifies: REQ-ATT-002` comment directly above the test.
   - Python: use a pytest marker, `@pytest.mark.req("REQ-CEN-004")`.
2. **Integrity.** Never weaken, skip, or delete a test to make it pass. Never edit this specification. If a requirement truly cannot be met, mark it `BLOCKED` in the report with a precise English explanation, then continue with the rest.
3. **Iterate.** Work in small steps: implement, test, run `make verify`, fix. Stop only when the Definition of Done (Section 11) holds.
4. **No hardware assumed.** Your machine may have no NVIDIA GPU, no Docker daemon, no multipass, and no root. All access to NVML, `/proc`, `/etc/passwd`, `/etc/subuid`, the Docker API, the filesystem scanner, and the clock MUST go through interfaces that have fake implementations for tests.
5. **English and ASCII only.** All code, comments, identifiers, log messages, error messages, commit messages, UI text, and documentation are in English. Source files and log output are ASCII-only, except for vendored third-party files.
6. **Offline at runtime.** The only runtime network traffic is agent-to-central. Do not use CDNs; vendor all frontend assets.
7. **Simplicity.** Choose the simplest design that satisfies the requirements. Do not implement anything listed as a non-goal (Section 2.2).
8. **Assumptions.** Section 10 lists assumptions made on behalf of the owner. Implement each one as a configurable setting with the stated default, and list them all in the README.

---

## 1. Context

A university lab runs four Ubuntu GPU servers on one internal network. Lab members need a shared, internal-only web tool that answers three questions: where is compute capacity free, who is using what, and who used what in the past (internal audit, not compliance).

### 1.1 Server inventory (see ASSUMPTION A1)

| Server ID | IP address        | GPUs                                 | Architecture |
|-----------|-------------------|--------------------------------------|--------------|
| srv-40    | `<PREFIX>.40`     | 1 x NVIDIA GeForce RTX 3080          | Ampere       |
| srv-41    | `<PREFIX>.41`     | 2 x NVIDIA GeForce RTX 2080 Ti       | Turing       |
| srv-42    | `<PREFIX>.42`     | 1 x NVIDIA GeForce RTX 3080 Ti       | Ampere       |
| srv-43    | `<PREFIX>.43`     | 1 x NVIDIA GeForce RTX 5070 Ti       | Blackwell    |

That is 5 GPUs in total. GPU models, VRAM sizes, and driver versions MUST be auto-detected and never hard-coded. The RTX 5070 Ti requires a recent driver branch, so driver versions may differ between servers.

### 1.2 Environment facts

- OS: Ubuntu Server (22.04 or 24.04, see A12).
- Time sync: NTP is installed on all servers.
- GPU compute mode is `Default`, so several processes from different users MAY share one GPU at the same time.
- All GPUs are GeForce cards: there is no MIG, and some NVML queries return `NOT_SUPPORTED` (for example, per-process utilization).
- Accounts are local on each server, with no LDAP. The same person has the same username on every server, but **UIDs may differ between servers**.
- A shared `root` account exists. Some people may log in as root directly over SSH; others use `sudo`.
- Docker: only 2 or 3 people may use it. All of them use **rootless Docker** except one person, who uses **rootful Docker**.
- multipass: users create VMs (QEMU backend). The QEMU processes run as `root` under the `multipassd` daemon. The VMs do not use GPUs, but they consume CPU and memory.
- Remote users reach the internal network through a VPN, which assigns them addresses from a separate pool.
- The number of servers will change over time.

---

## 2. Goals and non-goals

### 2.1 Goals
- **G1:** A live view of GPU, CPU, memory, and disk usage on every server.
- **G2:** For every resource in use, show which user is using it and how that ownership was determined.
- **G3:** Keep audit records: raw snapshots for a few months, plus usage sessions and daily rollups permanently.
- **G4:** Adding or removing a server needs no code change and no change on the central server.

### 2.2 Non-goals (do NOT implement)
- Enforcement, scheduling, reservations, quotas, or usage policies.
- Login or authentication for the dashboard.
- LDAP or any central account management.
- Tamper-proofing, signing, or hash chains.
- MIG support.
- Alerts or notifications.
- Agent-side buffering or replay during outages. Outages are recorded as gaps instead.
- Kubernetes, Slurm, or other schedulers.

---

## 3. Architecture

```
 srv-40 .. srv-43 (and future servers)              central host (one of the servers, A10)
 +--------------------------+   HTTP POST JSON      +-----------------------------------+
 | gpumon-agent (Go, root)  | --------------------> | gpumon-central (Python, non-root) |
 | no listening ports       |   Bearer token        |  - ingest API                     |
 +--------------------------+   every 30 s          |  - SQLite storage                 |
                                                    |  - dashboard (static HTML/JS)     |
                                                    |  - CLI: export, maintenance       |
                                                    +-----------------------------------+
                                                         ^  browser (internal subnet + VPN pool)
```

- **gpumon-agent**: written in Go, version 1.22 or later. Uses `github.com/NVIDIA/go-nvml`, which loads `libnvidia-ml.so` at runtime; the agent MUST start and keep working when NVML is absent. It ships as a single linux/amd64 binary. It runs on every server, including the central host.
- **gpumon-central**: written in Python 3.10 or later. Uses `sqlite3` from the standard library and one small pinned web stack (FastAPI or Starlette, with uvicorn). No ORM is required.
- **Dashboard**: static HTML, CSS, and JS served by gpumon-central, with a vendored chart library. Either no build step, or the build output is committed.
- **Contract**: the payload format is defined once in JSON Schema under `schema/`. Both sides validate against it in tests.

---

## 4. Requirements

Each requirement has a **Verify** method:
- `auto`: an automated test proves it.
- `e2e`: proved by the end-to-end simulation (Section 8.3), which is also automated.
- `manual`: needs real servers; it goes into the manual checklist (Section 8.5).

### 4.1 Agent: data collection (AGT)

**REQ-AGT-001: Sampling loop.**
- The agent takes one sample every `interval`; the default is 30 s and it is configurable.
- Each sample is pushed to central immediately.
- Each sample carries:
  - a UTC RFC 3339 timestamp with millisecond precision
  - a `run_id` (random, generated at agent start)
  - a sequence number that increases by one per sample within a run.
- Verify: auto.

**REQ-AGT-002: GPU device metrics.**
- For each GPU, keyed by UUID, report:
  - index, UUID, name
  - VRAM total and used, in MiB
  - utilization %, temperature, power draw, power limit, fan %.
- Any field that NVML reports as `NOT_SUPPORTED` or that errors is `null`. A failure on one field MUST NOT drop the other fields.
- Verify: auto.

**REQ-AGT-003: GPU processes.**
- For each GPU, list compute and graphics processes with:
  - PID
  - process type (`compute` or `graphics`)
  - used VRAM in MiB (`null` if unsupported)
  - executable name
  - attributed owner and attribution method (Section 4.2)
  - container name and image, or VM name, when applicable.
- Verify: auto.

**REQ-AGT-004: No fabricated numbers.**
- Per-process or per-user GPU utilization is included only when NVML actually provides it.
- When a GPU is shared, device utilization is reported for the device only. It is never split across users by estimation.
- Verify: auto.

**REQ-AGT-005: Host CPU and memory.**
- Report: total CPU % (computed from `/proc/stat` deltas), logical core count, load averages, memory total, used, and available, and swap total and used.
- Verify: auto.

**REQ-AGT-006: Per-user CPU and memory.**
- Aggregate all non-kernel processes by attributed owner. For each owner, report:
  - CPU usage in cores, computed from `utime + stime` deltas between samples (2.5 means 2.5 cores)
  - total RSS in bytes
  - process count.
- Verify: auto.

**REQ-AGT-007: Top processes per owner.**
- For each owner, report up to N processes (default 5) by CPU and up to N by RSS.
- Each entry has: PID, executable name, CPU cores, RSS, and container or VM name when applicable.
- **Never** include the full command line or environment variables.
- Verify: auto.

**REQ-AGT-008: Hardware inventory.**
- Report:
  - hostname, OS release, kernel version
  - CPU model, logical cores, total RAM
  - NVIDIA driver version, CUDA driver version
  - the GPU list (index, UUID, name, total VRAM)
  - the monitored filesystems.
- Send the inventory at startup, every 6 h, and whenever it changes.
- Verify: auto.

**REQ-AGT-009: Disk usage.**
- (a) Every sample includes `statfs` totals (size, used, free) for the configured mount points.
- (b) A per-owner usage scan runs once a day; the default time is 03:30 local and it is configurable. It scans the configured paths, default `["/home"]`. The scan must:
  - run at the lowest CPU and IO priority
  - not cross filesystem boundaries
  - not follow symlinks
  - count each hard link only once
  - sum allocated blocks, not apparent size
  - map file owners through the same UID, subuid, and username logic as Section 4.2 (an unknown UID becomes `uid:<n>`)
  - stop at a timeout (default 2 h) and report the partial result with `partial=true`.
- (c) The latest scan result and its completion time are sent to central and shown on the dashboard as "as of <time>".
- Verify: auto. Use a fixture directory tree with hard links, symlinks, and multiple owners through a fake stat layer.

**REQ-AGT-010: Graceful degradation.**
- If NVML, the Docker API, `/etc/subuid`, or any other source is unavailable, the agent keeps reporting everything else.
- It includes an `errors[]` array of English messages in the payload.
- It logs each distinct error at most once per hour.
- Verify: auto.

**REQ-AGT-011: Push.**
- The agent sends an HTTP POST of JSON to the central URL with the header `Authorization: Bearer <token>`. The token is read from a file. The request timeout is 5 s.
- On failure, the agent logs a warning (rate-limited) and drops the sample. There is no buffering.
- Each payload includes a `schema_version` field.
- Verify: auto.

**REQ-AGT-012: Resource budget.**
- Collecting one sample from a fake procfs with 2000 processes completes in under 200 ms (benchmark test).
- Steady-state RSS is under 50 MB; measure it in the E2E run and record it in the report.
- Verify: auto and e2e.

**REQ-AGT-013: Diagnose command.**
- `gpumon-agent diagnose` prints everything the agent detects, in English, to the local terminal. For every GPU process and every process with UID 0 that uses more than 1 % CPU, it prints:
  - the attribution chain and the evidence for it (UID, loginuid, cgroup path, container ID and labels, subuid match)
  - for QEMU processes, the raw argv, so the multipass detection can be checked by hand.
- This is the only place where a full argv may appear. It is never sent to central.
- Verify: auto (output contains the expected sections when run against fakes) and manual.

**REQ-AGT-014: Simulation mode.**
- `gpumon-agent simulate --inventory <file> --server-id <id>` generates synthetic samples for E2E tests. It covers these scenarios:
  - a free GPU
  - a busy GPU
  - an idle-occupied GPU
  - a GPU shared by two users
  - a rootful Docker container with an owner label
  - a rootful Docker container without a label
  - a rootless Docker process
  - a multipass VM
  - a `sudo` process
  - a direct-root-login process
  - an unknown UID.
- Verify: e2e.

### 4.2 Attribution (ATT)

**REQ-ATT-001: Output fields.**
- Every reported process has three fields:
  - `owner`: a username or a special value
  - `attribution`: one of `uid`, `loginuid`, `subuid`, `docker_label`, `docker_default_owner`, `multipass_name`, `system`, `root_shared`, `unattributed`
  - optional context: `container_name`, `container_image`, `vm_name`.
- The special owner values are:
  - `root-shared`: someone logged in as root directly
  - `system`: a daemon or a system account
  - `unattributed`: the owner cannot be determined
  - `uid:<n>`: a UID with no passwd entry.
- Verify: auto.

**REQ-ATT-002: Precedence.** The first matching rule wins.

1. **Rootful Docker.** If `/proc/<pid>/cgroup` contains a Docker container ID (support both cgroup v1 and v2 path formats, such as `/docker/<id>` and `docker-<id>.scope`):
   - query the Docker Engine API over the unix socket, using GET requests only
   - owner = the value of the label key (default `owner`), method `docker_label`
   - otherwise owner = the per-host config `docker_default_owner`, method `docker_default_owner`
   - otherwise `unattributed`.
2. **multipass VM.** If the executable matches `qemu-system-*` AND (the parent chain includes `multipassd` OR the argv contains the multipass data directory):
   - extract the instance name from the argv using a configurable list of regexes. The defaults cover the instance image path segment `/instances/<name>/` and `-name <name>`.
   - owner = the part of the name before the first `-`, if that is an existing local username; method `multipass_name`
   - otherwise `unattributed`.
   - In both cases, keep `vm_name`.
3. **subuid.** If the UID falls inside a range in `/etc/subuid`, owner = that range's owner, method `subuid`. This covers non-root processes inside rootless containers.
4. **sudo or su.** If UID = 0 and `/proc/<pid>/loginuid` is valid (not 4294967295) and not 0, owner = the username of the loginuid, method `loginuid`.
5. **Direct root login.** If UID = 0 and loginuid = 0, owner = `root-shared`, method `root_shared`.
6. **System.** If UID = 0 and loginuid is unset, or UID < `uid_min` (default 1000, from `/etc/login.defs` when present), owner = `system`, method `system`. Keep the real account name in context.
7. **Regular user.** Otherwise, owner = the username of the UID, method `uid`. This covers direct runs and rootless Docker container root, which maps to the user's own UID.

- Verify: auto. There must be one fixture-based test per rule, plus tests for rule-order conflicts.

**REQ-ATT-003: Identity is the username.**
- Identity across servers is always the username, never the UID.
- UIDs are resolved through the host's own passwd lookup.
- Verify: auto. Include a fixture where the same name has different UIDs on two servers.

**REQ-ATT-004: Kernel threads excluded.**
- Kernel threads are excluded: use the PF_KTHREAD flag in `/proc/<pid>/stat`, or treat PPID 2 or PID 2 as a kernel thread.
- Verify: auto.

**REQ-ATT-005: subuid formats.**
- `/etc/subuid` parsing supports both the `name:start:count` and the `uid:start:count` forms.
- Malformed lines are skipped with a warning.
- Verify: auto.

**REQ-ATT-006: Attribution happens on the agent.**
- Attribution is computed on the agent.
- Central stores the owner, the method, and the context fields unchanged.
- Verify: auto (contract test).

**REQ-ATT-007: Docker API failure.**
- If the Docker API is unreachable, rule 1 falls back to `docker_default_owner` or `unattributed`.
- The agent logs a rate-limited English warning.
- Verify: auto.

### 4.3 Central: ingestion, registry, and status (CEN)

**REQ-CEN-001: Ingest endpoint.**
- `POST /api/v1/ingest` validates the payload against the schema.
- It returns 401 for a missing or wrong token, using a constant-time comparison.
- It returns 400 for a malformed payload, with an English error body.
- It stores the receive time next to the agent's timestamp.
- Verify: auto.

**REQ-CEN-002: Auto-registration and overrides.**
- An unknown `server_id` that presents a valid token is registered automatically.
- Inventory messages update the stored inventory, and inventory history is kept.
- An optional `servers.json` overrides `display_name`, `notes`, `sort_order`, and `retired`.
- Changes to `servers.json` are picked up without a restart, either on mtime change or on SIGHUP.
- Verify: auto and e2e.

**REQ-CEN-003: Online and offline status.**
- A server is `online` if its last sample is no older than 3 x interval; otherwise it is `offline`.
- Retired servers are hidden from the live view, but their history is kept.
- Verify: auto.

**REQ-CEN-004: Gaps.**
- Central writes a heartbeat to the database every 60 s.
- If two consecutive samples from a server are more than 3 x interval apart while central was running, store a gap with reason `agent_offline_or_unreachable`.
- On startup, store a gap with reason `central_offline` for each known server, covering the time from the last heartbeat to now.
- A change of `run_id` is stored as the event `agent_restarted`.
- Every gap has an English description, for example:
  `DATA GAP: no samples from srv-41 between 2026-10-25T14:02:00Z and 2026-10-25T14:37:30Z (agent offline or unreachable).`
- Verify: auto and e2e.

**REQ-CEN-005: Clock skew.**
- If |agent timestamp - receive time| > 5 s (configurable), central logs a rate-limited English warning and shows the skew on the dashboard.
- Both timestamps are stored.
- Verify: auto.

**REQ-CEN-006: GPU state classification.**
- Each GPU in each sample gets exactly one state:
  - `unknown`: no recent data
  - `free`: no compute process, ignoring names in `ignore_processes` (default `Xorg`, `Xwayland`, `gnome-shell`), and used VRAM <= `free_vram_mib` (default 512)
  - `idle_occupied`: at least one compute process has been present for the entire `idle_window` (default 30 min), and mean utilization over that window is below `idle_util_pct` (default 5)
  - `busy`: any other case with a compute process present.
- All thresholds are configurable.
- Verify: auto.

**REQ-CEN-007: Bind address and privileges.**
- Central binds to the configured address and port. The default is `127.0.0.1:8080`; production binds to the internal IP.
- It runs as a non-root user and refuses to start as root unless `--allow-root` is given.
- Verify: auto.

**REQ-CEN-008: Export CLI.**
- `gpumon export --kind {snapshots|sessions|rollups|gaps|disk} --from <time> --to <time> [--server <id>] [--owner <name>] --out <file.csv>` exports data from any time range.
- The CSV has a header row and UTF-8 encoding, with both a UTC ISO 8601 column and a local-time column in the configured timezone.
- This CLI is the only way to read history older than the public window.
- Verify: auto.

### 4.4 Storage and retention (STO)

**REQ-STO-001: Raw snapshot files.**
- Raw snapshots are stored in monthly SQLite files named `snapshots-YYYY-MM.db` (UTC month), with WAL mode enabled.
- Verify: auto.

**REQ-STO-002: Retention.**
- Keep the monthly files of the last `raw_retention_months` months, including the current month (default 3), and delete older ones.
- If the total size exceeds `raw_max_total_bytes` (default 5 GiB), delete the oldest non-current files until the total is under the limit.
- Never delete the current month's file. If it alone exceeds the limit, log an English error.
- Every deletion is logged and stored as an event.
- Verify: auto, using a fake clock and fake file sizes.

**REQ-STO-003: Permanent audit database.**
- `audit.db` holds servers, inventory history, GPU sessions, daily rollups, disk scan results, gaps, and events.
- It is never pruned automatically.
- Verify: auto.

**REQ-STO-004: GPU sessions.**
- A session is the continuous presence of one owner on one GPU (server, GPU UUID, owner).
- A session closes when the owner has been absent longer than `session_gap_tolerance` (default 5 min).
- If a data gap interrupts a session, the session closes at the last seen sample and is flagged `ended_by_data_gap`.
- Each session stores:
  - start, end, duration
  - peak and average VRAM
  - average device utilization while present
  - `shared` (true if another owner was on the same GPU at the same time)
  - the attribution methods seen
  - up to 10 distinct executable, container, or VM names.
- Open sessions are updated incrementally. Restarting central must not create duplicate sessions or lose open ones.
- Verify: auto.

**REQ-STO-005: Daily rollups.**
- For each (local date in the configured timezone, server, owner), store:
  - GPU-hours
  - VRAM GiB-hours
  - CPU core-hours
  - memory GiB-hours
  - peak RSS.
- Recomputing the same day is idempotent.
- Verify: auto.

**REQ-STO-006: Sizing report.**
- The E2E run measures bytes per day of raw snapshots for the real inventory (4 servers, 5 GPUs) with 20 simulated users.
- It writes the measured value and the projected monthly size to the report.
- Verify: e2e.

### 4.5 Dashboard (DSH)

**REQ-DSH-001: Single page.**
- One page at `/`, with no login.
- Data refreshes every 15 s (configurable) without reloading the page.
- Verify: auto (served page and API) and manual (visual check).

**REQ-DSH-002: Where can I run?**
- A summary at the top lists every GPU across all servers.
- GPUs are ordered by state (`free` first, then by free VRAM descending), showing server, GPU name, free VRAM / total VRAM, and current owners.
- Verify: auto (API ordering).

**REQ-DSH-003: Server cards.**
- One card per server shows:
  - display name
  - online or offline status, with last-seen time
  - clock-skew warning
  - CPU %, load, memory used / total, swap
  - the server's GPUs.
- Verify: auto (API fields) and manual.

**REQ-DSH-004: GPU detail.**
- Each GPU shows:
  - name and state badge (state shown as text, not only color)
  - utilization %
  - a VRAM bar
  - each owner with their VRAM, attribution method, and container or VM name
  - the executable names.
- Verify: auto (API fields) and manual.

**REQ-DSH-005: Per-user CPU and memory.**
- Each server has a table of owners showing cores used, RSS, process count, and attribution method.
- Special owners (`root-shared`, `system`, `unattributed`) are visually distinct.
- Verify: auto (API) and manual.

**REQ-DSH-006: Disk chart.**
- One horizontal stacked bar per monitored filesystem.
- Segments show the top N owners (default 8), then "others", "unscanned/other usage" (statfs used minus the scanned total), and "free".
- Segment labels show human-readable sizes.
- The chart shows "as of <scan completion time>" and a partial-scan warning when it applies.
- Verify: auto (API) and manual.

**REQ-DSH-007: History.**
- The page shows the last `public_history_hours` hours (default 72):
  - per-GPU utilization and VRAM, stacked by owner
  - per-server CPU and memory, stacked by owner.
- Gaps are shown as shaded regions with their English description on hover.
- Verify: auto (API) and manual.

**REQ-DSH-008: Server-side history limit.**
- Every public API endpoint rejects ranges that start before now - `public_history_hours`, with HTTP 403 and an English message.
- Verify: auto.

**REQ-DSH-009: Offline-safe page.**
- The served HTML, CSS, and JS contain no references to external hosts.
- Verify: auto (scan served assets for `http://` or `https://` URLs not on the same origin).

**REQ-DSH-010: Language and time.**
- The UI is in English (A8).
- Times are shown in the configured timezone (default `Asia/Taipei`, A9), with the zone label.
- Verify: auto.

**REQ-DSH-011: Layout.**
- The page is usable at 1280 px wide and readable at 375 px wide.
- Verify: manual (screenshot if a headless browser is available).

### 4.6 Security and privacy (SEC)

**REQ-SEC-001: No listening sockets on the agent.**
- The agent opens no listening sockets.
- Verify: auto (static check that the agent module calls no `net.Listen` or `http.ListenAndServe`) and e2e.

**REQ-SEC-002: Token handling.**
- The token is read from a file. The agent and central both refuse to start if the file's permissions are wider than 0600.
- The token is never logged.
- Verify: auto.

**REQ-SEC-003: Central systemd unit.**
- Runs as user `gpumon`, with `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ReadWritePaths=<data dir>`, `PrivateTmp=yes`, and `ProtectHome=yes`.
- Verify: auto (unit file content test) and manual.

**REQ-SEC-004: Agent systemd unit.**
- Runs as root, because it reads every process's `/proc` entries, the Docker socket, and every user's files for the disk scan.
- Hardening: `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=read-only`, `PrivateTmp=yes`, `ProtectKernelTunables=yes`, `ProtectKernelModules=yes`, `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`, and no writable paths beyond an optional state directory.
- Verify: auto (unit file content test) and manual.

**REQ-SEC-005: Docker API is read-only.**
- The Docker client only issues GET requests.
- Verify: auto (fake transport asserts the HTTP method).

**REQ-SEC-006: Firewall documentation.**
- The docs give `ufw` rules that:
  - allow the dashboard port only from `<INTERNAL_SUBNET>` and `<VPN_POOL>`
  - allow the ingest path only from the server IPs (same port, so the subnet rule covers it)
  - deny everything else.
- Verify: manual.

**REQ-SEC-007: Privacy.**
- Full command lines and environment variables are never sent or stored.
- Verify: auto. A fixture process whose argv contains `--api-key=SECRET123` must not leak that string into the payload, the database, or the logs.

### 4.7 Logging (LOG)

**REQ-LOG-001: Format.**
- All logs are English, ASCII-only, and structured (logfmt or JSON lines) on stdout or stderr, for journald.
- Verify: auto.

**REQ-LOG-002: Levels and rate limiting.**
- Levels are `debug`, `info`, `warn`, and `error`, configurable.
- Repeated identical warnings and errors are rate-limited.
- Verify: auto.

**REQ-LOG-003: ASCII source check.**
- A check script fails if any non-vendored source file, template, or doc contains non-ASCII characters.
- Verify: auto.

### 4.8 Operations and documentation (OPS)

**REQ-OPS-001: Makefile.**
- The Makefile has the targets `build`, `test`, `lint` (`go vet`, `gofmt` check, `ruff` or `flake8`), `e2e`, `verify`, and `package`.
- Verify: auto.

**REQ-OPS-002: Agent package.**
- The agent package contains:
  - the linux/amd64 binary
  - the systemd unit
  - an example config
  - an idempotent `install-agent.sh`.
- Verify: auto (package contents test).

**REQ-OPS-003: Central package.**
- An idempotent `install-central.sh` creates the `gpumon` user, the venv, the data directory, and the systemd unit.
- Verify: auto (script lint and dry-run mode) and manual.

**REQ-OPS-004: Adding a server.**
- Adding a server only requires installing the agent with the central URL and the token.
- Verify: e2e. A fifth simulated agent joins mid-run and appears in the API.

**REQ-OPS-005: Configuration.**
- Agent and central configs are YAML, with every key documented and defaulted.
- An invalid config makes the program refuse to start with an English error that names the key.
- Verify: auto.

**REQ-OPS-006: Documentation.** The `docs/` folder contains:
- architecture
- install
- configuration reference
- firewall
- operations (export, retention, troubleshooting)
- manual verification (Section 8.5)
- a **user policy note** for lab members, explaining:
  - multipass VMs must be named `<username>-<anything>`
  - rootful Docker containers should use `--label owner=<username>`
  - use `sudo` from your own account instead of logging in as root, and why this makes usage attributable.
- Verify: auto (files exist and contain the required headings).

---

## 5. Configuration defaults (summary)

```yaml
# agent.yaml
server_id: ""                 # empty = use hostname
central_url: "http://<CENTRAL_IP>:8080/api/v1/ingest"
token_file: /etc/gpumon/token
interval_seconds: 30
top_processes_per_owner: 5
uid_min: 1000
docker:
  socket: /var/run/docker.sock
  owner_label: owner
  default_owner: ""           # set on the host with rootful Docker (A5)
multipass:
  name_patterns: ['/instances/([^/]+)/', '-name\s+(\S+)']
disk:
  statfs_mounts: ["/", "/home"]
  scan_paths: ["/home"]
  scan_time_local: "03:30"
  scan_timeout_minutes: 120
log_level: info
```

```yaml
# central.yaml
listen: "127.0.0.1:8080"
data_dir: /var/lib/gpumon
token_file: /etc/gpumon/token
servers_file: /etc/gpumon/servers.json   # optional
timezone: Asia/Taipei
expected_interval_seconds: 30
offline_after_intervals: 3
clock_skew_warn_seconds: 5
public_history_hours: 72
raw_retention_months: 3
raw_max_total_bytes: 5368709120
session_gap_tolerance_minutes: 5
gpu_state:
  free_vram_mib: 512
  idle_window_minutes: 30
  idle_util_pct: 5
  ignore_processes: [Xorg, Xwayland, gnome-shell]
disk_top_owners: 8
log_level: info
```

---

## 6. Suggested repository layout

```
agent/                 Go module (cmd/gpumon-agent, internal/{nvml,procfs,attrib,docker,disk,push,sim})
agent/testdata/        procfs, cgroup, subuid, passwd, docker JSON, qemu argv fixtures
central/               Python package (gpumon_central: api, storage, sessions, rollups, cli)
central/tests/
web/                   dashboard static files (vendored libs under web/vendor/)
schema/                payload JSON Schema + example payloads
deploy/                systemd units, install scripts, example configs, servers.json example
scripts/               e2e.sh, verify.py, ascii_check.py
docs/
verification/          generated reports (git-ignored except .gitkeep)
Makefile
README.md
```

---

## 7. Payload contract

- Define `schema/payload.schema.json` (sample, inventory, and disk-scan message types, each with `schema_version`).
- Go tests marshal fake samples and validate them against the schema.
- Python tests validate the same example files in `schema/examples/`.
- If one side changes the payload, at least one test must fail on the other side.

---

## 8. Testing and verification

### 8.1 Test layers
1. **Unit tests (Go and Python)**: all logic, with fakes for every OS and hardware interface and a fake clock.
2. **Contract tests**: the shared JSON Schema and example payloads.
3. **E2E simulation**: real binaries and processes on localhost, with simulated data.
4. **Manual checks on real servers**: a written checklist.

### 8.2 Required fixtures (under `agent/testdata/`)
- cgroup v1 and cgroup v2 paths for a rootful Docker container; Docker inspect JSON with and without an `owner` label
- a rootless Docker process with the user's own UID, and one with a subuid
- `/etc/subuid` in both line formats, including a malformed line
- a process with UID 0 and loginuid 1001 (sudo); one with UID 0 and loginuid 0 (direct root login); one with UID 0 and loginuid unset (daemon)
- multipass QEMU argv variants: one where the name is extractable, and one with a name prefix that is not a valid user
- kernel threads
- an unknown UID with no passwd entry
- the same username with different UIDs on two simulated servers
- NVML: `NOT_SUPPORTED` on fan speed and on per-process utilization; one GPU with two owners
- a process whose argv contains `--api-key=SECRET123`.

### 8.3 E2E scenario (`make e2e`, runs `scripts/e2e.sh`)
Use accelerated settings: `interval = 1 s`, `idle_window = 20 s`, `session_gap_tolerance = 3 s`. Then:

1. Start central on `127.0.0.1` with a temporary data directory.
2. Start four `gpumon-agent simulate` instances matching the inventory in Section 1.1, covering every scenario in REQ-AGT-014.
3. Assert through the API:
   - all four servers are online
   - five GPUs are listed with the correct names
   - each scenario gets the expected state and owner
   - the summary order is correct.
4. Kill srv-41's agent for 5 s, then restart it. Assert that a gap with the English description exists and that an `agent_restarted` event exists.
5. Stop central for 5 s, then restart it. Assert that a `central_offline` gap exists and that no sessions were duplicated.
6. Start a fifth simulated agent (`srv-44`). Assert that it auto-registers.
7. Request history older than the public window. Assert HTTP 403.
8. Run `gpumon export` for every kind. Assert that the CSV headers and row counts are correct.
9. Assert that no agent process has a listening socket.
10. Record agent RSS and raw-snapshot bytes per day (REQ-AGT-012, REQ-STO-006).

### 8.4 Verification report (`make verify`)
`make verify` runs lint, unit tests, contract tests, and E2E, then `scripts/verify.py`, which:

- parses this document for every `REQ-...` ID and its Verify method
- collects the test results (`go test -json`, pytest JSON or JUnit output, E2E step results)
- writes `verification/report.md` and `verification/report.json` with one row per requirement:
  - ID
  - Verify method
  - status: `PASS`, `FAIL`, `BLOCKED`, `MANUAL-PENDING`, or `NO-TEST`
  - the test names
  - notes
- appends the measured numbers (agent RSS, collection time, bytes per day, projected monthly size)
- appends the completed self-review checklist (Section 9)
- **exits non-zero** if any `auto` or `e2e` requirement is `FAIL` or `NO-TEST`.

### 8.5 Manual verification checklist (generate as `docs/manual-verification.md`)
For the owner to run on the real servers. Each item has the exact commands and the expected result:

1. On each server, run `gpumon-agent diagnose`. Compare the GPUs with `nvidia-smi -L` and the processes with `nvidia-smi`.
2. Confirm the QEMU argv shown by `diagnose` lets multipass names be extracted. If not, adjust `multipass.name_patterns`.
3. Run a rootful Docker container with `--label owner=<user>` and another without the label. Check the attribution.
4. Run a rootless Docker GPU job. Check the attribution.
5. Run `sudo python3 -c "while True: pass"` from a personal account. Check that it is attributed via `loginuid`.
6. Check `timedatectl` on all servers, and check the skew column on the dashboard.
7. From a host outside the subnet and the VPN pool, confirm the dashboard is unreachable. From the VPN, confirm it is reachable.
8. Run `systemctl status` for both units and `systemd-analyze security gpumon-agent`.
9. Check the dashboard layout on a laptop and on a phone.
10. After one week, compare the real bytes per day with the E2E estimate and adjust retention if needed.

---

## 9. Self-review checklist (answer each item in the report, with evidence)

- [ ] Every `auto`/`e2e` requirement has at least one passing test that references its ID.
- [ ] No test was skipped, weakened, or deleted to achieve a pass.
- [ ] No non-goal feature (Section 2.2) was implemented.
- [ ] No full command line or environment variable reaches the payload, the database, or the logs.
- [ ] No GPU model, VRAM size, server count, or IP address is hard-coded outside examples and tests.
- [ ] The agent works with NVML absent, Docker absent, and `/etc/subuid` absent.
- [ ] Served frontend assets make no external requests.
- [ ] All logs and source files are ASCII English.
- [ ] Every assumption in Section 10 is a config setting and is listed in the README.
- [ ] Central never requires root; the agent's root use is limited by its systemd hardening.
- [ ] Restarting either component causes no duplicated sessions or rollups.

---

## 10. Assumptions made on behalf of the owner (all configurable)

| ID  | Assumption |
|-----|------------|
| A1  | The servers' IPs end in .40 to .43, and GPUs map to servers in the order listed in Section 1.1. The prefix is unknown; use the placeholder `<PREFIX>`. |
| A2  | The internal subnet and the VPN address pool are unknown; use the placeholders `<INTERNAL_SUBNET>` and `<VPN_POOL>`. |
| A3  | Direct root login may stay enabled. Such usage shows as `root-shared`, and the docs recommend disabling it. |
| A4  | multipass VMs are named `<username>-<anything>`. |
| A5  | Rootful Docker containers use the label `owner`. Unlabeled ones fall back to `docker_default_owner`, which is set on the host where the rootful Docker user works. |
| A6  | The disk scan covers `/home` daily at 03:30 local time. Other paths are added by config. |
| A7  | The session gap tolerance is 5 minutes. |
| A8  | The dashboard UI is in English. |
| A9  | The timezone is `Asia/Taipei`. |
| A10 | Central runs on one of the four servers (examples use srv-40), as a non-root user. |
| A11 | The servers may lack internet access. Runtime needs no internet, and build artifacts can be copied in. |
| A12 | Target Ubuntu 22.04 and 24.04, Python 3.10 or later, Go 1.22 or later. |
| A13 | Raw snapshots are kept for 3 months with a 5 GiB cap; sessions and rollups are kept permanently. |

---

## 11. Definition of Done

All of the following hold:

1. `make verify` exits 0.
2. `verification/report.md` shows no `FAIL` or `NO-TEST`, and every `BLOCKED` item has a written reason.
3. Every `manual` requirement appears in `docs/manual-verification.md` as `MANUAL-PENDING`, with exact commands.
4. The self-review checklist is answered with evidence.
5. The README explains, in under one page, how to install central, install an agent, add a server, and export audit data.
