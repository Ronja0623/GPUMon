#!/usr/bin/env bash
# Install or update gpumon-agent on one server. Safe to run again: unchanged
# files are left alone and an existing agent.yaml keeps its other settings.
#
# Usage:
#   sudo ./install-agent.sh --central-url http://<CENTRAL_IP>:8080/api/v1/ingest \
#        (--token-file FILE | --token VALUE) [--server-id ID] [--binary PATH] \
#        [--root DIR] [--no-systemd] [--dry-run]
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CENTRAL_URL=""
TOKEN_FILE=""
TOKEN_VALUE=""
SERVER_ID=""
BINARY=""
ROOT=""
SYSTEMD=1
DRY_RUN=0

usage() {
  sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --central-url) CENTRAL_URL="$2"; shift 2 ;;
    --token-file) TOKEN_FILE="$2"; shift 2 ;;
    --token) TOKEN_VALUE="$2"; shift 2 ;;
    --server-id) SERVER_ID="$2"; shift 2 ;;
    --binary) BINARY="$2"; shift 2 ;;
    --root) ROOT="${2%/}"; shift 2 ;;
    --no-systemd) SYSTEMD=0; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage 0 ;;
    *) echo "error: unknown option $1" >&2; usage 2 ;;
  esac
done

die() { echo "error: $*" >&2; exit 1; }
say() { echo "install-agent: $*"; }
run() {
  if [ "$DRY_RUN" = 1 ]; then echo "DRY-RUN: $*"; else "$@"; fi
}

[ -n "$CENTRAL_URL" ] || die "--central-url is required"
case "$CENTRAL_URL" in http://*|https://*) ;; *) die "--central-url must start with http:// or https://" ;; esac
[ -n "$TOKEN_FILE" ] || [ -n "$TOKEN_VALUE" ] || die "--token-file or --token is required"
if [ -z "$BINARY" ]; then
  for c in "$HERE/gpumon-agent" "$HERE/../bin/gpumon-agent"; do
    if [ -f "$c" ]; then BINARY="$c"; break; fi
  done
fi
[ -n "$BINARY" ] && [ -f "$BINARY" ] || die "agent binary not found; pass --binary PATH"
UNIT_SRC="$HERE/gpumon-agent.service"
EXAMPLE_SRC="$HERE/agent.yaml.example"
[ -f "$UNIT_SRC" ] || die "missing $UNIT_SRC"
[ -f "$EXAMPLE_SRC" ] || die "missing $EXAMPLE_SRC"
if [ -z "$ROOT" ] && [ "$DRY_RUN" = 0 ] && [ "$(id -u)" != 0 ]; then
  die "run as root (or use --dry-run, or --root DIR for a staging directory)"
fi

BIN_DST="$ROOT/usr/local/bin/gpumon-agent"
ETC="$ROOT/etc/gpumon"
CONF="$ETC/agent.yaml"
TOKEN_DST="$ETC/token"
UNIT_DST="$ROOT/etc/systemd/system/gpumon-agent.service"
CHANGED=0

same() { [ -f "$2" ] && cmp -s "$1" "$2"; }

# 1. Binary.
if same "$BINARY" "$BIN_DST"; then
  say "binary is up to date: $BIN_DST"
else
  run install -D -m 0755 "$BINARY" "$BIN_DST"
  say "installed $BIN_DST"
  CHANGED=1
fi

# 2. Token (mode 0600; an identical existing token keeps its owner).
run install -d -m 0755 "$ETC"
TMP_TOKEN="$(mktemp)"
trap 'rm -f "$TMP_TOKEN"' EXIT
if [ -n "$TOKEN_FILE" ]; then
  [ -r "$TOKEN_FILE" ] || die "cannot read $TOKEN_FILE"
  tr -d '\n\r' < "$TOKEN_FILE" > "$TMP_TOKEN"
else
  printf '%s' "$TOKEN_VALUE" > "$TMP_TOKEN"
fi
[ -s "$TMP_TOKEN" ] || die "the token is empty"
if [ -f "$TOKEN_DST" ] && [ "$(tr -d '\n\r' < "$TOKEN_DST")" = "$(cat "$TMP_TOKEN")" ]; then
  run chmod go-rwx "$TOKEN_DST"
  say "token is up to date: $TOKEN_DST"
else
  run install -m 0600 "$TMP_TOKEN" "$TOKEN_DST"
  say "installed token $TOKEN_DST (mode 0600)"
  CHANGED=1
fi

# 3. Configuration: create from the example, then set central_url and server_id.
if [ ! -f "$CONF" ]; then
  run install -m 0644 "$EXAMPLE_SRC" "$CONF"
  say "created $CONF from the example"
  CHANGED=1
fi
set_key() {
  local key="$1" value="$2"
  if [ "$DRY_RUN" = 1 ] && [ ! -f "$CONF" ]; then
    echo "DRY-RUN: set $key: \"$value\" in $CONF"
    return
  fi
  if grep -q "^$key: \"$value\"$" "$CONF"; then
    return
  fi
  if grep -q "^$key:" "$CONF"; then
    run sed -i "s|^$key:.*|$key: \"$value\"|" "$CONF"
  else
    if [ "$DRY_RUN" = 1 ]; then echo "DRY-RUN: append $key to $CONF"; else echo "$key: \"$value\"" >> "$CONF"; fi
  fi
  say "set $key in $CONF"
  CHANGED=1
}
set_key central_url "$CENTRAL_URL"
if [ -n "$SERVER_ID" ]; then
  set_key server_id "$SERVER_ID"
fi
if [ "$DRY_RUN" = 0 ] && [ -z "$ROOT" ]; then
  "$BIN_DST" version >/dev/null || die "the installed binary does not run"
fi

# 4. systemd unit.
if same "$UNIT_SRC" "$UNIT_DST"; then
  say "unit is up to date: $UNIT_DST"
else
  run install -D -m 0644 "$UNIT_SRC" "$UNIT_DST"
  say "installed $UNIT_DST"
  CHANGED=1
fi

if [ "$SYSTEMD" = 1 ] && [ -z "$ROOT" ]; then
  run systemctl daemon-reload
  run systemctl enable gpumon-agent.service
  if [ "$CHANGED" = 1 ]; then
    run systemctl restart gpumon-agent.service
  else
    run systemctl start gpumon-agent.service
  fi
  say "service gpumon-agent is enabled; check it with: systemctl status gpumon-agent"
else
  say "skipped systemctl (staging root or --no-systemd)"
fi
if [ "$CHANGED" = 0 ]; then
  say "nothing changed"
fi
