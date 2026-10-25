#!/usr/bin/env bash
# Build the release packages into dist/ (or OUTDIR):
#   gpumon-agent-VERSION-linux-amd64.tar.gz  binary, systemd unit, example config, install-agent.sh
#   gpumon-central-VERSION.tar.gz            source, wheels for offline install, web, schema, docs, install-central.sh
# Usage: scripts/package.sh VERSION [--agent-only] [--outdir DIR]
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
VERSION="${1:?usage: package.sh VERSION [--agent-only] [--outdir DIR]}"
shift
AGENT_ONLY=0
OUTDIR="dist"
while [ $# -gt 0 ]; do
  case "$1" in
    --agent-only) AGENT_ONLY=1; shift ;;
    --outdir) OUTDIR="$2"; shift 2 ;;
    *) echo "error: unknown option $1" >&2; exit 2 ;;
  esac
done
GO="${GO:-$(command -v go || echo /usr/local/go/bin/go)}"
export CGO_CFLAGS="${CGO_CFLAGS:--O2 -Wno-deprecated-declarations}"
mkdir -p "$OUTDIR"
OUTDIR="$(cd "$OUTDIR" && pwd)"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

# Agent package.
A="$STAGE/gpumon-agent-$VERSION-linux-amd64"
mkdir -p "$A"
(cd agent && CGO_ENABLED=1 GOOS=linux GOARCH=amd64 "$GO" build -trimpath \
  -ldflags "-s -w -X main.version=$VERSION" -o "$A/gpumon-agent" ./cmd/gpumon-agent)
install -m 0644 deploy/gpumon-agent.service deploy/agent.yaml.example "$A/"
install -m 0755 deploy/install-agent.sh "$A/"
tar -C "$STAGE" -czf "$OUTDIR/gpumon-agent-$VERSION-linux-amd64.tar.gz" "$(basename "$A")"
echo "wrote $OUTDIR/gpumon-agent-$VERSION-linux-amd64.tar.gz"
[ "$AGENT_ONLY" = 1 ] && exit 0

# Central package with wheels for Python 3.10 (Ubuntu 22.04) and 3.12 (Ubuntu 24.04).
C="$STAGE/gpumon-central-$VERSION"
mkdir -p "$C/central" "$C/wheels"
cp -r central/gpumon_central central/pyproject.toml central/requirements.txt "$C/central/"
find "$C/central" -name __pycache__ -prune -exec rm -rf {} +
cp -r web schema docs "$C/"
install -m 0644 deploy/gpumon-central.service deploy/gpumon-export.service deploy/gpumon-export.timer \
  deploy/central.yaml.example deploy/servers.json.example "$C/"
install -m 0755 deploy/install-central.sh "$C/"
PIP=".venv/bin/pip"
for pyver in 3.10 3.12; do
  "$PIP" download --quiet --disable-pip-version-check -r central/requirements.txt -d "$C/wheels" \
    --only-binary=:all: --implementation cp --python-version "$pyver" \
    --platform manylinux_2_17_x86_64 --platform manylinux2014_x86_64 --platform any
done
# pip evaluates environment markers for the host interpreter, so download the
# marker-guarded requirements (for example exceptiongroup on 3.10) explicitly.
grep ';' central/requirements.txt | sed 's/[[:space:]]*;.*//' | while read -r req; do
  "$PIP" download --quiet --disable-pip-version-check --no-deps "$req" -d "$C/wheels" \
    --only-binary=:all: --implementation cp --python-version 3.10 --platform any
done
"$PIP" wheel --quiet --disable-pip-version-check --no-deps -w "$C/wheels" ./central
tar -C "$STAGE" -czf "$OUTDIR/gpumon-central-$VERSION.tar.gz" "$(basename "$C")"
echo "wrote $OUTDIR/gpumon-central-$VERSION.tar.gz"
