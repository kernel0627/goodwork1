"""A brain that guesses, used to validate the harness before any real arm exists.

This is not a baseline. It is a test of the scoring framework.

A scoring framework can be wrong in ways that produce entirely plausible numbers:
ground truth read from the wrong field, symptom echo counted backwards, discarded
episodes quietly dropped from a denominator. Every one of those yields a run that
completes and reports something believable, and none of them announces itself.

Against a guesser the expected results are known in advance:

* exact accuracy near ``1/n_services × 1/n_classes`` -- a few percent
* symptom echo near chance, not near zero
* a false alarm on nearly every healthy control
* discards only where a fault genuinely failed to fire

If this brain scores well, the bug is in the scoring rather than in its luck. Only
once these numbers come out as predicted is a real arm worth writing.
"""

from __future__ import annotations

import random
from typing import Any

from ..tools.base import Registry
from .base import FAULT_CLASSES, Alert, Brain, Budget, Diagnosis, Trace

# Arguments good enough to exercise each tool. The point is to make real calls --
# a guesser that never touches the tools would not exercise the trace-recording
# and truncation paths the real arms depend on.
_SAMPLE_ARGS: dict[str, list[dict[str, Any]]] = {
    "list_services": [{}],
    "get_queue_lag": [{}],
    "get_service_topology": [{}],
    "get_service_health": [{"service": "checkout"}, {"service": "cart"}],
    "get_endpoint_breakdown": [{"service": "cart"}, {"service": "product-catalog"}],
    "get_client_calls": [{"caller": "checkout"}, {"caller": "frontend"}],
    "get_resource_usage": [{"container": "ad"}, {"container": "recommendation"}],
    "find_error_traces": [{"service": "frontend", "lookback": "10m"}],
    "query_logs": [{"service": "cart", "since": "5m", "limit": 10}],
    "find_error_logs": [{"service": "checkout", "since": "5m"}],
    "promql": [{"query": "up"}],
}


class RandomBrain(Brain):
    name = "random"

    def __init__(self, seed: int | None = None):
        # Seeded so a suspicious run can be reproduced exactly. A harness bug
        # found once and then not reproducible is a bug that does not get fixed.
        self.rng = random.Random(seed)

    def diagnose(
        self,
        alert: Alert,
        registry: Registry,
        budget: Budget,
        trace: Trace,
    ) -> Diagnosis:
        # Discover services through the tools, exactly as a real brain must. A
        # hardcoded list would let this brain answer names the real arms would
        # have to find first, making its guesses unrepresentatively lucky.
        services = self._discover_services(registry, trace)

        steps = self.rng.randint(1, max(1, min(budget.max_steps, 6)))
        called: list[str] = []
        available = [n for n in registry.names() if n in _SAMPLE_ARGS]

        for _ in range(steps):
            if not available:
                break
            name = self.rng.choice(available)
            args = self.rng.choice(_SAMPLE_ARGS[name])
            result = registry.call(name, args)
            called.append(name)
            trace.record(
                "tool_call",
                tool=name,
                arguments=args,
                ok=result.ok,
                truncated=result.truncated,
                content=result.content[:1500],
            )

        # Occasionally decline to answer, so the harness's handling of "healthy"
        # and "escalate" is exercised rather than only its handling of a guess.
        roll = self.rng.random()
        if roll < 0.08:
            diagnosis = Diagnosis(healthy=True, confidence=self.rng.random(), reasoning="random: healthy")
        elif roll < 0.16:
            diagnosis = Diagnosis(escalate=True, confidence=self.rng.random(), reasoning="random: escalate")
        else:
            diagnosis = Diagnosis(
                root_cause_service=self.rng.choice(services) if services else alert.service,
                root_cause_class=self.rng.choice(FAULT_CLASSES),
                confidence=self.rng.random(),
                reasoning="random: guessed",
            )

        diagnosis.steps = len(called)
        diagnosis.tool_calls = called
        trace.record("diagnosis", **diagnosis.to_json())
        return diagnosis

    def _discover_services(self, registry: Registry, trace: Trace) -> list[str]:
        result = registry.call("list_services")
        trace.record("tool_call", tool="list_services", ok=result.ok, content=result.content[:1500])
        if result.ok and result.raw:
            return list(result.raw.get("services", []))
        return []
