"""Structured JSON logging and helpers that keep secrets out of logs."""

from __future__ import annotations

import json
import logging
import re
import sys
from datetime import UTC, datetime
from typing import Any


class JsonFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        out: dict[str, Any] = {
            "time": datetime.fromtimestamp(record.created, UTC).isoformat(),
            "level": record.levelname,
            "msg": record.getMessage(),
            "component": getattr(record, "component", record.name),
        }
        extra = getattr(record, "fields", None)
        if extra:
            out.update(extra)
        if record.exc_info:
            out["exc"] = self.formatException(record.exc_info)
        return json.dumps(out, default=str)


class Log:
    """Small wrapper around logging that attaches key/value fields."""

    def __init__(self, logger: logging.Logger, fields: dict[str, Any] | None = None):
        self._l = logger
        self._f = fields or {}

    def with_(self, **fields: Any) -> Log:
        return Log(self._l, {**self._f, **fields})

    def _log(self, level: int, msg: str, fields: dict[str, Any]) -> None:
        if self._l.isEnabledFor(level):
            self._l.log(level, msg, extra={"fields": {**self._f, **fields}, "component": self._f.get("component", "")})

    def debug(self, msg: str, **f: Any) -> None:
        self._log(logging.DEBUG, msg, f)

    def info(self, msg: str, **f: Any) -> None:
        self._log(logging.INFO, msg, f)

    def warning(self, msg: str, **f: Any) -> None:
        self._log(logging.WARNING, msg, f)

    def error(self, msg: str, **f: Any) -> None:
        self._log(logging.ERROR, msg, f)


def new_logger(level: str, component: str) -> Log:
    logger = logging.getLogger("taskflow")
    if not logger.handlers:
        h = logging.StreamHandler(sys.stdout)
        h.setFormatter(JsonFormatter())
        logger.addHandler(h)
        logger.propagate = False
    logger.setLevel({"debug": logging.DEBUG, "warn": logging.WARNING, "error": logging.ERROR}.get(level.lower(), logging.INFO))
    return Log(logger, {"component": component})


def null_logger() -> Log:
    logger = logging.getLogger("taskflow.null")
    logger.addHandler(logging.NullHandler())
    logger.propagate = False
    logger.setLevel(logging.CRITICAL + 1)
    return Log(logger)


_SENSITIVE = (
    "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key", "apikey",
    "token", "secret", "password", "passwd", "private-key", "access_token", "refresh_token", "client_secret",
)


def is_sensitive_key(key: str) -> bool:
    """True if a header or field name looks like it carries a credential."""
    k = key.lower()
    return any(s in k for s in _SENSITIVE)


_BEARER = re.compile(r"(?i)(bearer|basic)\s+[A-Za-z0-9\-._~+/=]+")
_URL_CRED = re.compile(r"://[^/\s:@]+:[^/\s@]+@")
_QUERY_KEY = re.compile(r"(?i)([?&](?:token|key|apikey|api_key|secret|signature|sig|password)=)[^&\s]+")


def redact(text: str) -> str:
    """Masks credential-looking substrings before they are logged or stored."""
    text = _BEARER.sub(r"\1 [REDACTED]", text)
    text = _URL_CRED.sub("://[REDACTED]@", text)
    return _QUERY_KEY.sub(r"\1[REDACTED]", text)
