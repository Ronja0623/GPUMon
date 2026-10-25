#!/usr/bin/env bash
# End-to-end simulation (SPEC.md Section 8.3). Needs bin/gpumon-agent and the
# .venv created by "make build".
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
[ -x bin/gpumon-agent ] || { echo "error: bin/gpumon-agent is missing; run make build" >&2; exit 1; }
[ -x .venv/bin/gpumon ] || { echo "error: .venv is missing; run make build" >&2; exit 1; }
rm -rf verification/e2e-work
exec .venv/bin/python scripts/e2e.py "$@"
