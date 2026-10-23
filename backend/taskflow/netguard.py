"""Outbound HTTP that refuses internal, loopback, link-local (cloud metadata)
and other non-public addresses.

The check runs on the IP address actually being dialled, after DNS
resolution, so DNS-rebinding tricks cannot bypass it: the connection is made
to the vetted IP itself, while TLS still verifies the original hostname. It
applies equally to every redirect hop, and environment proxies are never
used (a proxy would connect on our behalf and bypass the check).
"""

from __future__ import annotations

import asyncio
import ipaddress
import socket
from urllib.parse import urlsplit

import httpcore


class Blocked(Exception):
    """The destination address is not allowed."""


_BLOCKED = [ipaddress.ip_network(c) for c in (
    "0.0.0.0/8",          # "this" network
    "10.0.0.0/8",         # RFC 1918
    "100.64.0.0/10",      # carrier-grade NAT
    "127.0.0.0/8",        # loopback
    "169.254.0.0/16",     # link-local, incl. 169.254.169.254 cloud metadata
    "172.16.0.0/12",      # RFC 1918
    "192.0.0.0/24",       # IETF protocol assignments
    "192.0.2.0/24",       # TEST-NET-1
    "192.88.99.0/24",     # 6to4 relay anycast
    "192.168.0.0/16",     # RFC 1918
    "198.18.0.0/15",      # benchmarking
    "198.51.100.0/24",    # TEST-NET-2
    "203.0.113.0/24",     # TEST-NET-3
    "224.0.0.0/4",        # multicast
    "240.0.0.0/4",        # reserved
    "255.255.255.255/32", # broadcast
    "::/128",             # unspecified
    "::1/128",            # loopback
    "::ffff:0:0/96",      # IPv4-mapped (checked again after unmapping)
    "64:ff9b::/96",       # NAT64 (could reach internal IPv4)
    "64:ff9b:1::/48",
    "100::/64",           # discard
    "2001::/23",          # IETF protocol assignments
    "2001:db8::/32",      # documentation
    "2002::/16",          # 6to4 (can embed internal IPv4)
    "fc00::/7",           # unique local
    "fe80::/10",          # link-local
    "ff00::/8",           # multicast
)]


def is_public(ip: str | ipaddress.IPv4Address | ipaddress.IPv6Address) -> bool:
    """Whether ip is a publicly routable unicast address."""
    try:
        addr = ipaddress.ip_address(ip) if isinstance(ip, str) else ip
    except ValueError:
        return False
    if isinstance(addr, ipaddress.IPv6Address):
        if addr.scope_id:
            return False
        if addr.ipv4_mapped is not None:
            addr = addr.ipv4_mapped
    for net in _BLOCKED:
        if addr.version == net.version and addr in net:
            return False
    return addr.is_global


def validate_url(raw: str, allow_private: bool = False) -> None:
    """Static checks at configuration time: scheme, credentials, host, port
    and literal IPs. Hostnames are re-checked against their resolved IPs on
    every connection. Raises ValueError (or Blocked) with a user-facing message."""
    if len(raw) > 2048:
        raise ValueError("URL is too long")
    try:
        u = urlsplit(raw)
        port = u.port
    except ValueError:
        raise ValueError("URL is not valid") from None
    if u.scheme not in ("http", "https"):
        raise ValueError("URL scheme must be http or https")
    if u.username is not None or u.password is not None:
        raise ValueError("URL must not contain credentials; use a header instead")
    host = u.hostname or ""
    if not host:
        raise ValueError("URL must include a host")
    if port is not None:
        if not 1 <= port <= 65535:
            raise ValueError("URL port is not valid")
        # Restricting ports limits use of the webhook as a port scanner
        # against well-known internal services.
        if not allow_private and port not in (80, 443) and port < 1024:
            raise ValueError("URL port must be 80, 443 or above 1023")
    if allow_private:
        return
    lower = host.lower().rstrip(".")
    if (lower == "localhost" or lower.endswith((".localhost", ".local", ".internal"))
            or lower == "metadata.google.internal"):
        raise Blocked(f"destination address is not allowed: {host}")
    try:
        ip = ipaddress.ip_address(lower.strip("[]"))
    except ValueError:
        return
    if not is_public(ip):
        raise Blocked(f"destination address is not allowed: {ip}")


async def resolve(host: str, port: int) -> list[str]:
    """All addresses a hostname resolves to, in resolver order."""
    infos = await asyncio.get_running_loop().getaddrinfo(host, port, type=socket.SOCK_STREAM)
    return list(dict.fromkeys(info[4][0] for info in infos))


class GuardedBackend(httpcore.AsyncNetworkBackend):
    """Resolves the hostname itself and connects only to a public address."""

    def __init__(self, allow_private: bool = False):
        self.allow_private = allow_private
        self._inner = httpcore.AnyIOBackend()

    async def connect_tcp(self, host, port, timeout=None, local_address=None, socket_options=None):  # noqa: ASYNC109 - httpcore interface
        if self.allow_private:
            return await self._inner.connect_tcp(host, port, timeout, local_address, socket_options)
        ips = await resolve(host, port)
        public = [ip for ip in ips if is_public(ip)]
        if not public:
            shown = ips[0] if ips else host
            raise Blocked(f"destination address is not allowed: {shown} is a private, loopback, "
                          "link-local or reserved address")
        last: Exception | None = None
        for ip in public:
            try:
                return await self._inner.connect_tcp(ip, port, timeout, local_address, socket_options)
            except (httpcore.ConnectError, httpcore.ConnectTimeout) as e:
                last = e
        raise last  # type: ignore[misc]

    async def connect_unix_socket(self, *args, **kwargs):
        raise Blocked("unix sockets are not allowed")

    async def sleep(self, seconds: float) -> None:
        await self._inner.sleep(seconds)


def new_pool(allow_private: bool = False) -> httpcore.AsyncConnectionPool:
    """A connection pool whose every connection is checked. httpcore never
    consults environment proxies, which is exactly what we want here."""
    return httpcore.AsyncConnectionPool(network_backend=GuardedBackend(allow_private), max_connections=50,
                                        keepalive_expiry=60, http2=False, retries=0)
