"""image-service entry point.

Started by s6 with:
    /opt/image-service/.venv/bin/uvicorn main:app --host 127.0.0.1 --port 8000
"""
from __future__ import annotations

import uvicorn

from api.app import create_app

app = create_app()

if __name__ == "__main__":
    uvicorn.run(app, host="127.0.0.1", port=8000, access_log=False, log_level="info")
