"""Response envelopes, error mapping and strict request-body decoding.

Success: {"data": ..., "meta": {page, page_size, total, total_pages}?}
Error:   {"error": {code, message, fields?, request_id}}
"""

from __future__ import annotations

import dataclasses
import json
import math
from datetime import date, datetime
from decimal import Decimal
from enum import Enum
from typing import Any
from uuid import UUID

from fastapi import Request
from fastapi.responses import Response
from pydantic import BaseModel, ValidationError

from ..store import Page


def _default(o: Any) -> Any:
    if isinstance(o, BaseModel):
        return o.model_dump(mode="json")
    if isinstance(o, datetime):
        return o.isoformat().replace("+00:00", "Z")
    if isinstance(o, date):
        return o.isoformat()
    if isinstance(o, UUID):
        return str(o)
    if isinstance(o, Enum):
        return o.value
    if isinstance(o, Decimal):
        return float(o)
    if hasattr(o, "to_json"):
        return o.to_json()
    if dataclasses.is_dataclass(o) and not isinstance(o, type):
        return dataclasses.asdict(o)
    raise TypeError(f"not JSON serialisable: {type(o).__name__}")


def encode(v: Any) -> bytes:
    return json.dumps(v, default=_default, ensure_ascii=False, separators=(",", ":")).encode()


def json_response(status: int, body: Any, headers: dict[str, str] | None = None) -> Response:
    return Response(encode(body), status_code=status, media_type="application/json; charset=utf-8", headers=headers)


def ok(data: Any, status: int = 200, headers: dict[str, str] | None = None) -> Response:
    return json_response(status, {"data": data}, headers)


def created(data: Any, headers: dict[str, str] | None = None) -> Response:
    return ok(data, 201, headers)


def paged(data: Any, page: Page, total: int) -> Response:
    page = page.normalize()
    meta = {"page": page.page, "page_size": page.page_size, "total": total,
            "total_pages": math.ceil(total / page.page_size)}
    return json_response(200, {"data": data, "meta": meta})


def no_content() -> Response:
    return Response(status_code=204)


def request_id(request: Request) -> str:
    return request.scope.get("state", {}).get("request_id", "")


def error_body(code: str, message: str, rid: str, fields: dict[str, str] | None = None) -> dict:
    err: dict = {"code": code, "message": message}
    if fields:
        err["fields"] = fields
    if rid:
        err["request_id"] = rid
    return {"error": err}


class ApiError(Exception):
    """An HTTP error with a stable machine-readable code."""

    def __init__(self, status: int, code: str, message: str, fields: dict[str, str] | None = None,
                 headers: dict[str, str] | None = None):
        super().__init__(message)
        self.status, self.code, self.message, self.fields, self.headers = status, code, message, fields, headers


def not_found(message: str = "Resource not found.") -> ApiError:
    return ApiError(404, "not_found", message)


def parse_uuid(raw: str) -> UUID:
    """Path ids that are not UUIDs are reported as missing resources."""
    try:
        return UUID(raw)
    except ValueError:
        raise not_found() from None


def _describe(err: dict) -> str:
    loc = ".".join(str(p) for p in err.get("loc", ()))
    typ = err.get("type", "")
    if typ == "json_invalid":
        return "malformed body"
    if typ == "extra_forbidden":
        return f'unknown field "{loc}"'
    expected = {"string_type": "a string", "int_type": "an integer", "bool_type": "a boolean",
                "list_type": "an array", "float_type": "a number", "datetime_type": "an RFC 3339 timestamp",
                "datetime_from_date_parsing": "an RFC 3339 timestamp", "datetime_parsing": "an RFC 3339 timestamp",
                "uuid_parsing": "a UUID", "uuid_type": "a UUID", "int_from_float": "an integer",
                "model_type": "an object", "dict_type": "an object"}.get(typ)
    if expected and loc:
        return f'field "{loc}" must be {expected}'
    return "malformed body"


async def decode[T: BaseModel](request: Request, model: type[T], max_bytes: int) -> T:
    """Reads a JSON body strictly: wrong content type, empty bodies, bodies
    over the limit, unknown fields and type mismatches are rejected."""
    ct = request.headers.get("content-type", "")
    if ct and not ct.lower().startswith("application/json"):
        raise ApiError(415, "unsupported_media_type", "Content-Type must be application/json.")
    raw = bytearray()
    async for chunk in request.stream():
        raw += chunk
        if len(raw) > max_bytes:
            raise ApiError(413, "body_too_large", f"Request body must be at most {max_bytes} bytes.")
    if not raw.strip():
        raise ApiError(400, "invalid_json", "Request body is empty.")
    try:
        return model.model_validate_json(bytes(raw))
    except ValidationError as e:
        errs = e.errors()
        top_level_type = errs and errs[0].get("type") == "model_type"
        detail = "body must be a JSON object" if top_level_type else _describe(errs[0]) if errs else "malformed body"
        raise ApiError(400, "invalid_json", f"Request body is not valid JSON: {detail}") from None
