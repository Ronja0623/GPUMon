# GPUMon

Internal, read-only usage view for a group's GPU servers: where capacity is free, who uses what, and who used
what (internal audit). A Go agent on every server pushes samples to a Python central service that stores them
in SQLite and serves a dashboard. Any number of servers is supported. Details: [docs/](docs/); the original
specification is [SPEC.md](SPEC.md).

In the commands below, `<CENTRAL_HOST>` is the IP address or host name of the machine that runs central: one
of your GPU servers or a small VM that all servers can reach. Read "Before you start" in
[docs/install.md](docs/install.md) for the few settings to decide first.

## Install central (once)

```
make package                                   # on a build machine; copy dist/ to the servers
tar xzf gpumon-central-0.1.0.tar.gz && cd gpumon-central-0.1.0
sudo ./install-central.sh --listen <CENTRAL_HOST>:8080   # idempotent; --dry-run shows the actions
```

It creates the `gpumon` user, `/opt/gpumon/venv`, `/var/lib/gpumon`, `/etc/gpumon/central.yaml`, a token in
`/etc/gpumon/token`, and the `gpumon-central` service. It asks whether to install a daily timer that exports
records older than 72 hours to CSV (`--auto-export yes|no` skips the question). Then apply
[docs/firewall.md](docs/firewall.md).

## Install an agent / add a server

Copy `/etc/gpumon/token` from the central host to the server, then:

```
tar xzf gpumon-agent-0.1.0-linux-amd64.tar.gz && cd gpumon-agent-0.1.0-linux-amd64
sudo ./install-agent.sh --central-url http://<CENTRAL_HOST>:8080/api/v1/ingest --token-file ./token
sudo gpumon-agent diagnose                     # shows what the agent detects and why
```

That is all a new server needs: it registers itself on its first sample. Optional names, order, and
retirement go in `/etc/gpumon/servers.json` (no restart needed).

## Export audit data

The dashboard shows 72 hours. Older data is exported on the central host:

```
sudo -u gpumon /opt/gpumon/venv/bin/gpumon export --config /etc/gpumon/central.yaml \
    --kind sessions --from 2026-09-01 --to 2026-10-01 [--server gpu-02] [--owner alice] --out sessions.csv
```

Kinds: `snapshots`, `sessions`, `rollups`, `gaps`, `disk`. With the optional timer, each day that leaves the
72-hour window is written automatically to `/var/lib/gpumon/exports/<kind>-YYYY-MM-DD.csv`. See
[docs/operations.md](docs/operations.md).

## Defaults and site-specific settings

IDs refer to the assumptions in Section 10 of SPEC.md.

| ID | Default or assumption | Where to change it |
|----|-----------------------|--------------------|
| A1 | Server addresses and names are site-specific | agent `central_url`, `server_id`; nothing on central |
| A2 | Internal subnet and VPN pool are site-specific | `<INTERNAL_SUBNET>`, `<VPN_POOL>` in docs/firewall.md |
| A3 | Direct root login may stay enabled; shown as `root-shared` | attribution rule 5; docs/user-policy.md |
| A4 | VMs named `<username>-<anything>` | agent `multipass.name_patterns` |
| A5 | Rootful containers use label `owner`; default owner per host | agent `docker.owner_label`, `docker.default_owner` |
| A6 | Disk scan of `/home` daily at 03:30 | agent `disk.scan_paths`, `disk.scan_time_local` |
| A7 | Session gap tolerance 5 min | central `session_gap_tolerance_minutes` |
| A8 | Dashboard in English | (fixed) |
| A9 | Time zone Asia/Taipei | central `timezone` |
| A10 | Central on one of the servers, as a non-root user | central `listen`; unit user `gpumon` |
| A11 | No internet on servers | `make package` bundles the binary and wheels |
| A12 | Ubuntu 22.04/24.04, Python 3.10+, Go 1.22+ | build targets |
| A13 | Raw snapshots 3 months, 5 GiB cap; sessions and rollups forever | central `raw_retention_months`, `raw_max_total_bytes` |

## Development

`make build | test | lint | e2e | verify | package`. `make verify` runs everything and writes
`verification/report.md`. Requires Go 1.22+ with cgo, Python 3.10+, and network access for dependencies.
See [CONTRIBUTING.md](CONTRIBUTING.md) for the branch and commit conventions.
