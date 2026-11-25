"""Decision desks: one laya Learner (+ Escalator) per kind of decision.

A desk is defined by a JSON file in the desks directory:

    {"name": "line", "description": "...",
     "questions": {qid: {"type": "choice"|"boolean"|"score", "instructions": ..., "criteria": ...}},
     "every": 50, "audit": 0.05, "trust": {qid: 0.95}, "shadow": true}

LICENCE NOTE: Learner and Escalator come from vegaml-learn (PolyForm Noncommercial 1.0.0). This
project is noncommercial. For commercial use, get a licence (https://embedding.in/licence) or set
LAYA_SVC_LEARNING=off, which serves laya's own engine (Apache-2.0) without learning.
"""

from __future__ import annotations

import json
import os
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Dict, Optional

Teacher = Callable[[str, Dict[str, Any]], Optional[Dict[str, Any]]]

VALID_TYPES = {"choice", "boolean", "score"}


@dataclass
class DeskSpec:
    name: str
    questions: Dict[str, Dict[str, Any]]
    description: str = ""
    every: int = 50
    audit: float = 0.05
    trust: Dict[str, float] = field(default_factory=dict)
    shadow: bool = True

    @classmethod
    def load(cls, path: Path) -> "DeskSpec":
        raw = json.loads(path.read_text())
        spec = cls(
            name=raw["name"],
            questions=raw["questions"],
            description=raw.get("description", ""),
            every=int(raw.get("every", 50)),
            audit=float(raw.get("audit", 0.05)),
            trust=dict(raw.get("trust", {})),
            shadow=bool(raw.get("shadow", True)),
        )
        spec.validate()
        return spec

    def validate(self) -> None:
        if not self.questions:
            raise ValueError(f"desk {self.name}: no questions")
        for qid, q in self.questions.items():
            if q.get("type") not in VALID_TYPES:
                raise ValueError(f"desk {self.name}: question {qid} has type {q.get('type')!r}")
            if q["type"] in ("choice", "score") and not q.get("criteria"):
                raise ValueError(f"desk {self.name}: question {qid} needs criteria")
        unknown = set(self.trust) - set(self.questions)
        if unknown:
            raise ValueError(f"desk {self.name}: trust names unknown questions {sorted(unknown)}")


class Backend:
    """What a desk needs from laya. The real one wraps Learner + Escalator; tests use a fake."""

    def decide(self, text: str, shadow: bool) -> Dict[str, Any]: ...
    def feedback(self, text: str, answers: Dict[str, Any], correction: bool) -> Dict[str, Any]: ...
    def report(self) -> Dict[str, Any]: ...
    def held(self) -> Dict[str, Any]: ...
    def accept(self, qid: str) -> None: ...
    def discard(self, qid: str) -> None: ...
    def undo(self) -> Dict[str, Any]: ...
    def save_bundle(self, path: str) -> str: ...
    def close(self) -> None: ...


class LearnerBackend(Backend):
    """laya Learner with an Escalator whose teacher is noc-bot (rules, then the local LLM with tools).

    Held batches are NOT auto-accepted: a batch that sharply disagrees with what was learned waits for a
    person (accept_held=False), because the teacher here is an LLM that can be wrong."""

    def __init__(self, spec: DeskSpec, workdir: Path, teacher: Teacher, model: Optional[str], device: Optional[str]):
        from laya import Escalator, Learner  # vegaml-learn (PolyForm Noncommercial)

        self.learner = Learner(
            spec.questions,
            workdir=str(workdir),
            every=spec.every,
            model=model,
            device=device,
        )
        self.escalator = Escalator(
            self.learner,
            teacher,
            audit=spec.audit,
            trust=spec.trust or 0.95,
            accept_held=False,
        )
        # the reader is not documented as thread-safe: one decision at a time per desk
        self._lock = threading.Lock()

    def decide(self, text, shadow):
        with self._lock:
            return self.escalator.decide(text, shadow=shadow)

    def feedback(self, text, answers, correction):
        return self.learner.feedback(text, answers, correction=correction)

    def report(self):
        return {"escalator": self.escalator.report(), "learner": self.learner.status()}

    def held(self):
        return self.learner.held()

    def accept(self, qid):
        self.learner.accept(qid)

    def discard(self, qid):
        self.learner.discard(qid)

    def undo(self):
        return {"undone": self.learner.undo()}

    def save_bundle(self, path):
        self.learner.save_bundle(path)
        return path

    def close(self):
        with self._lock:
            try:
                # Learner.close() drops feedback still below the batch threshold.
                self.learner.flush()
            finally:
                self.learner.close()


class EngineBackend(Backend):
    """LAYA_SVC_LEARNING=off: laya's own vega engine (Apache-2.0), no learning, no teacher."""

    def __init__(self, spec: DeskSpec, model: Optional[str]):
        import laya

        self.spec = spec
        self.agent = laya.Agent(vegaml=model or True)
        self._lock = threading.Lock()

    def decide(self, text, shadow):
        with self._lock:
            out = self.agent.predict(text, _engine_questions(self.spec.questions))
        for a in out.get("answers", {}).values():
            a["source"] = "vega"
        return out

    def feedback(self, text, answers, correction):
        return {"pending": 0, "learning": False, "note": "learning is off"}

    def report(self):
        return {"learning": "off"}

    def held(self):
        return {}

    def accept(self, qid):
        raise RuntimeError("learning is off")

    def discard(self, qid):
        raise RuntimeError("learning is off")

    def undo(self):
        raise RuntimeError("learning is off")

    def save_bundle(self, path):
        raise RuntimeError("learning is off")

    def close(self):
        pass


def _engine_questions(questions):
    """laya.Agent calls a yes/no question "noul"; vegaml-learn calls it "boolean"."""
    out = {}
    for qid, q in questions.items():
        q = dict(q)
        if q["type"] == "boolean":
            q["type"] = "noul"
        out[qid] = q
    return out


class Desks:
    def __init__(self, specs: Dict[str, DeskSpec], factory: Callable[[DeskSpec], Backend]):
        self.specs = specs
        self._factory = factory
        self._backends: Dict[str, Backend] = {}
        self._lock = threading.Lock()

    def backend(self, name: str) -> Backend:
        if name not in self.specs:
            raise KeyError(name)
        with self._lock:
            b = self._backends.get(name)
            if b is None:
                b = self._factory(self.specs[name])
                self._backends[name] = b
            return b

    def close(self) -> None:
        for b in self._backends.values():
            b.close()


def load_specs(directory: Path) -> Dict[str, DeskSpec]:
    specs = {}
    for p in sorted(directory.glob("*.json")):
        s = DeskSpec.load(p)
        if s.name in specs:
            raise ValueError(f"desk {s.name} defined twice")
        specs[s.name] = s
    if not specs:
        raise ValueError(f"no desks in {directory}")
    return specs


def default_factory(teacher_for: Callable[[str], Teacher]) -> Callable[[DeskSpec], Backend]:
    workroot = Path(os.environ.get("LAYA_SVC_WORKDIR", "./state"))
    model = os.environ.get("LAYA_SVC_MODEL") or None
    device = os.environ.get("LAYA_SVC_DEVICE") or None
    learning = os.environ.get("LAYA_SVC_LEARNING", "on") != "off"

    def make(spec: DeskSpec) -> Backend:
        if not learning:
            return EngineBackend(spec, model)
        wd = workroot / spec.name
        wd.mkdir(parents=True, exist_ok=True)
        return LearnerBackend(spec, wd, teacher_for(spec.name), model, device)

    return make
