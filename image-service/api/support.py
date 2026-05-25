"""Trimmed support helpers.

Original (vendor/chatgpt2api/api/support.py) was 119 LOC handling multi-user
auth via auth_service, web-asset resolution, sanitize helpers for CPA + sub2api
pools, and a background "limited account" watcher. Image-service only does
image gen, so all of that drops. What remains:

  - extract_bearer_token + require_identity: check the single internal auth
    key (config.auth_key) — the same one the Go reverse-proxy injects on
    every call. No multi-user, no DB.
  - resolve_image_base_url: produce a stable URL prefix for stored images.
  - raise_image_quota_error: convert "no available image quota" into 429.
"""
from __future__ import annotations

from fastapi import HTTPException, Request

from services.config import config


def extract_bearer_token(authorization: str | None) -> str:
    scheme, _, value = str(authorization or "").partition(" ")
    if scheme.lower() != "bearer" or not value.strip():
        return ""
    return value.strip()


def require_identity(authorization: str | None) -> dict[str, object]:
    """Single-key check against the internal Bearer token injected by the Go
    reverse proxy. Returns an opaque identity dict on success."""
    token = extract_bearer_token(authorization)
    expected = (config.auth_key or "").strip()
    if not expected:
        # No auth configured (development mode). Allow all.
        return {"id": "anon", "role": "anon"}
    if token != expected:
        raise HTTPException(status_code=401, detail={"error": "invalid auth key"})
    return {"id": "internal", "role": "admin"}


def resolve_image_base_url(request: Request) -> str:
    if config.base_url:
        return config.base_url
    return f"{request.url.scheme}://{request.headers.get('host', request.url.netloc)}"


def raise_image_quota_error(exc: Exception) -> None:
    message = str(exc)
    if "no available image quota" in message.lower():
        raise HTTPException(
            status_code=429,
            detail={"error": {"code": "insufficient_quota", "message": message}},
        ) from exc
    raise HTTPException(status_code=502, detail={"error": message}) from exc
