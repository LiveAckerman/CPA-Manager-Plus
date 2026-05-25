"""Image generation routes (slim).

The original ai.py exposed /v1/models, /v1/images/{generations,edits},
/v1/chat/completions, /v1/responses, /v1/messages — the full OpenAI +
Anthropic surface. Image-service only needs the two image endpoints.
"""
from __future__ import annotations

from fastapi import APIRouter, Header, HTTPException, Request
from fastapi.concurrency import run_in_threadpool
from pydantic import BaseModel, Field

from api.image_inputs import parse_image_edit_request, read_image_sources
from api.support import (
    raise_image_quota_error,
    require_identity,
    resolve_image_base_url,
)
from services.protocol import openai_v1_image_edit, openai_v1_image_generations


class ImageGenerationRequest(BaseModel):
    prompt: str = Field(..., min_length=1)
    model: str = "gpt-image-2"
    n: int = Field(default=1, ge=1, le=4)
    size: str | None = None
    response_format: str = "b64_json"
    history_disabled: bool = True
    stream: bool | None = None


def create_router() -> APIRouter:
    router = APIRouter()

    @router.post("/v1/images/generations")
    async def generate_images(
        body: ImageGenerationRequest,
        request: Request,
        authorization: str | None = Header(default=None),
    ):
        require_identity(authorization)
        payload = body.model_dump(mode="python")
        payload["base_url"] = resolve_image_base_url(request)
        try:
            return await run_in_threadpool(openai_v1_image_generations.handle, payload)
        except HTTPException:
            raise
        except Exception as exc:
            raise_image_quota_error(exc)

    @router.post("/v1/images/edits")
    async def edit_images(
        request: Request,
        authorization: str | None = Header(default=None),
    ):
        require_identity(authorization)
        payload, image_sources = await parse_image_edit_request(request)
        payload["images"] = await read_image_sources(image_sources)
        payload["base_url"] = resolve_image_base_url(request)
        try:
            return await run_in_threadpool(openai_v1_image_edit.handle, payload)
        except HTTPException:
            raise
        except Exception as exc:
            raise_image_quota_error(exc)

    return router
