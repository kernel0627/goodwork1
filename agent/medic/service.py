"""The agent HTTP service: a thin shell around a swappable brain.

Contract in docs/agent-api.md. The shell owns everything that must be identical
across control arms -- tool construction, trace capture, budget enforcement, error
handling -- so that a difference between arms reflects their reasoning and nothing
else.

Run with:

    MEDIC_BRAIN=random python -m medic.service
"""

from __future__ import annotations

import json
import os
import time
import traceback
from pathlib import Path
from typing import Any

from fastapi import FastAPI
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field

from .agents.base import Alert, Brain, Budget, Diagnosis, Trace
from .agents.random_brain import RandomBrain
from .tools.base import Registry
from .tools.logs import register_log_tools
from .tools.prom import Prometheus, register_metric_tools
from .tools.traces import Jaeger, register_trace_tools

TRACE_DIR = Path(os.getenv("MEDIC_TRACE_DIR", "results/traces"))

# Tools an ablation can withhold, as a comma-separated list of names. Applied here
# rather than inside a brain so every arm is ablated identically.
WITHHELD_TOOLS = [t for t in os.getenv("MEDIC_WITHHOLD_TOOLS", "").split(",") if t]


def build_registry() -> Registry:
    registry = Registry()
    register_metric_tools(registry, Prometheus())
    register_trace_tools(registry, Jaeger())
    register_log_tools(registry)
    if WITHHELD_TOOLS:
        registry = registry.without(*WITHHELD_TOOLS)
    return registry


def build_brain(name: str) -> Brain:
    if name == "random":
        seed = os.getenv("MEDIC_SEED")
        return RandomBrain(seed=int(seed) if seed else None)
    # The real arms land here as they are written. Failing loudly on an unknown
    # name matters: silently falling back to a default would mean a run labelled
    # as one arm was actually another, and the comparison would be worthless.
    raise ValueError(
        f"unknown brain {name!r}; available: random "
        "(checklist, oneshot, react, medic not yet implemented)"
    )


class AlertModel(BaseModel):
    service: str
    summary: str = ""
    fired_at: str = ""
    observed: dict[str, Any] = Field(default_factory=dict)


class BudgetModel(BaseModel):
    max_steps: int = 20
    deadline_seconds: int = 300


class DiagnoseRequest(BaseModel):
    episode_id: str
    alert: AlertModel
    budget: BudgetModel = Field(default_factory=BudgetModel)


app = FastAPI(title="medic-agent")

_brain: Brain | None = None
_registry: Registry | None = None


def _ensure_ready() -> tuple[Brain, Registry]:
    global _brain, _registry
    if _brain is None:
        _brain = build_brain(os.getenv("MEDIC_BRAIN", "random"))
    if _registry is None:
        _registry = build_registry()
    return _brain, _registry


@app.get("/healthz")
def healthz() -> dict[str, Any]:
    """Polled before a run starts, so a misconfigured agent fails in the first
    second rather than forty minutes in."""
    try:
        brain, registry = _ensure_ready()
    except Exception as exc:  # noqa: BLE001
        return {"ok": False, "error": str(exc)}
    return {
        "ok": True,
        "brain": brain.name,
        "tools": len(registry.names()),
        "withheld": WITHHELD_TOOLS,
    }


@app.post("/diagnose")
def diagnose(request: DiagnoseRequest) -> JSONResponse:
    brain, registry = _ensure_ready()

    alert = Alert.from_json(request.alert.model_dump())
    budget = Budget(
        max_steps=request.budget.max_steps,
        deadline_seconds=request.budget.deadline_seconds,
    )
    trace = Trace(
        episode_id=request.episode_id,
        alert=request.alert.model_dump(),
        brain=brain.name,
    )

    started = time.monotonic()
    try:
        diagnosis = brain.diagnose(alert, registry, budget, trace)
    except Exception as exc:  # noqa: BLE001
        # Returned as a 500 with the traceback in the trace file. The runner marks
        # the episode invalid rather than scoring it, since a crashed brain
        # produced no answer to grade.
        trace.record("error", error=str(exc), traceback=traceback.format_exc())
        _write_trace(trace)
        return JSONResponse(
            status_code=500,
            content={"error": f"{type(exc).__name__}: {exc}"},
        )

    elapsed = time.monotonic() - started
    trace.record("timing", elapsed_seconds=round(elapsed, 3))
    _write_trace(trace)

    payload = diagnosis.to_json()
    payload["elapsed_seconds"] = round(elapsed, 3)
    return JSONResponse(content=payload)


def _write_trace(trace: Trace) -> None:
    """Persist the trace, but never let a write failure lose the diagnosis.

    Trace capture is for later analysis; an unwritable directory is a nuisance,
    while a lost episode in a 40-minute run is expensive.
    """
    try:
        TRACE_DIR.mkdir(parents=True, exist_ok=True)
        safe_id = "".join(c if c.isalnum() or c in "-_." else "_" for c in trace.episode_id)
        path = TRACE_DIR / f"{trace.brain}-{safe_id}.json"
        path.write_text(json.dumps(trace.to_json(), ensure_ascii=False, indent=2))
    except OSError as exc:
        print(f"warning: could not write trace for {trace.episode_id}: {exc}")


def main() -> None:
    import uvicorn

    host, _, port = os.getenv("AGENT_ADDR", "127.0.0.1:7802").rpartition(":")
    uvicorn.run(app, host=host or "127.0.0.1", port=int(port), log_level="warning")


if __name__ == "__main__":
    main()
