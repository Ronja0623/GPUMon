# Firewall

The dashboard has no login, so central must only be reachable from the lab network. The agents open no
listening sockets and need no inbound rules; they only connect to central.

Placeholders (assumptions A1 and A2): `<PREFIX>` is the first three octets of the server addresses,
`<INTERNAL_SUBNET>` is the internal network (for example `<PREFIX>.0/24`), `<VPN_POOL>` is the address pool of
the VPN. The dashboard and the ingest API share port 8080.

## Rules on the central host

Run on the central host (srv-40). Order matters: allow rules first, then the deny rule.

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

The ingest path `/api/v1/ingest` uses the same port as the dashboard. The servers (`<PREFIX>.40` to
`<PREFIX>.43` and any future server) are inside `<INTERNAL_SUBNET>`, so the subnet rule above already allows
them, and the bearer token rejects every other sender. If the servers ever live outside the subnet, add one
rule per server:

```
sudo ufw allow from <PREFIX>.41 to any port 8080 proto tcp comment 'gpumon agent srv-41'
```

## Bind address

Also bind central to the internal IP rather than `0.0.0.0` (`listen: "<PREFIX>.40:8080"` in
`/etc/gpumon/central.yaml`), so the port is not exposed on other interfaces.

## Check

From a host outside the subnet and the VPN pool, `curl -m 5 http://<PREFIX>.40:8080/` must time out. From the
VPN it must return the dashboard. See item 7 of [manual-verification.md](manual-verification.md).
