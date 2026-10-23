"""Serves the built frontend from STATIC_DIR (all-in-one deployment), falling
back to index.html for client-side routes. Paths are resolved and checked to
stay inside the directory, so "..", absolute paths and symlinks cannot escape it."""

from __future__ import annotations

from pathlib import Path

from fastapi.responses import FileResponse, Response

# Matches the policy nginx sets in the multi-service deployment.
SPA_CSP = ("default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "
           "font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")


class StaticSite:
    def __init__(self, directory: str):
        self.root = Path(directory).resolve()
        self.index = self.root / "index.html"

    def _resolve(self, rel: str) -> Path | None:
        rel = rel.lstrip("/")
        if not rel or "\x00" in rel:
            return None
        try:
            candidate = (self.root / rel).resolve()
        except (OSError, ValueError):
            return None
        if not candidate.is_relative_to(self.root) or not candidate.is_file():
            return None
        return candidate

    def serve(self, path: str) -> Response:
        f = self._resolve(path)
        headers = {"Content-Security-Policy": SPA_CSP}
        if f is not None:
            rel = f.relative_to(self.root).as_posix()
            # Vite fingerprints asset names, so they never change.
            headers["Cache-Control"] = "public, max-age=31536000, immutable" if rel.startswith("assets/") else "no-cache"
            return FileResponse(f, headers=headers)
        # Unknown path: let the SPA router handle it.
        headers["Cache-Control"] = "no-cache"
        return FileResponse(self.index, headers=headers, media_type="text/html")
