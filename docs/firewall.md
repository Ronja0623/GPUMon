# Firewall

The dashboard has no login, so central must only be reachable from your own network. The agents open no
listening sockets and need no inbound rules; they only connect to central.

Placeholders: `<CENTRAL_HOST>` is the address of the machine that runs central, `<INTERNAL_SUBNET>` is your
internal network in CIDR form (for example `10.20.30.0/24`), and `<VPN_POOL>` is the address pool your VPN
assigns to remote users (leave its rules out if you have no VPN). The dashboard and the ingest API share
port 8080.

## Rules on the central host

Run on the central host. Order matters: allow rules first, then the deny rule.

```
# Keep SSH reachable before enabling the firewall.
sudo ufw allow from <INTERNAL_SUBNET> to any port 22 proto tcp
sudo ufw allow from <VPN_POOL> to any port 22 proto tcp

# Dashboard and ingest API: internal subnet and VPN pool only.
sudo ufw allow from <INTERNAL_SUBNET> to any port 8080 proto tcp comment 'gpumon dashboard and ingest'
sudo ufw allow from <VPN_POOL> to any port 8080 proto tcp comment 'gpumon dashboard'

# Deny everything else on that port, then default-deny inbound.
sudo ufw deny 8080/tcp
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw enable
sudo ufw status numbered
```

## Ingest path

The ingest path `/api/v1/ingest` uses the same port as the dashboard. When every GPU server is inside
`<INTERNAL_SUBNET>`, the subnet rule above already allows them, and the bearer token rejects every other
sender. If a server lives outside the subnet, add one rule per server address:

```
sudo ufw allow from <SERVER_IP> to any port 8080 proto tcp comment 'gpumon agent <SERVER_ID>'
```

## Bind address

Also bind central to its internal address rather than `0.0.0.0` (`listen: "<CENTRAL_HOST>:8080"` in
`/etc/gpumon/central.yaml`), so the port is not exposed on other interfaces.

## Check

From a host outside the subnet and the VPN pool, `curl -m 5 http://<CENTRAL_HOST>:8080/` must time out. From
the VPN it must return the dashboard. See item 7 of [manual-verification.md](manual-verification.md).
