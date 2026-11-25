"""The Escalator's teacher: ask noc-bot.

noc-bot answers with, in order: deterministic rules (known issues, the team map), then the local LLM
with tools, which must cite evidence; a question it cannot back with evidence is left out, so nothing
is learned from it. Any failure returns None: the Escalator then serves vega's answer and learns
nothing from the record.
"""

from __future__ import annotations

import logging
from typing import Any, Dict, Optional

import httpx

log = logging.getLogger("laya_svc.teacher")


class HTTPTeacher:
    def __init__(self, url: str, token: Optional[str], desk: str, timeout: float = 60.0, client: Optional[httpx.Client] = None):
        self.url = url
        self.desk = desk
        self.headers = {"User-Agent": "laya-svc/0.1"}
        if token:
            self.headers["Authorization"] = f"Bearer {token}"
        self.client = client or httpx.Client(timeout=timeout)

    def __call__(self, text: str, questions: Dict[str, Any]) -> Optional[Dict[str, Any]]:
        try:
            r = self.client.post(
                self.url,
                json={"desk": self.desk, "text": text, "questions": questions},
                headers=self.headers,
            )
            r.raise_for_status()
            answers = r.json().get("answers") or {}
        except Exception as e:  # never take answering down
            log.warning("teacher failed for desk %s: %r", self.desk, e)
            return None
        # keep only questions we asked about
        return {q: v for q, v in answers.items() if q in questions and v is not None} or None


class NoTeacher:
    """No teacher configured: vega answers alone and only people's feedback teaches it."""

    def __call__(self, text, questions):
        return None
