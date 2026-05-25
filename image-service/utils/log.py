"""Minimal logger using stdlib logging.

Replaces vendor/chatgpt2api/utils/log.py (which had file rotation + custom
formatting we don't need). All call sites use logger.{debug,info,warning,error}.
"""
from __future__ import annotations

import logging

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)-5s %(name)s: %(message)s",
)


class _Logger:
    def __init__(self) -> None:
        self._log = logging.getLogger("image-service")

    def debug(self, msg, *args, **kw):
        self._log.debug(msg, *args, **kw)

    def info(self, msg, *args, **kw):
        self._log.info(msg, *args, **kw)

    def warning(self, msg, *args, **kw):
        self._log.warning(msg, *args, **kw)

    def error(self, msg, *args, **kw):
        self._log.error(msg, *args, **kw)


logger = _Logger()
