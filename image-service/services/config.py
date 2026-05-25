"""Minimal config singleton.

Replaces vendor/chatgpt2api/services/config.py (~400 LOC of file-watching
JSON config + panel mutations) with env-driven knobs. Image-service only
needs a handful of properties; everything else is dropped.
"""
from __future__ import annotations

import os
from pathlib import Path


def _env_int(key: str, default: int) -> int:
    try:
        return int(os.environ.get(key, "").strip() or default)
    except (TypeError, ValueError):
        return default


def _env_float(key: str, default: float) -> float:
    try:
        return float(os.environ.get(key, "").strip() or default)
    except (TypeError, ValueError):
        return default


class _Config:
    """Runtime config; all values resolved at import time from env vars."""

    # ---- internal auth key the Go reverse-proxy injects on every call ----
    # Phase 2's docker/scripts/init-auth-key.sh writes a 64-char hex token
    # into /run/chatgpt2api_internal_key, then the s6 run script exports
    # CHATGPT2API_AUTH_KEY before exec'ing this process. require_identity
    # in api/support.py compares Bearer tokens against this.
    auth_key: str = os.environ.get("CHATGPT2API_AUTH_KEY", "").strip()

    # ---- CPA upstream the account_service reads tokens from ----
    cpa_base_url: str = os.environ.get("CPA_BASE_URL", "").strip()
    cpa_management_key: str = os.environ.get("CPA_MANAGEMENT_KEY", "").strip()

    # ---- image-gen polling cadence (consumed by openai_backend_api &
    #      conversation.stream_image_outputs) ----
    image_poll_timeout_secs: int = _env_int("IMAGE_POLL_TIMEOUT_SECS", 120)
    image_poll_interval_secs: float = _env_float("IMAGE_POLL_INTERVAL_SECS", 10.0)
    image_poll_initial_wait_secs: float = _env_float("IMAGE_POLL_INITIAL_WAIT_SECS", 10.0)
    image_account_concurrency: int = _env_int("IMAGE_ACCOUNT_CONCURRENCY", 3)

    # ---- account pool refresh cadence (in minutes; consumed by
    #      account_service's background refresher) ----
    refresh_account_interval_minute: int = _env_int("REFRESH_ACCOUNT_INTERVAL_MIN", 5)

    # ---- CPA file-list cache invalidation; how stale CPA's list can be ----
    cpa_list_cache_secs: float = _env_float("CPA_LIST_CACHE_SECS", 15.0)

    # ---- prompt shaping ----
    global_system_prompt: str = os.environ.get("GLOBAL_SYSTEM_PROMPT", "").strip()

    # ---- where saved images live; URL clients see when response_format=url ----
    base_url: str = os.environ.get("IMAGE_PUBLIC_BASE_URL", "").strip()

    @property
    def data_dir(self) -> Path:
        d = Path(os.environ.get("CHATGPT2API_DATA_DIR", "/data/chatgpt-image"))
        d.mkdir(parents=True, exist_ok=True)
        return d

    @property
    def images_dir(self) -> Path:
        d = self.data_dir / "images"
        d.mkdir(parents=True, exist_ok=True)
        return d


config = _Config()
