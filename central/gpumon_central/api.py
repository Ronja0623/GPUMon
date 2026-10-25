"""HTTP API and dashboard.

POST /api/v1/ingest needs the bearer token. Every other endpoint is public
(no login, by design) and range queries are limited to the last
public_history_hours; older data is only available through "gpumon export".
"""

from __future__ import annotations

import asyncio
import contextlib
import hmac
import json
import signal
import time
from collections.abc import Callable
from pathlib import Path

import fastjsonschema
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import FileResponse, JSONResponse, Response
from starlette.routing import Mount, Route
from starlette.staticfiles import StaticFiles

from . import __version__, gaps, history, maintenance, sessions, views
from .config import Config
from .ingest import Ingestor
from .logutil import Log
from .servers_file import ServersFile
from .storage import Storage
from .timeutil import iso_local, iso_utc, parse_user_time, zone_label

log = Log("gpumon.api")


class RangeError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status
        self.message = message


def error(status: int, message: str) -> JSONResponse:
    return JSONResponse({"error": message}, status_code=status)


def parse_range(request: Request, cfg: Config, now: float) -> tuple[float, float]:
    """Return (start, end). A start before now - public_history_hours is
    rejected with HTTP 403."""
    q = request.query_params
    earliest = now - cfg.public_history_s
    try:
        start = parse_user_time(q["from"], cfg.tz) if "from" in q else earliest
        end = parse_user_time(q["to"], cfg.tz) if "to" in q else now
    except ValueError as e:
        raise RangeError(400, str(e)) from e
    if start < earliest:
        raise RangeError(403, f"The requested range starts before the public history window of the last "
                              f"{cfg.public_history_hours:g} hours. Use the 'gpumon export' command on the central "
                              f"host to read older history.")
    if end <= start:
        raise RangeError(400, "The end of the range must be after its start.")
    return start, end


def create_app(cfg: Config, storage: Storage, token: str, clock: Callable[[], float] = time.time,
               validator: Callable[[dict], dict] | None = None, web: Path | None = None,
               servers: ServersFile | None = None, background: bool = False) -> Starlette:
    from .resources import load_validator, web_dir

    validate = validator or load_validator(cfg.schema_file)
    webroot = web or web_dir(cfg.web_dir)
    ingestor = Ingestor(storage, cfg)
    srv = servers or ServersFile(cfg.servers_file)
    token_bytes = token.encode("utf-8")

    async def ingest(request: Request) -> Response:
        auth = request.headers.get("authorization", "")
        given = auth[7:].strip() if auth[:7].lower() == "bearer " else ""
        if not hmac.compare_digest(given.encode("utf-8", "replace"), token_bytes):
            log.warn("ingest rejected: missing or invalid bearer token", _key="auth-fail",
                     client=request.client.host if request.client else "")
            return error(401, "Missing or invalid bearer token.")
        length = request.headers.get("content-length")
        if length and length.isdigit() and int(length) > cfg.max_body_bytes:
            return error(413, f"The payload is larger than {cfg.max_body_bytes} bytes.")
        body = await request.body()
        if len(body) > cfg.max_body_bytes:
            return error(413, f"The payload is larger than {cfg.max_body_bytes} bytes.")
        try:
            msg = json.loads(body)
        except (ValueError, UnicodeDecodeError):
            return error(400, "The request body is not valid JSON.")
        try:
            validate(msg)
        except fastjsonschema.JsonSchemaException as e:
            return error(400, f"The payload does not match the schema: {e.message}")
        received = clock()
        result = ingestor.ingest(msg, received)
        return JSONResponse({"status": result, "received_at": iso_utc(received)})

    async def status(request: Request) -> Response:
        return JSONResponse(views.build_status(storage, cfg, srv.get(), clock()))

    def ranged(fn):
        async def handler(request: Request) -> Response:
            now = clock()
            try:
                start, end = parse_range(request, cfg, now)
            except RangeError as e:
                return error(e.status, e.message)
            return JSONResponse(fn(request, start, end, now))
        return handler

    def history_fn(request, start, end, now):
        return history.build_history(storage, cfg, srv.get(), start, end, now, request.query_params.get("server"))

    def gaps_fn(request, start, end, now):
        rows = gaps.overlapping(storage, request.query_params.get("server"), start, end)
        return {"gaps": [{"server_id": r["server_id"], "start": iso_utc(r["start_ts"]), "end": iso_utc(r["end_ts"]),
                          "start_local": iso_local(r["start_ts"], cfg.tz), "end_local": iso_local(r["end_ts"], cfg.tz),
                          "reason": r["reason"], "description": r["description"]} for r in rows]}

    def events_fn(request, start, end, now):
        rows = storage.audit.execute("SELECT * FROM events WHERE ts >= ? AND ts <= ? ORDER BY ts", (start, end))
        return {"events": [{"ts": iso_utc(r["ts"]), "ts_local": iso_local(r["ts"], cfg.tz), "server_id": r["server_id"],
                            "kind": r["kind"], "detail": r["detail"]} for r in rows]}

    def sessions_fn(request, start, end, now):
        q = "SELECT * FROM gpu_sessions WHERE start_ts <= ? AND end_ts >= ?"
        args: list = [end, start]
        if request.query_params.get("server"):
            q += " AND server_id = ?"
            args.append(request.query_params["server"])
        rows = storage.audit.execute(q + " ORDER BY start_ts", args).fetchall()
        return {"sessions": [{**sessions.to_dict(r), "start": iso_utc(r["start_ts"]), "end": iso_utc(r["end_ts"])}
                             for r in rows]}

    async def config_ep(request: Request) -> Response:
        now = clock()
        return JSONResponse({
            "version": __version__,
            "timezone": cfg.timezone,
            "timezone_label": zone_label(cfg.tz, now),
            "refresh_seconds": cfg.refresh_seconds,
            "public_history_hours": cfg.public_history_hours,
            "expected_interval_seconds": cfg.expected_interval_seconds,
        })

    async def healthz(request: Request) -> Response:
        return JSONResponse({"status": "ok"})

    async def index(request: Request) -> Response:
        return FileResponse(webroot / "index.html", media_type="text/html; charset=utf-8")

    async def heartbeat_loop() -> None:
        while True:
            await asyncio.sleep(cfg.heartbeat_seconds)
            storage.write_heartbeat(clock())

    async def maintenance_loop() -> None:
        while True:
            try:
                maintenance.run(storage, cfg, clock())
            except Exception as e:  # keep serving even if maintenance fails
                log.error("maintenance failed", error=str(e))
            await asyncio.sleep(cfg.maintenance_interval_minutes * 60)

    @contextlib.asynccontextmanager
    async def lifespan(app):
        tasks = []
        if background:
            storage.write_heartbeat(clock())
            tasks = [asyncio.create_task(heartbeat_loop()), asyncio.create_task(maintenance_loop())]
            with contextlib.suppress(NotImplementedError, RuntimeError):
                asyncio.get_running_loop().add_signal_handler(signal.SIGHUP, srv.request_reload)
            log.info("central started", listen=cfg.listen, data_dir=cfg.data_dir)
        yield
        for t in tasks:
            t.cancel()
        if background:
            storage.write_heartbeat(clock())
            storage.commit()

    routes = [
        Route("/api/v1/ingest", ingest, methods=["POST"]),
        Route("/api/v1/status", status),
        Route("/api/v1/history", ranged(history_fn)),
        Route("/api/v1/gaps", ranged(gaps_fn)),
        Route("/api/v1/events", ranged(events_fn)),
        Route("/api/v1/sessions", ranged(sessions_fn)),
        Route("/api/v1/config", config_ep),
        Route("/api/v1/healthz", healthz),
        Route("/", index),
        Mount("/static", app=StaticFiles(directory=str(webroot)), name="static"),
    ]
    app = Starlette(routes=routes, lifespan=lifespan)
    app.state.ingestor = ingestor
    app.state.servers = srv
    return app
