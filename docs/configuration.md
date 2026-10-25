# Configuration reference

Both configuration files are YAML. Every key is optional and has the default shown here. Unknown keys and
invalid values make the program refuse to start with an English error that names the key, for example
`config error: key "disk.scan_time_local": must be a 24-hour time "HH:MM"`. Annotated examples:
`deploy/agent.yaml.example` and `deploy/central.yaml.example`.

## Agent (agent.yaml)

Path: `/etc/gpumon/agent.yaml` (`gpumon-agent run --config PATH`).

| Key | Default | Meaning |
|-----|---------|---------|
| `server_id` | `""` | Identifier on the dashboard; empty means the host name. |
| `central_url` | `http://127.0.0.1:8080/api/v1/ingest` | Ingest endpoint of central. |
| `token_file` | `/etc/gpumon/token` | Shared bearer token; refused if wider than 0600. |
| `interval_seconds` | `30` | Seconds between samples; each sample is pushed immediately. |
| `top_processes_per_owner` | `5` | Top processes per owner by CPU and by RSS. |
| `uid_min` | `UID_MIN` from `/etc/login.defs`, else `1000` | UIDs below this are `system`. |
| `inventory_interval_hours` | `6` | Hours between unconditional inventory messages. |
| `push_timeout_seconds` | `5` | HTTP request timeout of a push. |
| `error_log_interval_minutes` | `60` | Minimum minutes between identical warning or error lines. |
| `docker.enabled` | `true` | Query the rootful Docker API for container owners. |
| `docker.socket` | `/var/run/docker.sock` | Unix socket of the rootful daemon (GET requests only). |
| `docker.owner_label` | `owner` | Container label that names the owner (A5). |
| `docker.default_owner` | `""` | Owner of unlabeled rootful containers on this host (A5). |
| `multipass.name_patterns` | `['/instances/([^/]+)/', '-name\s+(\S+)']` | Regexes for the VM name in the QEMU argv. |
| `multipass.data_dirs` | snap and deb data directories | A QEMU argv containing one of these is multipass. |
| `disk.statfs_mounts` | `["/", "/home"]` | Mount points reported in every sample. |
| `disk.scan_paths` | `["/home"]` | Paths of the daily per-owner scan (A6). |
| `disk.scan_time_local` | `"03:30"` | Local time of the daily scan. |
| `disk.scan_timeout_minutes` | `120` | The scan stops and reports `partial=true` after this. |
| `disk.scan_on_start` | `false` | Also scan once right after start. |
| `log_level` | `info` | `debug`, `info`, `warn`, or `error`. |

## Central (central.yaml)

Path: `/etc/gpumon/central.yaml` (`gpumon serve --config PATH`). Check a file with
`gpumon check-config --config PATH`.

| Key | Default | Meaning |
|-----|---------|---------|
| `listen` | `127.0.0.1:8080` | Bind address; production uses the central host's internal address (A10). |
| `data_dir` | `/var/lib/gpumon` | Databases. |
| `token_file` | `/etc/gpumon/token` | Shared bearer token; refused if wider than 0600. |
| `servers_file` | `/etc/gpumon/servers.json` | Optional display overrides, reloaded on change or SIGHUP. |
| `timezone` | `Asia/Taipei` | Your IANA time zone: dashboard times, rollup days, exports, export timer (A9). |
| `expected_interval_seconds` | `30` | Interval assumed until a server reports its own. |
| `offline_after_intervals` | `3` | Offline status and data gaps after this many intervals. |
| `clock_skew_warn_seconds` | `5` | Skew warning threshold. |
| `public_history_hours` | `72` | History window of the public API and the dashboard. |
| `raw_retention_months` | `3` | Monthly raw snapshot files kept, current month included (A13). |
| `raw_max_total_bytes` | `5368709120` | Size cap of the raw snapshot files (A13). |
| `session_gap_tolerance_minutes` | `5` | Absence that ends a GPU session (A7). |
| `gpu_state.free_vram_mib` | `512` | Max used VRAM of a free GPU. |
| `gpu_state.idle_window_minutes` | `30` | Window for `idle_occupied`. |
| `gpu_state.idle_util_pct` | `5` | Mean utilization below which an occupied GPU is idle. |
| `gpu_state.ignore_processes` | `[Xorg, Xwayland, gnome-shell]` | Never count as GPU users. |
| `disk_top_owners` | `8` | Owners shown individually in each disk bar. |
| `auto_export.dir` | `""` (`<data_dir>/exports`) | Output directory of the daily automatic export. |
| `auto_export.kinds` | `[sessions, rollups, gaps, disk]` | Kinds written per day; `snapshots` is also allowed (large). |
| `auto_export.catch_up_days` | `7` | Past days checked on each run; missing files are written. |
| `refresh_seconds` | `15` | Dashboard refresh interval. |
| `heartbeat_seconds` | `60` | Heartbeat interval used for `central_offline` gaps. |
| `maintenance_interval_minutes` | `60` | Retention and rollup interval. |
| `log_rate_limit_minutes` | `60` | Minimum minutes between identical warning or error lines. |
| `max_body_bytes` | `4194304` | Largest accepted ingest request. |
| `web_dir` | auto | Dashboard files (the installer sets `/opt/gpumon/share/web`). |
| `schema_file` | auto | Payload schema (the installer sets `/opt/gpumon/share/schema/payload.schema.json`). |
| `log_level` | `info` | `debug`, `info`, `warn`, or `error`. |

## servers.json

```
{"servers": {"gpu-01": {"display_name": "gpu-01 (2 GPUs)", "notes": "also runs central", "sort_order": 1, "retired": false}}}
```

Allowed keys per server: `display_name`, `notes`, `sort_order`, `retired`. An invalid file is logged and the
previous overrides stay active.
