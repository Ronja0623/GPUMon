#!/usr/bin/env bash
# Install or update gpumon-central on the central host (A10). Safe to run
# again: it creates the gpumon user, the virtualenv, the data directory, the
# configuration, the token, and the systemd unit only when they are missing,
# and upgrades the package in place. Optionally it installs a daily timer that
# exports records older than the public window (72 h) to CSV files.
#
# Usage:
#   sudo ./install-central.sh [--listen IP:PORT] [--prefix /opt/gpumon] \
#        [--data-dir /var/lib/gpumon] [--python python3] [--auto-export yes|no] \
#        [--no-systemd] [--dry-run]
#
# Without --auto-export, the script asks on a terminal (default: no) and keeps
# an already installed export timer when run non-interactively.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LISTEN="127.0.0.1:8080"
PREFIX="/opt/gpumon"
DATA_DIR="/var/lib/gpumon"
PYTHON="python3"
SYSTEMD=1
DRY_RUN=0
SVC_USER="gpumon"
AUTO_EXPORT=""

usage() {
  sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --listen) LISTEN="$2"; shift 2 ;;
    --prefix) PREFIX="${2%/}"; shift 2 ;;
    --data-dir) DATA_DIR="${2%/}"; shift 2 ;;
    --python) PYTHON="$2"; shift 2 ;;
    --auto-export) AUTO_EXPORT="$2"; shift 2 ;;
    --no-systemd) SYSTEMD=0; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage 0 ;;
    *) echo "error: unknown option $1" >&2; usage 2 ;;
  esac
done

die() { echo "error: $*" >&2; exit 1; }
case "$AUTO_EXPORT" in ""|yes|no) ;; *) echo "error: --auto-export must be yes or no" >&2; exit 2 ;; esac
say() { echo "install-central: $*"; }
run() {
  if [ "$DRY_RUN" = 1 ]; then echo "DRY-RUN: $*"; else "$@"; fi
}

# The package directory holds central/, web/, schema/, wheels/ (optional)
# and the deploy files; in a source checkout they are one level up.
PKG="$HERE"
[ -d "$PKG/central" ] || PKG="$(cd "$HERE/.." && pwd)"
[ -f "$PKG/central/pyproject.toml" ] || die "cannot find central/pyproject.toml next to this script"
[ -d "$PKG/web" ] || die "cannot find web/"
[ -f "$PKG/schema/payload.schema.json" ] || die "cannot find schema/payload.schema.json"
DEPLOY="$HERE"
[ -f "$DEPLOY/gpumon-central.service" ] || DEPLOY="$PKG/deploy"
if [ "$DRY_RUN" = 0 ] && [ "$(id -u)" != 0 ]; then
  die "run as root (or use --dry-run to see what would be done)"
fi
"$PYTHON" -c 'import sys; sys.exit(0 if sys.version_info >= (3, 10) else 1)' || die "Python 3.10 or later is required"

VENV="$PREFIX/venv"
SHARE="$PREFIX/share"
CONF="/etc/gpumon/central.yaml"
TOKEN="/etc/gpumon/token"
UNITS="/etc/systemd/system"
UNIT="$UNITS/gpumon-central.service"
EXPORT_SERVICE="$UNITS/gpumon-export.service"
EXPORT_TIMER="$UNITS/gpumon-export.timer"

# install_unit SRC DST: install a unit file with the chosen prefix, data
# directory, and time zone substituted; unchanged files are left alone.
install_unit() {
  local tmp
  tmp="$(mktemp)"
  sed -e "s|/opt/gpumon|$PREFIX|g" -e "s|/var/lib/gpumon|$DATA_DIR|g" -e "s|Asia/Taipei|$TZ_NAME|g" "$1" > "$tmp"
  if [ -f "$2" ] && cmp -s "$tmp" "$2"; then
    say "unit is up to date: $2"
  else
    run install -m 0644 "$tmp" "$2"
    say "installed $2"
  fi
  rm -f "$tmp"
}

# 1. Service user.
if id "$SVC_USER" >/dev/null 2>&1; then
  say "user $SVC_USER exists"
else
  run useradd --system --home-dir "$DATA_DIR" --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
  say "created system user $SVC_USER"
fi

# 2. Virtualenv and package (offline when wheels/ is present).
if [ -x "$VENV/bin/python" ]; then
  say "virtualenv exists: $VENV"
else
  run "$PYTHON" -m venv "$VENV"
  say "created virtualenv $VENV"
fi
PIP_ARGS=(--upgrade --disable-pip-version-check)
if [ -d "$PKG/wheels" ]; then
  # Offline install from the wheels shipped in the package (A11).
  PIP_ARGS+=(--no-index --find-links "$PKG/wheels")
  run "$VENV/bin/pip" install "${PIP_ARGS[@]}" -r "$PKG/central/requirements.txt"
  run "$VENV/bin/pip" install "${PIP_ARGS[@]}" --force-reinstall --no-deps gpumon-central
else
  # Source checkout with internet access.
  run "$VENV/bin/pip" install "${PIP_ARGS[@]}" -r "$PKG/central/requirements.txt"
  run "$VENV/bin/pip" install "${PIP_ARGS[@]}" --no-deps "$PKG/central"
fi
say "installed gpumon-central into $VENV"

# 3. Dashboard, schema, and docs.
run install -d -m 0755 "$SHARE/web" "$SHARE/schema" "$PREFIX/docs"
run cp -r "$PKG/web/." "$SHARE/web/"
run install -m 0644 "$PKG/schema/payload.schema.json" "$SHARE/schema/payload.schema.json"
if [ -d "$PKG/docs" ]; then
  run cp -r "$PKG/docs/." "$PREFIX/docs/"
fi

# 4. Data directory.
run install -d -m 0750 -o "$SVC_USER" -g "$SVC_USER" "$DATA_DIR"
say "data directory $DATA_DIR (mode 0750, owner $SVC_USER)"

# 5. Configuration (kept when present).
run install -d -m 0755 /etc/gpumon
if [ -f "$CONF" ]; then
  say "keeping existing $CONF"
else
  if [ "$DRY_RUN" = 1 ]; then
    echo "DRY-RUN: create $CONF from central.yaml.example (listen $LISTEN, data_dir $DATA_DIR)"
  else
    sed -e "s|^listen:.*|listen: \"$LISTEN\"|" \
        -e "s|^data_dir:.*|data_dir: $DATA_DIR|" \
        -e "s|^web_dir:.*|web_dir: $SHARE/web|" \
        -e "s|^schema_file:.*|schema_file: $SHARE/schema/payload.schema.json|" \
        "$DEPLOY/central.yaml.example" > "$CONF"
    chmod 0644 "$CONF"
  fi
  say "created $CONF"
fi

# 6. Shared token (generated once; copy it to every agent).
if [ -f "$TOKEN" ]; then
  run chmod 0600 "$TOKEN"
  say "keeping existing token $TOKEN"
else
  if [ "$DRY_RUN" = 1 ]; then
    echo "DRY-RUN: generate a random token in $TOKEN (mode 0600, owner $SVC_USER)"
  else
    umask 077
    "$PYTHON" -c 'import secrets; print(secrets.token_urlsafe(32))' > "$TOKEN"
    chown "$SVC_USER:$SVC_USER" "$TOKEN"
    chmod 0600 "$TOKEN"
  fi
  say "generated token $TOKEN; copy it to each agent (install-agent.sh --token-file)"
fi

# Dashboard time zone from central.yaml, used by the export timer.
TZ_NAME="Asia/Taipei"
if [ -f "$CONF" ]; then
  configured="$(sed -n 's/^timezone:[[:space:]]*["'\'']\{0,1\}\([^"'\''[:space:]#]*\).*/\1/p' "$CONF" | head -n 1)"
  if [ -n "$configured" ]; then TZ_NAME="$configured"; fi
fi
[ -f "/usr/share/zoneinfo/$TZ_NAME" ] || die "unknown time zone $TZ_NAME in $CONF"

# 7. systemd unit.
install_unit "$DEPLOY/gpumon-central.service" "$UNIT"
if [ "$SYSTEMD" = 1 ]; then
  run systemctl daemon-reload
  run systemctl enable gpumon-central.service
  run systemctl restart gpumon-central.service
  say "service gpumon-central is running; check it with: systemctl status gpumon-central"
fi

# 8. Optional daily CSV export of records older than the public window. The
# timer fires at 04:30 in the configured dashboard time zone (TZ_NAME).
if [ -z "$AUTO_EXPORT" ]; then
  if [ -f "$EXPORT_TIMER" ]; then
    AUTO_EXPORT="yes"
    say "keeping the installed daily export timer (pass --auto-export no to remove it)"
  elif [ -t 0 ] && [ "$DRY_RUN" = 0 ]; then
    read -r -p "Install a daily timer that exports records older than 72 hours to CSV files in $DATA_DIR/exports? [y/N] " answer
    case "$answer" in [yY]|[yY][eE][sS]) AUTO_EXPORT="yes" ;; *) AUTO_EXPORT="no" ;; esac
  else
    AUTO_EXPORT="no"
    say "skipping the daily CSV export timer (pass --auto-export yes to install it)"
  fi
fi
if [ "$AUTO_EXPORT" = "yes" ]; then
  install_unit "$DEPLOY/gpumon-export.service" "$EXPORT_SERVICE"
  install_unit "$DEPLOY/gpumon-export.timer" "$EXPORT_TIMER"
  if [ "$SYSTEMD" = 1 ]; then
    run systemctl daemon-reload
    run systemctl enable --now gpumon-export.timer
    say "daily export enabled; check it with: systemctl list-timers gpumon-export.timer"
  fi
elif [ -f "$EXPORT_TIMER" ] || [ -f "$EXPORT_SERVICE" ]; then
  if [ "$SYSTEMD" = 1 ]; then
    run systemctl disable --now gpumon-export.timer
  fi
  run rm -f "$EXPORT_TIMER" "$EXPORT_SERVICE"
  if [ "$SYSTEMD" = 1 ]; then
    run systemctl daemon-reload
  fi
  say "removed the daily export timer; files already exported are kept"
fi
say "done. Dashboard: http://$LISTEN/"
