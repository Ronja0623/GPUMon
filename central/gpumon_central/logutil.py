"""Structured JSON-lines logging for journald.

Every line is ASCII-only JSON. Repeated identical warnings and errors are
rate-limited: the same line is written at most once per window, and the next
emission reports how many were suppressed.
"""

from __future__ import annotations

import json
import logging
import sys
import threading
import time
from collections.abc import Callable

LEVELS = {"debug": logging.DEBUG, "info": logging.INFO, "warn": logging.WARNING, "error": logging.ERROR}
_NAMES = {logging.DEBUG: "debug", logging.INFO: "info", logging.WARNING: "warn", logging.ERROR: "error",
          logging.CRITICAL: "error"}


class JSONFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        out = {
            "ts": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(record.created)) + f".{int(record.msecs):03d}Z",
            "level": _NAMES.get(record.levelno, "info"),
            "logger": record.name,
            "msg": record.getMessage(),
        }
        fields = getattr(record, "fields", None)
        if fields:
            out.update(fields)
        if record.exc_info:
            out["exception"] = self.formatException(record.exc_info)
        return json.dumps(out, ensure_ascii=True, default=str, sort_keys=False)


class RateLimitFilter(logging.Filter):
    """Drops repeated identical warnings and errors within the window."""

    def __init__(self, window_s: float, clock: Callable[[], float] = time.time):
        super().__init__()
        self.window_s = window_s
        self.clock = clock
        self._seen: dict[str, list[float]] = {}
        self._lock = threading.Lock()

    def filter(self, record: logging.LogRecord) -> bool:
        if record.levelno < logging.WARNING or self.window_s <= 0:
            return True
        key = getattr(record, "rate_key", None)
        if key is None:
            fields = json.dumps(getattr(record, "fields", None), sort_keys=True, default=str)
            key = f"{record.levelno}|{record.name}|{record.getMessage()}|{fields}"
        now = self.clock()
        with self._lock:
            entry = self._seen.get(key)
            if entry is not None and now - entry[0] < self.window_s:
                entry[1] += 1
                return False
            if entry is not None and entry[1]:
                fields = dict(getattr(record, "fields", None) or {})
                fields["suppressed"] = int(entry[1])
                record.fields = fields
            self._seen[key] = [now, 0]
            if len(self._seen) > 10000:
                self._seen.clear()
        return True


_handler: logging.Handler | None = None


def setup(level: str = "info", window_s: float = 3600, stream=None, clock: Callable[[], float] = time.time) -> logging.Handler:
    """Configure the root logger with one JSON handler (stderr by default)."""
    global _handler
    root = logging.getLogger()
    if _handler is not None:
        root.removeHandler(_handler)
    h = logging.StreamHandler(stream or sys.stderr)
    h.setFormatter(JSONFormatter())
    h.addFilter(RateLimitFilter(window_s, clock))
    root.addHandler(h)
    root.setLevel(LEVELS[level])
    for name in ("uvicorn", "uvicorn.error", "uvicorn.access"):
        lg = logging.getLogger(name)
        lg.handlers = []
        lg.propagate = True
    _handler = h
    return h


class Log:
    """Small wrapper that passes structured fields to the JSON formatter."""

    def __init__(self, name: str = "gpumon"):
        self._l = logging.getLogger(name)

    def _log(self, level: int, msg: str, key: str | None, fields: dict) -> None:
        extra: dict = {"fields": fields}
        if key is not None:
            extra["rate_key"] = key
        self._l.log(level, msg, extra=extra)

    def debug(self, msg: str, _key: str | None = None, **fields) -> None:
        self._log(logging.DEBUG, msg, _key, fields)

    def info(self, msg: str, _key: str | None = None, **fields) -> None:
        self._log(logging.INFO, msg, _key, fields)

    def warn(self, msg: str, _key: str | None = None, **fields) -> None:
        self._log(logging.WARNING, msg, _key, fields)

    def error(self, msg: str, _key: str | None = None, **fields) -> None:
        self._log(logging.ERROR, msg, _key, fields)
