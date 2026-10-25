# Operations

## Services

```
systemctl status gpumon-central            # on the central host
systemctl status gpumon-agent              # on every server
journalctl -u gpumon-central -f            # JSON lines
journalctl -u gpumon-agent -f              # logfmt lines
sudo systemctl reload gpumon-central       # re-read servers.json (SIGHUP)
```

Repeated identical warnings and errors are logged at most once per hour (`log_rate_limit_minutes`,
`error_log_interval_minutes`); the next line reports how many were suppressed.

## Export

The dashboard and the public API only show the last 72 hours. Older audit data is read with the export
command on the central host, as the `gpumon` user:

```
sudo -u gpumon /opt/gpumon/venv/bin/gpumon export --config /etc/gpumon/central.yaml \
    --kind sessions --from 2026-09-01 --to 2026-10-01 --out /tmp/sessions-2026-09.csv
```

| Kind | Rows | Source |
|------|------|--------|
| `snapshots` | one per sample and owner (GPU presence and CPU/memory) plus host rows | raw files (last 3 months) |
| `sessions` | GPU sessions overlapping the range | `audit.db` (permanent) |
| `rollups` | daily GPU-hours, VRAM GiB-hours, CPU core-hours, memory GiB-hours, peak RSS | `audit.db` (permanent) |
| `gaps` | data gaps with their English description | `audit.db` (permanent) |
| `disk` | per-owner disk scan results | `audit.db` (permanent) |

Options: `--server ID`, `--owner NAME`. Times are ISO 8601 (`2026-10-25T14:00:00Z`), a date (local
midnight), or epoch seconds; values without a zone use the configured `timezone`. Every CSV is UTF-8 with a
header row and has both a UTC column (`*_utc`) and a local-time column (`*_local`).

## Automatic daily export

Optionally, a systemd timer exports every local day (in `timezone`) once it has completely left the 72-hour
public window, so the records end up in CSV files without anyone running the command. Choose it during
installation (`install-central.sh` asks, or pass `--auto-export yes`; `--auto-export no` removes it again):

```
sudo ./install-central.sh --auto-export yes
systemctl list-timers gpumon-export.timer          # next run: daily at about 04:30 in `timezone` (Asia/Taipei)
sudo systemctl start gpumon-export.service         # run it now
journalctl -u gpumon-export.service
ls -l /var/lib/gpumon/exports/
```

Each run writes `<kind>-YYYY-MM-DD.csv` for every kind in `auto_export.kinds` (default `sessions`, `rollups`,
`gaps`, `disk`) to `auto_export.dir` (default `/var/lib/gpumon/exports`, mode 0750, files 0640, owner
`gpumon`). The timer runs at 04:30 in the configured `timezone` (default Asia/Taipei, that is 20:30 UTC),
whatever the system time zone of the host is; the installer copies the zone from `central.yaml` into the timer,
so run it again after changing `timezone`. The newest exported day is the day before the one that contains "now minus 72 hours", so on
2026-10-25 at 04:30 Taipei time the run writes 2026-10-21. Existing files are never overwritten; the last
`auto_export.catch_up_days` days (default 7) are checked on every run, so days missed while the host was down
are written later, and the first run after installation exports up to a week of backlog. Session files contain
every session that overlaps the day, so a session crossing midnight appears in both days. Add `snapshots` to
`auto_export.kinds` for the raw per-sample records (much larger files). The same command can be run by hand:

```
sudo -u gpumon /opt/gpumon/venv/bin/gpumon auto-export --config /etc/gpumon/central.yaml
sudo -u gpumon /opt/gpumon/venv/bin/gpumon auto-export --config /etc/gpumon/central.yaml --date 2026-10-01   # rewrite one day
```

The exported files are not deleted automatically; archive or remove them as your audit policy requires.

## Retention

Maintenance runs hourly inside central and can be run by hand:

```
sudo -u gpumon /opt/gpumon/venv/bin/gpumon maintenance --config /etc/gpumon/central.yaml --rollup-days 2
```

- Raw snapshot files `snapshots-YYYY-MM.db` of the last `raw_retention_months` months (current month
  included) are kept; older files are deleted.
- If the files together exceed `raw_max_total_bytes`, the oldest non-current files are deleted until the
  total fits. The current month's file is never deleted; if it alone is too large, an error is logged.
- Every deletion is logged and stored as a `raw_snapshot_deleted` event.
- `audit.db` (sessions, rollups, gaps, events, inventory history, disk scans) is never pruned.
- Rollups of today and yesterday are recomputed each run; recomputing a day replaces its rows.

Sizing: the E2E run measures raw snapshot bytes per day for 4 servers, 5 GPUs, and 20 users and writes the
projection to `verification/report.md`. After one week in production, compare it with
`du -b /var/lib/gpumon/snapshots-*.db` and adjust the retention settings if needed.

Backup: copy `audit.db` with `sqlite3 /var/lib/gpumon/audit.db ".backup /backup/audit.db"`.

## Troubleshooting

| Symptom | Check |
|---------|-------|
| Server shows `offline` | `systemctl status gpumon-agent` on it; `journalctl -u gpumon-agent` for "push to central failed"; firewall rules. |
| Agent log says HTTP 401 | The token on the agent differs from `/etc/gpumon/token` on central. |
| Agent refuses to start: token permissions | `sudo chmod 600 /etc/gpumon/token`. |
| GPU section empty, error "NVML library not found" | NVIDIA driver not installed or not loaded; `nvidia-smi` must work. |
| Many `unattributed` processes | Rootful containers without the `owner` label and no `docker.default_owner`, or VMs not named `<user>-...`; see `gpumon-agent diagnose` and [user-policy.md](user-policy.md). |
| `root-shared` usage | Someone logs in as root directly; ask them to use sudo (assumption A3). |
| Clock skew warning | `timedatectl` on the agent host; NTP must be synchronized. |
| Shaded gap in the charts | Hover it for the reason: agent offline or unreachable, or central offline. |
| Disk chart says "no per-owner scan yet" | The scan runs daily at `disk.scan_time_local`; set `disk.scan_on_start: true` to scan at start. |
| Disk scan marked partial | The scan hit `disk.scan_timeout_minutes`; raise it or scan fewer paths. |
| Central refuses to start as root | Run it as `gpumon` through systemd (or pass `--allow-root`, not recommended). |
