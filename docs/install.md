# Installation

Targets: Ubuntu 22.04 or 24.04 on linux/amd64, Python 3.10 or later on the central host. The servers do not
need internet access: build the packages on any machine with `make package` and copy `dist/` over.

## Build the packages

```
make package
# dist/gpumon-agent-0.1.0-linux-amd64.tar.gz
# dist/gpumon-central-0.1.0.tar.gz   (includes wheels for Python 3.10 and 3.12)
```

## Install central

On the central host (the examples use srv-40, `<PREFIX>.40`):

```
tar xzf gpumon-central-0.1.0.tar.gz && cd gpumon-central-0.1.0
sudo ./install-central.sh --listen <PREFIX>.40:8080
sudo ./install-central.sh --dry-run          # shows every action without changing anything
```

The script is idempotent. It creates the system user `gpumon`, the virtualenv `/opt/gpumon/venv`, the data
directory `/var/lib/gpumon` (mode 0750), `/etc/gpumon/central.yaml` (kept if present), a random token in
`/etc/gpumon/token` (mode 0600, kept if present), and the systemd unit `gpumon-central.service`, then starts
the service. Check it with `systemctl status gpumon-central` and open `http://<PREFIX>.40:8080/`.

When run on a terminal, the script asks whether to install the daily CSV export of records older than 72
hours (see "Automatic daily export" in [operations.md](operations.md)); answer `y` to enable it. Pass
`--auto-export yes` or `--auto-export no` to decide without the question, for example in automation, and run
the script again with the other value to change your mind. Without a terminal the default is no.

Then apply the firewall rules in [firewall.md](firewall.md).

## Install an agent

Copy the token from the central host to the server (for example with `scp`), then:

```
tar xzf gpumon-agent-0.1.0-linux-amd64.tar.gz && cd gpumon-agent-0.1.0-linux-amd64
sudo ./install-agent.sh --central-url http://<PREFIX>.40:8080/api/v1/ingest --token-file ./token
sudo shred -u ./token
```

The script is idempotent: it installs `/usr/local/bin/gpumon-agent`, `/etc/gpumon/token` (mode 0600),
`/etc/gpumon/agent.yaml` (created from the example once; later runs only update `central_url` and
`server_id`), and `gpumon-agent.service`, then starts the service. Options: `--server-id ID` (default: host
name), `--dry-run`, `--root DIR` (stage into a directory), `--no-systemd`.

On the central host itself, use `--token-file /etc/gpumon/token`; the existing file is left as it is.

On the host where the rootful Docker user works, set `docker.default_owner` in `/etc/gpumon/agent.yaml`
(assumption A5) and run `sudo systemctl restart gpumon-agent`.

Verify the agent with `sudo gpumon-agent diagnose` (see [manual-verification.md](manual-verification.md)).

## Add a server

Install the agent on the new server with the central URL and the token. Nothing changes on central: the
server registers itself with its first sample and appears on the dashboard. Optionally give it a display name
in `/etc/gpumon/servers.json` (see `servers.json.example`); central picks the change up without a restart.

## Remove a server

Stop and disable the agent (`sudo systemctl disable --now gpumon-agent`). To hide the server from the live
view while keeping its history, set `"retired": true` for it in `/etc/gpumon/servers.json`.

## Upgrade

Run the same install scripts from the newer packages. Configuration, token, and data are kept.
