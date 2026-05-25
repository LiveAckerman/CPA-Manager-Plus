# image-service

ChatGPT-web image generation, exposed as an OpenAI-compatible HTTP service
running on `127.0.0.1:8000` inside the container. The Go reverse proxy in
`usage-service/` is what the outside world talks to.

## Why this exists

The original integration ran [`basketikun/chatgpt2api`](https://github.com/basketikun/chatgpt2api)
as a vendored submodule. That worked, but chatgpt2api maintained its own copy
of the account pool inside `accounts.json`, which drifted out of sync with
CPA every time CPA's silent OAuth refresh rotated an `access_token`.

This rewrite keeps the ChatGPT reverse-engineering code (PoW solver, Turnstile
solver, TLS impersonation, conversation flow) and replaces the data layer:

* No `accounts.json`. CPA is the single source of truth.
* `services/account_service.py` is the only file fully rewritten — it talks to
  CPA's `/v0/management/auth-files{,/download}` on demand, caching the file
  list briefly and re-downloading tokens lazily / on 401.
* Everything else (`openai_backend_api.py`, `conversation.py`, `pow.py`,
  `turnstile.py`, `helper.py`) is unchanged from the upstream snapshot.

## What's kept vs dropped from upstream

| Kept (verbatim, ~2,400 LOC) | Dropped (was ~5,000 LOC) |
|---|---|
| ChatGPT protocol (`openai_backend_api.py`) | Their own Next.js web panel |
| Image-gen orchestration (`conversation.py`) | `backup_service`, `image_storage/` multi-backend |
| Image protocol shells (`openai_v1_image_*.py`) | `cpa_service.py` (we go direct to CPA now) |
| PoW + Turnstile (`utils/pow.py`, `utils/turnstile.py`) | Registration flow (`api/register.py`) |
| Helper utilities (`utils/helper.py`) | `api/system.py` (logs/settings/backups endpoints) |
| Image input parsing (`api/image_inputs.py`) | Chat completions / Anthropic messages / responses routes |
| | `auth_service` (multi-user; replaced by single-key check) |

## Running

Inside the container, `s6` launches:

```
/opt/image-service/.venv/bin/uvicorn main:app --host 127.0.0.1 --port 8000
```

Env vars consumed:

| Var | Default | Purpose |
|---|---|---|
| `CHATGPT2API_AUTH_KEY` | (required) | Internal Bearer token — Go usage-service injects on every call. Lives in `/run/chatgpt2api_internal_key`. |
| `CPA_BASE_URL` | (required) | CPA upstream URL — used by account_service for the token pool. |
| `CPA_MANAGEMENT_KEY` | (required) | CPA Management Key — admin auth for the auth-files endpoints. |
| `CHATGPT2API_DATA_DIR` | `/data/chatgpt-image` | Where saved PNGs land. |
| `CPA_LIST_CACHE_SECS` | `15` | How long to trust CPA's file-list response before re-pulling. |
| `REFRESH_ACCOUNT_INTERVAL_MIN` | `5` | Background CPA list refresh cadence. |
| `IMAGE_ACCOUNT_CONCURRENCY` | `3` | Max concurrent image-gen calls per account. |

## Tests

```bash
cd image-service
uv sync
uv run pytest
```

Tests live in `tests/` and exercise `account_service` against a mocked CPA
HTTP server.
