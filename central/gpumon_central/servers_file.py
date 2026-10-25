"""Optional servers.json overrides: display_name, notes, sort_order, retired.

The file is re-read when its modification time changes or after SIGHUP, so
changes apply without a restart. Example:

    {"servers": {"<server_id>": {"display_name": "<name shown on the dashboard>", "sort_order": 1}}}
"""

from __future__ import annotations

import json
import os
import threading

from .logutil import Log

ALLOWED = {"display_name": str, "notes": str, "sort_order": (int, float), "retired": bool}


class ServersFile:
    def __init__(self, path: str, log: Log | None = None):
        self.path = path
        self.log = log or Log("gpumon.servers")
        self._mtime: float | None = None
        self._data: dict[str, dict] = {}
        self._force = False
        self._lock = threading.Lock()

    def request_reload(self) -> None:
        """Called from the SIGHUP handler."""
        self._force = True

    def get(self) -> dict[str, dict]:
        with self._lock:
            try:
                mtime = os.stat(self.path).st_mtime_ns if self.path else None
            except OSError:
                mtime = None
            if mtime != self._mtime or self._force:
                self._force = False
                self._mtime = mtime
                self._data = self._load() if mtime is not None else {}
            return self._data

    def _load(self) -> dict[str, dict]:
        try:
            with open(self.path, encoding="utf-8") as f:
                raw = json.load(f)
            servers = raw.get("servers", {}) if isinstance(raw, dict) else None
            if not isinstance(servers, dict):
                raise ValueError('the top level must be {"servers": {...}}')
            out: dict[str, dict] = {}
            for sid, ov in servers.items():
                if not isinstance(ov, dict):
                    raise ValueError(f"the entry for {sid} must be an object")
                clean = {}
                for k, v in ov.items():
                    if k not in ALLOWED:
                        raise ValueError(f'unknown key "{k}" for server {sid}')
                    if not isinstance(v, ALLOWED[k]) or (k == "sort_order" and isinstance(v, bool)):
                        raise ValueError(f'invalid value for "{k}" of server {sid}')
                    clean[k] = v
                out[str(sid)] = clean
            self.log.info("servers file loaded", path=self.path, servers=len(out))
            return out
        except (OSError, ValueError) as e:
            self.log.error("servers file ignored because it is invalid", path=self.path, error=str(e))
            return self._data


def apply(server_id: str, overrides: dict[str, dict]) -> dict:
    ov = overrides.get(server_id, {})
    return {
        "display_name": ov.get("display_name") or server_id,
        "notes": ov.get("notes", ""),
        "sort_order": ov.get("sort_order", 1000),
        "retired": bool(ov.get("retired", False)),
    }
