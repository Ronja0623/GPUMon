# Architecture

GPUMon answers three questions for the users of a group of GPU servers: where is GPU capacity free, who is
using what right now, and who used what in the past. It is an internal usage view, not an enforcement or compliance tool.

```
 every GPU server                                 central host (one server or a small VM)
 +---------------------------+  HTTP POST JSON    +------------------------------------+
 | gpumon-agent (Go, root)   | -----------------> | gpumon-central (Python, user gpumon)|
 | no listening sockets      |  Bearer token      |  ingest API, SQLite, dashboard,    |
 +---------------------------+  every 30 s        |  export and maintenance CLI        |
                                                  +------------------------------------+
                                                        ^ browsers on the internal network and VPN
```

## Components

**gpumon-agent** (`agent/`, Go) runs as root on every server, including the central host. Every
`interval_seconds` it reads `/proc`, NVML (`libnvidia-ml.so`, loaded at runtime), the rootful Docker API
(GET requests over the unix socket), `/etc/passwd`, `/etc/subuid`, and `statfs`, attributes every process to
an owner, and posts one JSON sample to central. It also sends a hardware inventory at start, every 6 hours,
and on change, and the result of a daily per-owner disk scan. Every source is behind an interface with a
fake implementation, so the agent is tested without hardware and degrades gracefully: a missing source adds
an English message to `errors[]` and everything else is still reported. Failed pushes are dropped; outages
become gaps on central.

**gpumon-central** (`central/`, Python 3.10+, Starlette and uvicorn) validates each message against
`schema/payload.schema.json`, stores it, and serves the dashboard and a small JSON API. It never runs as root.

**Dashboard** (`web/`) is static HTML, CSS, and JavaScript with the vendored uPlot chart library. It makes
no external requests.

## Ownership attribution

Attribution is computed on the agent; central stores the owner, the method, and the context unchanged. The
first matching rule wins:

1. Rootful Docker: a container ID in `/proc/<pid>/cgroup` (cgroup v1 or v2, outside a per-user service).
   Owner from the `owner` label, else `docker.default_owner`, else `unattributed`.
2. multipass VM: a `qemu-system-*` process under `multipassd` or with the multipass data directory in its
   argv. The instance name comes from the argv; its prefix before `-` must be a local user.
3. subuid: the UID lies in a `/etc/subuid` range (non-root processes in rootless containers).
4. sudo or su: UID 0 with a valid, non-zero loginuid.
5. Direct root login: UID 0 with loginuid 0, reported as `root-shared`.
6. System: UID 0 without loginuid, or a UID below `uid_min`, reported as `system`.
7. Regular user: the passwd name of the UID, or `uid:<n>` when there is none.

Identity across servers is always the username, never the UID. Kernel threads are ignored. Command lines and
environment variables are never sent or stored; only `gpumon-agent diagnose` prints a QEMU argv locally.

## Storage

All files live in `data_dir` (default `/var/lib/gpumon`):

| File | Content | Retention |
|------|---------|-----------|
| `audit.db` | servers, inventory history, GPU sessions, daily rollups, disk scans, gaps, events, heartbeat | permanent |
| `snapshots-YYYY-MM.db` | compressed raw messages plus normalized time series (UTC month, WAL mode) | 3 months, 5 GiB cap |

Live GPU states (`free`, `idle_occupied`, `busy`, `unknown`) are computed from the latest sample and the
`idle_window`. GPU sessions are updated on every sample; a restart of central neither loses open sessions nor
duplicates them. Daily rollups are recomputed hourly for today and yesterday and are idempotent.

## Gaps and time

Central writes a heartbeat every 60 s. Samples more than 3 intervals apart produce an
`agent_offline_or_unreachable` gap; a central restart produces `central_offline` gaps from the last heartbeat.
Every gap has an English description. Central's receive time is used for all time series; the agent timestamp
is stored next to it and the difference is shown as clock skew.

## Adding or removing a server

Install the agent with the central URL and the token. The server registers itself on its first message. A
`servers.json` file can rename, sort, annotate, or retire servers without a restart.
