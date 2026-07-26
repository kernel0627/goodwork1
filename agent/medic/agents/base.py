"""The brain interface every control arm implements.

A brain receives an alert and a tool registry and returns a diagnosis. Arms differ
only in the decision procedure -- same contract, same tools -- which is what makes
their numbers comparable. If the checklist arm and the agent arm had different
tools, a difference between them would say nothing about the reasoning.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Protocol

from ..tools.base import Registry

# The fault classes ground truth is expressed in. A brain answering outside this
# set is simply wrong, so the vocabulary is fixed rather than free text.
FAULT_CLASSES = ("error", "latency", "resource", "connectivity", "health", "queue")


@dataclass
class Alert:
    """What the agent is told. Deliberately thin.

    ``service`` is where the alarm fired, which for most scenarios is not the root
    cause. ``summary`` is generated from measured metrics, so it cannot carry a
    hint the metrics do not.
    """

    service: str
    summary: str
    fired_at: str = ""
    observed: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_json(cls, payload: dict[str, Any]) -> "Alert":
        return cls(
            service=payload.get("service", ""),
            summary=payload.get("summary", ""),
            fired_at=payload.get("fired_at", ""),
            observed=payload.get("observed") or {},
        )

    def render(self) -> str:
        lines = [f"ALERT on service: {self.service}", self.summary]
        if self.observed:
            signal = self.observed.get("signal")
            value = self.observed.get("value")
            baseline = self.observed.get("baseline")
            unit = self.observed.get("unit", "")
            lines.append(
                f"measured {signal}={value} (baseline {baseline}) {unit}".strip()
            )
        return "\n".join(l for l in lines if l)


@dataclass
class Budget:
    max_steps: int = 20
    deadline_seconds: int = 300

    @classmethod
    def from_json(cls, payload: dict[str, Any] | None) -> "Budget":
        payload = payload or {}
        return cls(
            max_steps=int(payload.get("max_steps", 20)),
            deadline_seconds=int(payload.get("deadline_seconds", 300)),
        )


@dataclass
class ProposedAction:
    action: str
    target: str
    rationale: str = ""

    def to_json(self) -> dict[str, Any]:
        return {
            "action": self.action,
            "target": self.target,
            "rationale": self.rationale,
        }


@dataclass
class Diagnosis:
    """A brain's answer.

    ``healthy`` is separate from an empty ``root_cause_service`` on purpose. "I
    checked and the system is fine" and "I could not work it out" are different
    outcomes: the first is correct on a healthy control and a missed fault
    otherwise, the second is never correct. Collapsing them would make an agent
    that gives up look like one that cleared the system.
    """

    root_cause_service: str = ""
    root_cause_class: str = ""
    confidence: float = 0.0
    healthy: bool = False
    escalate: bool = False
    steps: int = 0
    tool_calls: list[str] = field(default_factory=list)
    reasoning: str = ""
    remediation: list[ProposedAction] = field(default_factory=list)
    input_tokens: int = 0
    output_tokens: int = 0

    def to_json(self) -> dict[str, Any]:
        return {
            "root_cause_service": self.root_cause_service,
            "root_cause_class": self.root_cause_class,
            "confidence": round(self.confidence, 4),
            "healthy": self.healthy,
            "escalate": self.escalate,
            "steps": self.steps,
            "tool_calls": self.tool_calls,
            "reasoning": self.reasoning,
            "remediation": [a.to_json() for a in self.remediation],
            "input_tokens": self.input_tokens,
            "output_tokens": self.output_tokens,
        }


@dataclass
class Trace:
    """One episode's full record, written to disk for replay and Bad Case work.

    Kept apart from Diagnosis because it is far too large to return over HTTP but
    is the only thing that makes a wrong answer explainable after the fact.
    """

    episode_id: str
    alert: dict[str, Any]
    brain: str
    entries: list[dict[str, Any]] = field(default_factory=list)

    def record(self, kind: str, **fields: Any) -> None:
        self.entries.append({"kind": kind, **fields})

    def to_json(self) -> dict[str, Any]:
        return {
            "episode_id": self.episode_id,
            "brain": self.brain,
            "alert": self.alert,
            "entries": self.entries,
        }


class Brain(Protocol):
    name: str

    def diagnose(
        self,
        alert: Alert,
        registry: Registry,
        budget: Budget,
        trace: Trace,
    ) -> Diagnosis: ...


def normalise_class(value: str) -> str:
    """Map a brain's class answer onto the fixed vocabulary.

    Tolerant of near-misses ("errors", "cpu") because penalising a right answer
    for its wording would measure phrasing rather than diagnosis. Anything with no
    plausible mapping is returned unchanged and scored wrong -- inventing a
    generous default would inflate class accuracy.
    """
    v = (value or "").strip().lower()
    if v in FAULT_CLASSES:
        return v
    aliases = {
        "errors": "error", "failure": "error", "failures": "error",
        "slow": "latency", "slowdown": "latency", "performance": "latency",
        "cpu": "resource", "memory": "resource", "oom": "resource",
        "leak": "resource", "saturation": "resource",
        "network": "connectivity", "unreachable": "connectivity",
        "unavailable": "connectivity", "timeout": "connectivity",
        "healthcheck": "health", "readiness": "health", "liveness": "health",
        "kafka": "queue", "lag": "queue", "backlog": "queue", "async": "queue",
    }
    return aliases.get(v, v)
