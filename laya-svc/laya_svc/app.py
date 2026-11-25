"""HTTP API for the decision desks.

  GET  /health
  GET  /v1/desks                          desk names, descriptions, questions
  POST /v1/desks/{desk}/decide            {"text", "shadow"?}  -> answers with source / route
  POST /v1/desks/{desk}/feedback          {"text", "answers", "correction"?, "origin"}
  GET  /v1/desks/{desk}/report            escalator + learner status (growth metrics)
  GET  /v1/desks/{desk}/held              batches held for a person
  POST /v1/desks/{desk}/held/{qid}/accept | /discard
  POST /v1/desks/{desk}/undo
  POST /v1/desks/{desk}/bundle            save a bundle for the read-only fallback copy

Every route but /health needs "Authorization: Bearer $LAYA_SVC_TOKEN" when that variable is set.

LICENCE NOTE: learning uses vegaml-learn (PolyForm Noncommercial 1.0.0); see laya_svc/__init__.py.
"""

from __future__ import annotations

import hmac
import logging
import os
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any, Dict, Optional

from fastapi import Depends, FastAPI, Header, HTTPException
from pydantic import BaseModel, Field

from .desks import Desks, default_factory, load_specs
from .teacher import HTTPTeacher, NoTeacher

log = logging.getLogger("laya_svc")

ORIGINS = {"human", "outcome", "teacher", "import"}
MAX_TEXT = 200_000  # characters; vega reads up to ~73k tokens


class DecideBody(BaseModel):
    text: str = Field(min_length=1, max_length=MAX_TEXT)
    shadow: Optional[bool] = None


class FeedbackBody(BaseModel):
    text: str = Field(min_length=1, max_length=MAX_TEXT)
    answers: Dict[str, Any]
    correction: bool = False
    # who the answer came from: a person (Slack), a later outcome (probe / Zabbix / complaint), ...
    origin: str = "human"


def create_app(desks: Desks, token: Optional[str]) -> FastAPI:
    @asynccontextmanager
    async def lifespan(_app):
        yield
        desks.close()

    app = FastAPI(title="laya-svc", docs_url=None, redoc_url=None, openapi_url=None, lifespan=lifespan)

    def auth(authorization: Optional[str] = Header(default=None)):
        if not token:
            return
        want = f"Bearer {token}"
        if not authorization or not hmac.compare_digest(authorization, want):
            raise HTTPException(401, "invalid or missing bearer token")

    def backend(desk: str):
        try:
            return desks.backend(desk)
        except KeyError:
            raise HTTPException(404, f"no desk {desk!r}")

    @app.get("/health")
    def health():
        return {"status": "ok", "desks": sorted(desks.specs)}

    @app.get("/v1/desks", dependencies=[Depends(auth)])
    def list_desks():
        return {
            name: {"description": s.description, "questions": s.questions, "shadow": s.shadow}
            for name, s in desks.specs.items()
        }

    @app.post("/v1/desks/{desk}/decide", dependencies=[Depends(auth)])
    def decide(desk: str, body: DecideBody):
        b = backend(desk)
        shadow = desks.specs[desk].shadow if body.shadow is None else body.shadow
        return b.decide(body.text, shadow)

    @app.post("/v1/desks/{desk}/feedback", dependencies=[Depends(auth)])
    def feedback(desk: str, body: FeedbackBody):
        if body.origin not in ORIGINS:
            raise HTTPException(422, f"origin must be one of {sorted(ORIGINS)}")
        b = backend(desk)
        unknown = set(body.answers) - set(desks.specs[desk].questions)
        if unknown:
            raise HTTPException(422, f"unknown questions {sorted(unknown)}")
        log.info("feedback desk=%s origin=%s correction=%s questions=%s", desk, body.origin, body.correction, sorted(body.answers))
        try:
            return b.feedback(body.text, body.answers, body.correction)
        except (ValueError, KeyError, TypeError) as e:
            raise HTTPException(422, str(e))

    @app.get("/v1/desks/{desk}/report", dependencies=[Depends(auth)])
    def report(desk: str):
        return backend(desk).report()

    @app.get("/v1/desks/{desk}/held", dependencies=[Depends(auth)])
    def held(desk: str):
        return backend(desk).held()

    @app.post("/v1/desks/{desk}/held/{qid}/accept", dependencies=[Depends(auth)])
    def accept(desk: str, qid: str):
        backend(desk).accept(qid)
        return {"accepted": qid}

    @app.post("/v1/desks/{desk}/held/{qid}/discard", dependencies=[Depends(auth)])
    def discard(desk: str, qid: str):
        backend(desk).discard(qid)
        return {"discarded": qid}

    @app.post("/v1/desks/{desk}/undo", dependencies=[Depends(auth)])
    def undo(desk: str):
        return backend(desk).undo()

    @app.post("/v1/desks/{desk}/bundle", dependencies=[Depends(auth)])
    def bundle(desk: str):
        root = Path(os.environ.get("LAYA_SVC_BUNDLES", "./bundles"))
        root.mkdir(parents=True, exist_ok=True)
        return {"path": backend(desk).save_bundle(str(root / f"{desk}.zip"))}

    return app


def build_from_env() -> FastAPI:
    specs = load_specs(Path(os.environ.get("LAYA_SVC_DESKS", "./desks")))
    teacher_url = os.environ.get("LAYA_SVC_TEACHER_URL")
    teacher_token = os.environ.get("LAYA_SVC_TEACHER_TOKEN")
    timeout = float(os.environ.get("LAYA_SVC_TEACHER_TIMEOUT", "60"))

    def teacher_for(desk: str):
        if not teacher_url:
            return NoTeacher()
        return HTTPTeacher(teacher_url, teacher_token, desk, timeout=timeout)

    token = os.environ.get("LAYA_SVC_TOKEN")
    if not token:
        log.warning("LAYA_SVC_TOKEN is not set: the API is open to anyone who can reach it")
    return create_app(Desks(specs, default_factory(teacher_for)), token)
