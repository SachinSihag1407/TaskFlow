"""Business logic between the HTTP layer and the repositories: validation,
authorization-aware orchestration and composition of engine operations."""

from pydantic import BaseModel, ConfigDict


class Input(BaseModel):
    """Base for request bodies. Parsed with ``model_validate_json`` in strict
    mode: unknown fields are rejected (typos surface as errors) and values are
    not coerced between JSON types."""

    model_config = ConfigDict(extra="forbid", strict=True)


def capitalize(s: str) -> str:
    return s[:1].upper() + s[1:] if s else s
