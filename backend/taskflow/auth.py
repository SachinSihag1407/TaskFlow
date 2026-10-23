"""Password hashing, password policy and session tokens.

Hashes are PHC-formatted argon2id strings ($argon2id$v=19$m=...,t=...,p=...$salt$key),
so they are interchangeable with any standard argon2 implementation.
"""

from __future__ import annotations

import asyncio
import hashlib
import secrets
import threading
import unicodedata

from argon2 import PasswordHasher, Type
from argon2.exceptions import InvalidHashError, VerificationError, VerifyMismatchError

# RFC 9106 "second recommended" profile: 64 MiB, t=1, p=4 (above the OWASP
# minimum of m=19 MiB, t=2, p=1).
_HASHER = PasswordHasher(time_cost=1, memory_cost=64 * 1024, parallelism=4, hash_len=32, salt_len=16, type=Type.ID)


def hash_password_sync(password: str) -> str:
    return _HASHER.hash(password)


def verify_password_sync(password: str, encoded: str) -> bool:
    """Constant-time verification; False for a wrong password or a malformed hash."""
    try:
        return _HASHER.verify(encoded, password)
    except (VerifyMismatchError, VerificationError, InvalidHashError):
        return False


# Verified against when a login names an unknown user, so the response time
# does not reveal whether the account exists.
_DUMMY_HASH = hash_password_sync("timing-equalisation-placeholder")


# Each hash burns ~64 MiB and tens of milliseconds of CPU. It runs off the
# event loop so one login cannot stall other requests, and at most two run at
# once so a burst of logins cannot exhaust memory on a small instance.
_SLOTS = threading.BoundedSemaphore(2)


def _bounded(fn, *args):
    with _SLOTS:
        return fn(*args)


async def _offload(fn, *args):
    return await asyncio.to_thread(_bounded, fn, *args)


async def hash_password(password: str) -> str:
    return await _offload(hash_password_sync, password)


async def verify_password(password: str, encoded: str) -> bool:
    return await _offload(verify_password_sync, password, encoded)


async def verify_dummy(password: str) -> None:
    await _offload(verify_password_sync, password, _DUMMY_HASH)


def password_problem(pw: str) -> str:
    """A user-facing explanation of what is wrong with a password, or "".
    10-128 characters with at least one letter and one digit, symbol or space.
    Length matters more than composition rules, so that bar is kept low."""
    n = len(pw)
    if n < 10:
        return "Password must be at least 10 characters."
    if n > 128:
        return "Password must be at most 128 characters."
    letter = other = False
    for ch in pw:
        cat = unicodedata.category(ch)
        if cat.startswith("L"):
            letter = True
        elif cat == "Nd" or cat[0] in ("P", "S", "Z") or ch.isspace():
            other = True
    if not (letter and other):
        return "Password must contain a letter and a number or symbol."
    return ""


def new_token() -> str:
    """A URL-safe random token with 256 bits of entropy."""
    return secrets.token_urlsafe(32)


def hash_token(token: str) -> bytes:
    """SHA-256 of a session token. Only the hash is stored, so a database
    leak does not yield usable session cookies."""
    return hashlib.sha256(token.encode()).digest()
