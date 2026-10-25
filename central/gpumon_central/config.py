"""Loading and validation of central.yaml.

Every key has a default. An invalid file raises ConfigError with an English
message that names the key.
"""

from __future__ import annotations

import copy
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

import yaml

DEFAULTS: dict[str, Any] = {
    "listen": "127.0.0.1:8080",
    "data_dir": "/var/lib/gpumon",
    "token_file": "/etc/gpumon/token",
    "servers_file": "/etc/gpumon/servers.json",
    "timezone": "Asia/Taipei",
    "expected_interval_seconds": 30,
    "offline_after_intervals": 3,
    "clock_skew_warn_seconds": 5,
    "public_history_hours": 72,
    "raw_retention_months": 3,
    "raw_max_total_bytes": 5368709120,
    "session_gap_tolerance_minutes": 5,
    "gpu_state": {
        "free_vram_mib": 512,
        "idle_window_minutes": 30,
        "idle_util_pct": 5,
        "ignore_processes": ["Xorg", "Xwayland", "gnome-shell"],
    },
    "disk_top_owners": 8,
    "auto_export": {
        "dir": "",
        "kinds": ["sessions", "rollups", "gaps", "disk"],
        "catch_up_days": 7,
    },
    "refresh_seconds": 15,
    "heartbeat_seconds": 60,
    "maintenance_interval_minutes": 60,
    "log_rate_limit_minutes": 60,
    "max_body_bytes": 4194304,
    "web_dir": "",
    "schema_file": "",
    "log_level": "info",
}

EXPORT_KINDS = ("snapshots", "sessions", "rollups", "gaps", "disk")

# key -> (python types, description, check, check description)
_NUM = (int, float)
_RULES: dict[str, tuple] = {
    "listen": (str, "a string", lambda v: _split_listen(v) is not None, 'must look like "host:port"'),
    "data_dir": (str, "a string", lambda v: v.startswith("/") or v.startswith("."), "must be a path"),
    "token_file": (str, "a string", bool, "must not be empty"),
    "servers_file": (str, "a string", lambda v: True, ""),
    "timezone": (str, "a string", lambda v: _zone_ok(v), "must be an IANA time zone such as Asia/Taipei"),
    "expected_interval_seconds": (_NUM, "a number", lambda v: v > 0, "must be greater than 0"),
    "offline_after_intervals": (_NUM, "a number", lambda v: v >= 1, "must be at least 1"),
    "clock_skew_warn_seconds": (_NUM, "a number", lambda v: v > 0, "must be greater than 0"),
    "public_history_hours": (_NUM, "a number", lambda v: v > 0, "must be greater than 0"),
    "raw_retention_months": (int, "an integer", lambda v: v >= 1, "must be at least 1"),
    "raw_max_total_bytes": (int, "an integer", lambda v: v > 0, "must be greater than 0"),
    "session_gap_tolerance_minutes": (_NUM, "a number", lambda v: v > 0, "must be greater than 0"),
    "gpu_state.free_vram_mib": (_NUM, "a number", lambda v: v >= 0, "must not be negative"),
    "gpu_state.idle_window_minutes": (_NUM, "a number", lambda v: v > 0, "must be greater than 0"),
    "gpu_state.idle_util_pct": (_NUM, "a number", lambda v: 0 <= v <= 100, "must be between 0 and 100"),
    "gpu_state.ignore_processes": (list, "a list of strings", lambda v: all(isinstance(x, str) for x in v),
                                   "must contain only strings"),
    "disk_top_owners": (int, "an integer", lambda v: v >= 1, "must be at least 1"),
    "auto_export.dir": (str, "a string", lambda v: v == "" or v.startswith("/"), "must be empty or an absolute path"),
    "auto_export.kinds": (list, "a list of strings",
                          lambda v: bool(v) and len(set(v)) == len(v) and all(x in EXPORT_KINDS for x in v),
                          "must be a non-empty list of distinct kinds from snapshots, sessions, rollups, gaps, disk"),
    "auto_export.catch_up_days": (int, "an integer", lambda v: 1 <= v <= 3650, "must be between 1 and 3650"),
    "refresh_seconds": (_NUM, "a number", lambda v: v >= 1, "must be at least 1"),
    "heartbeat_seconds": (_NUM, "a number", lambda v: v > 0, "must be greater than 0"),
    "maintenance_interval_minutes": (_NUM, "a number", lambda v: v > 0, "must be greater than 0"),
    "log_rate_limit_minutes": (_NUM, "a number", lambda v: v >= 0, "must not be negative"),
    "max_body_bytes": (int, "an integer", lambda v: v >= 1024, "must be at least 1024"),
    "web_dir": (str, "a string", lambda v: True, ""),
    "schema_file": (str, "a string", lambda v: True, ""),
    "log_level": (str, "a string", lambda v: v in ("debug", "info", "warn", "error"),
                  "must be one of debug, info, warn, error"),
}


class ConfigError(Exception):
    """An invalid configuration. The message names the offending key."""


def _zone_ok(name: str) -> bool:
    try:
        ZoneInfo(name)
        return True
    except (ZoneInfoNotFoundError, ValueError):
        return False


def _split_listen(v: str) -> tuple[str, int] | None:
    host, sep, port = v.rpartition(":")
    if not sep or not host or not port.isdigit() or not 0 < int(port) < 65536:
        return None
    return host.strip("[]"), int(port)


def _merge(base: dict, override: Any, prefix: str) -> None:
    if override is None:
        return
    if not isinstance(override, dict):
        raise ConfigError(f'config error: key "{prefix or "(top level)"}": expected a mapping')
    for key, value in override.items():
        path = f"{prefix}.{key}" if prefix else str(key)
        if key not in base:
            raise ConfigError(f'config error: unknown key "{path}"')
        if isinstance(base[key], dict):
            _merge(base[key], value, path)
            continue
        rule = _RULES[path]
        types = rule[0]
        if isinstance(value, bool) or not isinstance(value, types):
            raise ConfigError(f'config error: key "{path}": invalid value {value!r} (expected {rule[1]})')
        if not rule[2](value):
            raise ConfigError(f'config error: key "{path}": {rule[3]}')
        base[key] = value


@dataclass
class Config:
    raw: dict[str, Any]

    def __getattr__(self, name: str) -> Any:
        try:
            return self.raw[name]
        except KeyError as e:
            raise AttributeError(name) from e

    @property
    def host(self) -> str:
        return _split_listen(self.raw["listen"])[0]

    @property
    def port(self) -> int:
        return _split_listen(self.raw["listen"])[1]

    @property
    def tz(self) -> ZoneInfo:
        return ZoneInfo(self.raw["timezone"])

    @property
    def gpu(self) -> dict[str, Any]:
        return self.raw["gpu_state"]

    @property
    def session_gap_tolerance_s(self) -> float:
        return float(self.raw["session_gap_tolerance_minutes"]) * 60

    @property
    def idle_window_s(self) -> float:
        return float(self.raw["gpu_state"]["idle_window_minutes"]) * 60

    @property
    def export_dir(self) -> str:
        """Directory of the daily automatic export (default: <data_dir>/exports)."""
        return self.raw["auto_export"]["dir"] or str(Path(self.raw["data_dir"]) / "exports")

    @property
    def public_history_s(self) -> float:
        return float(self.raw["public_history_hours"]) * 3600


def load_dict(data: Any) -> Config:
    cfg = copy.deepcopy(DEFAULTS)
    _merge(cfg, data, "")
    return Config(cfg)


def load(path: str | None) -> Config:
    """Load a YAML file over the defaults. None or "" returns the defaults."""
    if not path:
        return load_dict(None)
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as e:
        raise ConfigError(f"config error: cannot read {path}: {e.strerror}") from e
    try:
        data = yaml.safe_load(text)
    except yaml.YAMLError as e:
        raise ConfigError(f"config error: invalid YAML in {path}: {e}") from e
    return load_dict(data)


def documented_keys() -> list[str]:
    """All configuration keys in dotted form."""
    return sorted(_RULES)
