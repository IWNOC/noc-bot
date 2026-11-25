from pathlib import Path
import threading
from unittest.mock import Mock, call

import httpx
import pytest
from fastapi.testclient import TestClient

from laya_svc.app import create_app
from laya_svc.desks import Backend, Desks, DeskSpec, LearnerBackend, load_specs
from laya_svc.teacher import HTTPTeacher

DESKS = Path(__file__).resolve().parent.parent / "desks"


@pytest.mark.parametrize("flush_fails", [False, True])
def test_shutdown_flushes_unlearned_feedback_before_closing(flush_fails):
    backend = LearnerBackend.__new__(LearnerBackend)
    backend._lock = threading.Lock()
    backend.learner = Mock()
    if flush_fails:
        backend.learner.flush.side_effect = RuntimeError("save failed")
        with pytest.raises(RuntimeError, match="save failed"):
            backend.close()
    else:
        backend.close()
    assert backend.learner.mock_calls == [call.flush(), call.close()]


class FakeBackend(Backend):
    def __init__(self, spec):
        self.spec = spec
        self.fed = []
        self.decided = []

    def decide(self, text, shadow):
        self.decided.append((text, shadow))
        return {"answers": {q: {"answer": "x", "source": "vega", "route": "review"} for q in self.spec.questions}}

    def feedback(self, text, answers, correction):
        self.fed.append((text, answers, correction))
        return {"pending": len(self.fed), "learning": False}

    def report(self):
        return {"escalator": {"teacher_share_recent": 1.0}}

    def held(self):
        return {}

    def accept(self, qid):
        pass

    def discard(self, qid):
        pass

    def undo(self):
        return {"undone": True}

    def save_bundle(self, path):
        return path

    def close(self):
        pass


@pytest.fixture
def setup():
    specs = load_specs(DESKS)
    made = {}

    def factory(spec):
        made[spec.name] = FakeBackend(spec)
        return made[spec.name]

    client = TestClient(create_app(Desks(specs, factory), token="secret"))
    return client, made


AUTH = {"Authorization": "Bearer secret"}


def test_all_desks_load():
    specs = load_specs(DESKS)
    assert set(specs) == {"line", "window", "incident", "comment", "action", "helpdesk"}


def test_bad_question_type_rejected():
    with pytest.raises(ValueError):
        DeskSpec(name="x", questions={"q": {"type": "noul", "instructions": "?"}}).validate()


def test_health_needs_no_auth(setup):
    client, _ = setup
    assert client.get("/health").json()["status"] == "ok"


def test_auth_required(setup):
    client, _ = setup
    assert client.post("/v1/desks/line/decide", json={"text": "x"}).status_code == 401
    assert client.post("/v1/desks/line/decide", json={"text": "x"}, headers={"Authorization": "Bearer nope"}).status_code == 401


def test_decide_uses_desk_shadow_default(setup):
    client, made = setup
    r = client.post("/v1/desks/line/decide", json={"text": "BGP down"}, headers=AUTH)
    assert r.status_code == 200
    assert made["line"].decided == [("BGP down", True)]
    client.post("/v1/desks/line/decide", json={"text": "y", "shadow": False}, headers=AUTH)
    assert made["line"].decided[-1] == ("y", False)


def test_unknown_desk_404(setup):
    client, _ = setup
    assert client.post("/v1/desks/nope/decide", json={"text": "x"}, headers=AUTH).status_code == 404


def test_feedback_validates_questions_and_origin(setup):
    client, made = setup
    ok = client.post("/v1/desks/line/feedback", json={"text": "t", "answers": {"odd": True}, "correction": True}, headers=AUTH)
    assert ok.status_code == 200
    assert made["line"].fed == [("t", {"odd": True}, True)]
    bad_q = client.post("/v1/desks/line/feedback", json={"text": "t", "answers": {"severity": 1}}, headers=AUTH)
    assert bad_q.status_code == 422
    bad_o = client.post("/v1/desks/line/feedback", json={"text": "t", "answers": {"odd": True}, "origin": "llm-guess"}, headers=AUTH)
    assert bad_o.status_code == 422


def test_teacher_filters_and_survives_errors():
    def handler(request: httpx.Request):
        body = request.read()
        assert b'"desk":"line"' in body.replace(b" ", b"")
        return httpx.Response(200, json={"answers": {"odd": True, "extra": "x", "team": None}})

    t = HTTPTeacher("http://bot/v1/teacher", "tok", "line", client=httpx.Client(transport=httpx.MockTransport(handler)))
    assert t("text", {"odd": {}, "team": {}}) == {"odd": True}

    def boom(request):
        return httpx.Response(500)

    t2 = HTTPTeacher("http://bot/v1/teacher", None, "line", client=httpx.Client(transport=httpx.MockTransport(boom)))
    assert t2("text", {"odd": {}}) is None
