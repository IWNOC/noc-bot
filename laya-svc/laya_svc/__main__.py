import logging
import os

import uvicorn

from .app import build_from_env

logging.basicConfig(level=os.environ.get("LAYA_SVC_LOG_LEVEL", "INFO"))
uvicorn.run(
    build_from_env(),
    host=os.environ.get("LAYA_SVC_HOST", "127.0.0.1"),
    port=int(os.environ.get("LAYA_SVC_PORT", "8100")),
)
