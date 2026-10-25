"""The gpumon command: serve, export, maintenance, check-config."""

from __future__ import annotations

import argparse
import os
import sys
import time
from datetime import date

from . import __version__, export, gaps, logutil, maintenance
from .config import ConfigError, load
from .logutil import Log
from .resources import TokenError, read_token
from .storage import Storage
from .timeutil import parse_user_time

DEFAULT_CONFIG = "/etc/gpumon/central.yaml"
log = Log("gpumon")


def _config(path: str):
    if path == DEFAULT_CONFIG and not os.path.exists(path):
        return load(None)
    return load(path)


def cmd_serve(args) -> int:
    cfg = _config(args.config)
    logutil.setup(cfg.log_level, cfg.log_rate_limit_minutes * 60)
    if os.geteuid() == 0 and not args.allow_root:
        log.error("refusing to start as root; run as the gpumon user or pass --allow-root")
        return 1
    try:
        token = read_token(cfg.token_file)
    except TokenError as e:
        log.error("refusing to start: token problem", error=str(e))
        return 1
    import uvicorn

    from .api import create_app

    st = Storage(cfg.data_dir)
    for text in gaps.record_central_offline(st, time.time()):
        log.warn("data gap recorded", description=text)
    app = create_app(cfg, st, token, background=True)
    server = uvicorn.Server(uvicorn.Config(app, host=cfg.host, port=cfg.port, log_config=None, access_log=False,
                                           lifespan="on", server_header=False, date_header=False))
    server.run()
    st.close()
    return 0


def cmd_export(args) -> int:
    cfg = _config(args.config)
    logutil.setup(cfg.log_level, cfg.log_rate_limit_minutes * 60)
    try:
        start = parse_user_time(args.from_, cfg.tz)
        end = parse_user_time(args.to, cfg.tz)
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    st = Storage(cfg.data_dir)
    try:
        n = export.export(st, cfg, args.kind, start, end, args.out, server=args.server, owner=args.owner)
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    finally:
        st.close()
    print(f"wrote {n} rows to {args.out}")
    return 0


def cmd_auto_export(args) -> int:
    cfg = _config(args.config)
    logutil.setup(cfg.log_level, cfg.log_rate_limit_minutes * 60)
    try:
        day = date.fromisoformat(args.date) if args.date else None
    except ValueError:
        print(f"error: invalid --date {args.date!r}; use YYYY-MM-DD", file=sys.stderr)
        return 2
    if args.catch_up_days is not None and args.catch_up_days < 1:
        print("error: --catch-up-days must be at least 1", file=sys.stderr)
        return 2
    st = Storage(cfg.data_dir)
    try:
        written = export.auto_export(st, cfg, time.time(), catch_up_days=args.catch_up_days, day=day)
    finally:
        st.close()
    log.info("automatic export finished", files=len(written), directory=cfg.export_dir)
    print(f"wrote {len(written)} files to {cfg.export_dir}")
    for p in written:
        print(f"  {p.name}")
    return 0


def cmd_maintenance(args) -> int:
    cfg = _config(args.config)
    logutil.setup(cfg.log_level, cfg.log_rate_limit_minutes * 60)
    st = Storage(cfg.data_dir)
    try:
        res = maintenance.run(st, cfg, time.time(), rollup_days=args.rollup_days)
    finally:
        st.close()
    print(f"deleted {len(res['deleted'])} raw snapshot files, closed {res['stale_sessions']} stale sessions, "
          f"wrote {res['rollup_rows']} rollup rows")
    return 0


def cmd_check_config(args) -> int:
    _config(args.config)
    print("configuration is valid")
    return 0


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="gpumon", description="GPUMon central server and tools.")
    p.add_argument("--version", action="version", version=__version__)
    sub = p.add_subparsers(dest="command", required=True)

    s = sub.add_parser("serve", help="run the ingest API and the dashboard")
    s.add_argument("--config", default=DEFAULT_CONFIG)
    s.add_argument("--allow-root", action="store_true", help="allow running as root (not recommended)")
    s.set_defaults(fn=cmd_serve)

    e = sub.add_parser("export", help="export audit data of any time range to CSV")
    e.add_argument("--config", default=DEFAULT_CONFIG)
    e.add_argument("--kind", required=True, choices=export.KINDS)
    e.add_argument("--from", dest="from_", required=True, help="start time, ISO 8601 or YYYY-MM-DD")
    e.add_argument("--to", required=True, help="end time, ISO 8601 or YYYY-MM-DD")
    e.add_argument("--server", help="only this server")
    e.add_argument("--owner", help="only this owner")
    e.add_argument("--out", required=True, help="output CSV file")
    e.set_defaults(fn=cmd_export)

    a = sub.add_parser("auto-export", help="export every local day that has left the public window to CSV files "
                                            "(run daily by gpumon-export.timer)")
    a.add_argument("--config", default=DEFAULT_CONFIG)
    a.add_argument("--date", help="export (or re-export) this local day, YYYY-MM-DD")
    a.add_argument("--catch-up-days", type=int, help="number of past days to check (default auto_export.catch_up_days)")
    a.set_defaults(fn=cmd_auto_export)

    m = sub.add_parser("maintenance", help="apply retention and recompute recent rollups")
    m.add_argument("--config", default=DEFAULT_CONFIG)
    m.add_argument("--rollup-days", type=int, default=2, help="number of recent local days to recompute")
    m.set_defaults(fn=cmd_maintenance)

    c = sub.add_parser("check-config", help="validate the configuration file")
    c.add_argument("--config", default=DEFAULT_CONFIG)
    c.set_defaults(fn=cmd_check_config)

    args = p.parse_args(argv)
    try:
        return args.fn(args)
    except ConfigError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
