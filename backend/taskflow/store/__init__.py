"""PostgreSQL repositories.

Functions take a connection so callers decide transaction boundaries; the
engine and services compose them into atomic operations. All queries are
parameterised; dynamic ORDER BY columns come from fixed allow-lists.
"""

from dataclasses import dataclass


@dataclass
class Page:
    page: int = 1
    page_size: int = 25

    def normalize(self) -> "Page":
        return Page(max(1, self.page), min(200, max(1, self.page_size)))

    @property
    def offset(self) -> int:
        return (self.page - 1) * self.page_size


def escape_like(s: str) -> str:
    return s.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
