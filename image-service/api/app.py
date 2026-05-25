"""FastAPI app for image-service.

Slim version of vendor's api/app.py: only image routes + health + read-only
accounts diagnostic. No Next.js web UI, no backup endpoints, no register
flow, no chat/responses/anthropic, no logs/settings management.
"""
from __future__ import annotations

from contextlib import asynccontextmanager

from fastapi import FastAPI, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse

from api import ai
from services.account_service import account_service
from services.config import config


def create_app() -> FastAPI:
    @asynccontextmanager
    async def lifespan(_: FastAPI):
        account_service.start_background_refresh()
        try:
            yield
        finally:
            account_service.stop_background_refresh()

    app = FastAPI(title="image-service", version="0.1.0", lifespan=lifespan)
    app.add_middleware(
        CORSMiddleware,
        allow_origins=["*"],
        allow_credentials=False,
        allow_methods=["*"],
        allow_headers=["*"],
    )
    app.include_router(ai.create_router())

    @app.get("/health")
    async def health():
        return {
            "status": "ok",
            "service": "image-service",
            "accounts_cached": account_service.account_count(),
        }

    @app.get("/api/accounts")
    async def list_accounts(authorization: str | None = None):
        # Diagnostic: returns redacted account list so operators can see
        # what's in the pool without exposing access tokens.
        return {"items": account_service.list_accounts_redacted()}

    @app.get("/images/{image_path:path}", include_in_schema=False)
    async def serve_image(image_path: str):
        # Serve the locally-stored PNG so URLs returned by image_storage_service
        # actually work when clients fetch them.
        target = (config.images_dir / image_path).resolve()
        try:
            target.relative_to(config.images_dir.resolve())
        except ValueError:
            raise HTTPException(status_code=404, detail="not found")
        if not target.is_file():
            raise HTTPException(status_code=404, detail="not found")
        return FileResponse(target)

    return app
