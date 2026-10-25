"""Token file, payload schema, and web asset locations."""

from __future__ import annotations

import json
import os
import stat
import sys
from collections.abc import Callable
from pathlib import Path

import fastjsonschema

REPO_ROOT = Path(__file__).resolve().parents[2]
SHARE_DIRS = [Path(sys.prefix) / "share" / "gpumon", Path("/opt/gpumon/share")]


class TokenError(Exception):
    pass


def read_token(path: str) -> str:
    """Read the shared token. Files with permissions wider than 0600 are refused."""
    try:
        st = os.stat(path)
    except OSError as e:
        raise TokenError(f"cannot read token file {path}: {e.strerror}") from e
    perm = stat.S_IMODE(st.st_mode)
    if perm & ~0o600:
        raise TokenError(f"token file {path} has permissions {perm:04o}; refusing to start (must be 0600 or stricter)")
    try:
        with open(path, encoding="utf-8") as f:
            token = f.read().strip()
    except OSError as e:
        raise TokenError(f"cannot read token file {path}: {e.strerror}") from e
    if not token:
        raise TokenError(f"token file {path} is empty")
    return token


def _find(configured: str, rel: str) -> Path:
    if configured:
        return Path(configured)
    for base in [REPO_ROOT, *SHARE_DIRS]:
        p = base / rel
        if p.exists():
            return p
    return REPO_ROOT / rel


def schema_path(configured: str = "") -> Path:
    return _find(configured, "schema/payload.schema.json")


def web_dir(configured: str = "") -> Path:
    return _find(configured, "web")


def load_validator(configured: str = "") -> Callable[[dict], dict]:
    """Return a function that validates one message against the schema. The
    message type selects the definition, so errors name the offending field."""
    with open(schema_path(configured), encoding="utf-8") as f:
        schema = json.load(f)
    whole = fastjsonschema.compile(schema)
    by_type = {}
    for ref in schema["oneOf"]:
        name = ref["$ref"].rsplit("/", 1)[1]
        by_type[name] = fastjsonschema.compile({"$schema": schema["$schema"], "definitions": schema["definitions"],
                                                "allOf": [{"$ref": f"#/definitions/{name}"}]})

    def validate(msg):
        if not isinstance(msg, dict) or msg.get("type") not in by_type:
            raise fastjsonschema.JsonSchemaException(
                f"data.type must be one of {', '.join(sorted(by_type))}")
        by_type[msg["type"]](msg)
        return whole(msg)

    return validate
