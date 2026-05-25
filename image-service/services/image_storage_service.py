"""Minimal local-file image storage.

Vendor's image_storage_service.py was 400+ LOC supporting JSON/SQLite/R2/git
backends, deduplication, retention. We write PNGs to disk under
config.images_dir and return a StoredImage descriptor. That's it.
"""
from __future__ import annotations

import time
import uuid
from dataclasses import dataclass

from services.config import config


@dataclass
class StoredImage:
    rel: str
    url: str
    storage: str
    size: int


class _ImageStorageService:
    def save(self, image_data: bytes, base_url: str | None = None) -> StoredImage:
        rel = f"{int(time.time())}-{uuid.uuid4().hex[:8]}.png"
        path = config.images_dir / rel
        path.write_bytes(image_data)

        # Resolve a public URL: if caller supplied a base_url (resolve_image_base_url
        # does this from the request's Host header), use it; else fall back to
        # config.base_url; else just produce a relative /images/ URL.
        chosen_base = (base_url or config.base_url or "").rstrip("/")
        if chosen_base:
            url = f"{chosen_base}/images/{rel}"
        else:
            url = f"/images/{rel}"
        return StoredImage(rel=rel, url=url, storage="local", size=len(image_data))


image_storage_service = _ImageStorageService()
