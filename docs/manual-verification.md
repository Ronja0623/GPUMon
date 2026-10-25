# Manual verification

These checks need the real servers. Run them after installation and record the result of each item. Every
requirement with a manual part is listed below with status MANUAL-PENDING until the owner signs it off.

## Requirement status

| Requirement | Manual part | Checklist items | Status |
|-------------|-------------|-----------------|--------|
| REQ-AGT-013 | diagnose output on real servers | 1, 2 | MANUAL-PENDING |
| REQ-DSH-001 | visual check of the single page and its refresh | 9, 11 | MANUAL-PENDING |
| REQ-DSH-003 | server cards | 9, 11 | MANUAL-PENDING |
| REQ-DSH-004 | GPU detail | 3, 4, 11 | MANUAL-PENDING |
| REQ-DSH-005 | per-user CPU and memory table | 5, 11 | MANUAL-PENDING |
| REQ-DSH-006 | disk chart | 12 | MANUAL-PENDING |
| REQ-DSH-007 | history charts and gap shading | 13 | MANUAL-PENDING |
| REQ-DSH-011 | layout at 1280 px and 375 px | 9 | MANUAL-PENDING |
| REQ-SEC-003 | central systemd unit on the host | 8 | MANUAL-PENDING |
| REQ-SEC-004 | agent systemd unit on the hosts | 8 | MANUAL-PENDING |
| REQ-SEC-006 | firewall rules | 7 | MANUAL-PENDING |
| REQ-OPS-003 | install-central.sh on the central host | 14 | MANUAL-PENDING |

Placeholders: `<CENTRAL>` is `<CENTRAL_HOST>:8080`, the address and port of the central host; `<you>` is your
login name.

## Checklist

### 1. diagnose matches nvidia-smi (every server)

```
sudo gpumon-agent diagnose | less
nvidia-smi -L
nvidia-smi
```

Expected: the "GPUs and GPU processes" section lists the same GPUs (index, UUID, name) as `nvidia-smi -L`,
the driver and CUDA versions match the `nvidia-smi` header, and every process in the `nvidia-smi` process table
appears with the same PID and a plausible owner and method. Every GPU process and every root process above
1 % CPU shows its attribution chain and evidence (uid, loginuid, cgroup path, container ID and labels, subuid
match). The "Problems" section is empty or explains a missing source.

### 2. multipass names can be extracted (servers with multipass VMs)

```
multipass list
sudo gpumon-agent diagnose | sed -n '/QEMU processes/,/== Owners/p'
```

Expected: every running VM appears with its raw argv and `vm_name` equal to the multipass instance name. If
`vm_name` is empty, add a regular expression with one capture group to `multipass.name_patterns` in
`/etc/gpumon/agent.yaml` that matches the argv, then `sudo systemctl restart gpumon-agent` and repeat. A VM
named `<you>-test` must show owner `<you>` with method `multipass_name`.

### 3. Rootful Docker with and without a label (the host with rootful Docker)

```
sudo docker run -d --rm --gpus all --name gm-label --label owner=<you> nvidia/cuda:12.4.1-base-ubuntu22.04 sleep 600
sudo docker run -d --rm --gpus all --name gm-nolabel nvidia/cuda:12.4.1-base-ubuntu22.04 sleep 600
sudo gpumon-agent diagnose | grep -A3 -E 'gm-label|gm-nolabel|container'
```

Expected: processes of `gm-label` show owner `<you>`, method `docker_label`, and the container name and image;
`gm-nolabel` shows the configured `docker.default_owner` with method `docker_default_owner` (or `unattributed`
when it is empty). For a GPU job, the dashboard GPU detail shows the same. Clean up with
`sudo docker stop gm-label gm-nolabel`.

### 4. Rootless Docker GPU job

As a rootless Docker user:

```
docker run --rm --gpus all --user 1000:1000 pytorch/pytorch python -c "import torch,time; x=torch.zeros(1<<28,device='cuda'); time.sleep(300)"
sudo gpumon-agent diagnose | grep -B1 -A6 'python'
```

Expected: the process is attributed to you with method `subuid` (container user) or `uid` (container root),
and the dashboard shows you on that GPU within 30 seconds.

### 5. sudo is attributed through loginuid

From your own account:

```
sudo python3 -c "while True: pass" &
sleep 40; sudo gpumon-agent diagnose | grep -A4 'python3'
sudo kill %1
```

Expected: the process (UID 0) shows `loginuid=<your uid>`, owner `<you>`, method `loginuid`. The per-owner
table on the dashboard shows about 1 core for you, method `loginuid`.

### 6. Time synchronization

```
timedatectl            # on every server
```

Expected: "System clock synchronized: yes" and "NTP service: active" on all servers. On the dashboard, the
"Clock skew" value of every server card is within +/- 5 s and no skew warning is shown.

### 7. Firewall (REQ-SEC-006)

```
sudo ufw status numbered                              # on the central host, see docs/firewall.md
curl -m 5 -s -o /dev/null -w '%{http_code}\n' http://<CENTRAL>/     # from outside the subnet and VPN
curl -m 5 -s -o /dev/null -w '%{http_code}\n' http://<CENTRAL>/     # from a VPN client
```

Expected: the rules allow port 8080 only from `<INTERNAL_SUBNET>` and `<VPN_POOL>` and deny the rest; the
outside request times out (`000`); the VPN request returns `200`.

### 8. systemd units (REQ-SEC-003, REQ-SEC-004)

```
systemctl status gpumon-central gpumon-agent
systemctl show gpumon-central -p User -p NoNewPrivileges -p ProtectSystem -p ProtectHome -p PrivateTmp -p ReadWritePaths
systemd-analyze security gpumon-agent
systemd-analyze security gpumon-central
ss -ltnp | grep gpumon-agent || echo "agent has no listening socket"
```

Expected: both units are active; central runs as `gpumon` with `NoNewPrivileges=yes`, `ProtectSystem=strict`,
`ReadWritePaths=/var/lib/gpumon`, `PrivateTmp=yes`, `ProtectHome=yes`; the agent runs as root with the
hardening options of `deploy/gpumon-agent.service` and its exposure score is reported (root is required to
read all processes); the agent has no listening socket.

### 9. Dashboard layout (REQ-DSH-011)

Open `http://<CENTRAL>/` on a laptop at 1280 px width and on a phone (375 px). Expected: at 1280 px the
summary, the server cards, and the charts are usable without horizontal page scrolling; at 375 px everything is
readable in one column and wide tables scroll inside their own box. The page updates itself every 15 s
("Updated ..." in the header) without reloading. Times carry the label of the configured `timezone`, for example "Asia/Taipei (UTC+08:00)".

### 10. Sizing after one week

```
du -b /var/lib/gpumon/snapshots-*.db
sudo -u gpumon sqlite3 /var/lib/gpumon/snapshots-$(date -u +%Y-%m).db "SELECT COUNT(*) FROM snapshots WHERE kind='sample'"
```

Expected: the bytes per day are close to "raw snapshot bytes per day" in `verification/report.md`. If the
projected 3-month total approaches `raw_max_total_bytes`, adjust `raw_retention_months` or the cap.

### 11. Live view content (REQ-DSH-001, REQ-DSH-003, REQ-DSH-004, REQ-DSH-005)

With a known job running (item 3, 4, or 5), open the dashboard. Expected: "Where can I run?" lists every GPU,
free GPUs first; each server card shows its display name, online status with last-seen time, CPU %, load,
memory and swap, its GPUs with a text state badge, utilization, a VRAM bar, and each owner with VRAM,
attribution method, container or VM name, and program names; the per-owner table shows cores, RSS, process
count, and method, with `root-shared`, `system`, and `unattributed` visibly distinct.

### 12. Disk chart (REQ-DSH-006)

Run a scan once (`disk.scan_on_start: true` and restart the agent, or wait for 03:30). Expected: one bar per
monitored filesystem with the top owners, "others", "unscanned/other usage", and "free", human-readable sizes,
and "per-owner scan as of <time>". Compare the owner totals with `sudo du -sx --block-size=1 /home/*`.

### 13. History and gaps (REQ-DSH-007)

Stop one agent for 3 minutes (`sudo systemctl stop gpumon-agent; sleep 180; sudo systemctl start
gpumon-agent`). Expected: after the next history refresh (1 minute) its charts show a shaded region; hovering
it shows "DATA GAP: no samples from <server> between ... (agent offline or unreachable)." The charts show CPU
and memory stacked by owner and VRAM stacked by owner for each GPU over the last 72 hours.

### 14. install-central.sh (REQ-OPS-003)

```
sudo ./install-central.sh --dry-run
sudo ./install-central.sh --listen <CENTRAL>
sudo ./install-central.sh --listen <CENTRAL>     # second run changes nothing
id gpumon; ls -ld /var/lib/gpumon /opt/gpumon/venv; ls -l /etc/gpumon/token
```

Expected: user `gpumon` exists, the venv and data directory (0750, owner gpumon) exist, the token is 0600,
`systemctl status gpumon-central` is active, and the second run reports existing items without errors.

### 15. Daily CSV export (optional timer)

```
sudo ./install-central.sh --auto-export yes
systemctl list-timers gpumon-export.timer
sudo systemctl start gpumon-export.service && journalctl -u gpumon-export.service -n 20
sudo ls -l /var/lib/gpumon/exports/
```

Expected: the timer is listed with its next run around 04:30; the service finishes successfully and writes
`sessions-`, `rollups-`, `gaps-`, and `disk-YYYY-MM-DD.csv` for days older than 72 hours (mode 0640, owner
gpumon); a second start writes no new files. `sudo ./install-central.sh --auto-export no` disables and removes
the timer and keeps the files.
