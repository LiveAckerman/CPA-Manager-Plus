"""Minimal proxy settings.

Real proxy_service.py was 150+ LOC supporting per-account proxy config from
the panel. We don't surface that: image-service inherits whatever HTTP_PROXY /
HTTPS_PROXY env vars the container has, and otherwise speaks directly.
"""
from __future__ import annotations

import os
from typing import Any


class _ProxySettings:
    def build_session_kwargs(self, **overrides: Any) -> dict[str, Any]:
        """Returns kwargs to pass into curl_cffi.requests.Session(...).

        Honors HTTPS_PROXY / HTTP_PROXY env vars if set; otherwise no proxy.
        """
        kwargs: dict[str, Any] = dict(overrides)
        proxy = (
            os.environ.get("HTTPS_PROXY")
            or os.environ.get("https_proxy")
            or os.environ.get("HTTP_PROXY")
            or os.environ.get("http_proxy")
            or ""
        ).strip()
        if proxy:
            kwargs.setdefault("proxies", {"http": proxy, "https": proxy})
        return kwargs

    def get_proxy_url(self) -> str:
        return (
            os.environ.get("HTTPS_PROXY")
            or os.environ.get("https_proxy")
            or os.environ.get("HTTP_PROXY")
            or os.environ.get("http_proxy")
            or ""
        ).strip()


proxy_settings = _ProxySettings()
