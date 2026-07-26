"""The fixed-checklist arm. This is the control that matters.

The question the whole project turns on is whether multi-step autonomous
diagnosis beats a good runbook. This arm is that runbook: a fixed sequence of
tool calls, then one model call to conclude from everything gathered.

# It is deliberately the strongest checklist I can write

A checklist that only inspected the alerting service would lose every L2 and L3
scenario by construction, and beating it would prove nothing. So this one expands
one hop along the measured topology and checks the alerting service's
dependencies too -- which is exactly what a competent runbook says to do.

What it does not do is decide *what to look at next based on what it found*.
Every call is determined before the episode starts; only the parameters come from
the topology. That is the honest line between a checklist and an agent, and it is
the thing being measured.

If the agent arm cannot beat this, that is the result, and it gets reported.
"""

from __future__ import annotations

from typing import Any

from ..llm import DIAGNOSIS_TOOL, SYSTEM_PROMPT, LLM, LLMError
from ..tools.base import Registry, ToolResult
from .base import (
    Alert,
    Brain,
    Budget,
    Diagnosis,
    ProposedAction,
    Trace,
    normalise_class,
)


class ChecklistBrain(Brain):
    name = "checklist"

    def __init__(self, model: str | None = None, max_dependencies: int = 4):
        self.llm = LLM(model=model)
        self.max_dependencies = max_dependencies

    def diagnose(
        self,
        alert: Alert,
        registry: Registry,
        budget: Budget,
        trace: Trace,
    ) -> Diagnosis:
        gathered: list[tuple[str, dict[str, Any], ToolResult]] = []

        def run(tool: str, **args: Any) -> ToolResult:
            result = registry.call(tool, args)
            gathered.append((tool, args, result))
            trace.record(
                "tool_call",
                tool=tool,
                arguments=args,
                ok=result.ok,
                truncated=result.truncated,
                content=result.content[:4000],
            )
            return result

        service = alert.service

        # Fixed sequence over the alerting service.
        run("list_services")
        run("get_service_health", service=service)
        topology = run("get_service_topology", service=service)
        run("get_endpoint_breakdown", service=service)
        run("get_resource_usage", container=service)
        run("find_error_traces", service=service, lookback="15m")
        run("find_error_logs", service=service, since="10m")
        run("get_queue_lag")

        # One hop out. Parameterised by the measured topology, but the decision to
        # look at dependencies at all was made before the episode began.
        for dependency in self._dependencies_of(service, topology)[: self.max_dependencies]:
            if len(gathered) >= budget.max_steps:
                break
            run("get_service_health", service=dependency)
            run("get_client_calls", caller=service)

        return self._conclude(alert, gathered, trace)

    @staticmethod
    def _dependencies_of(service: str, topology: ToolResult) -> list[str]:
        """Callees of `service`, busiest first.

        Only outbound edges: a fault in something the alerting service calls can
        explain its symptom, whereas a fault in something that calls it cannot.
        """
        if not topology.ok or not topology.raw:
            return []
        edges = topology.raw.get("edges", [])
        callees = [
            (e.get("child"), e.get("callCount", 0))
            for e in edges
            if e.get("parent") == service and e.get("child")
        ]
        callees.sort(key=lambda pair: -pair[1])
        return [name for name, _ in callees]

    def _conclude(
        self,
        alert: Alert,
        gathered: list[tuple[str, dict[str, Any], ToolResult]],
        trace: Trace,
    ) -> Diagnosis:
        sections = [f"# Alert\n\n{alert.render()}\n", "# Evidence gathered\n"]
        for tool, args, result in gathered:
            arg_text = ", ".join(f"{k}={v}" for k, v in args.items())
            body = result.content
            if result.truncated:
                body += "\n(result truncated)"
            sections.append(f"## {tool}({arg_text})\n\n{body}\n")

        messages = [{"role": "user", "content": "\n".join(sections)}]
        try:
            turn = self.llm.complete(
                system=SYSTEM_PROMPT,
                messages=messages,
                tools=[DIAGNOSIS_TOOL],
                force_tool=DIAGNOSIS_TOOL["name"],
            )
        except LLMError as exc:
            trace.record("llm_error", error=str(exc))
            # Escalating rather than raising: a model failure is not the agent
            # being wrong, and the runner records escalations separately from
            # wrong answers.
            return Diagnosis(
                escalate=True,
                steps=len(gathered),
                tool_calls=[t for t, _, _ in gathered],
                reasoning=f"model call failed: {exc}",
            )

        diagnosis = build_diagnosis(turn, len(gathered), [t for t, _, _ in gathered])
        trace.record("diagnosis", **diagnosis.to_json())
        return diagnosis


def build_diagnosis(turn, steps: int, tool_calls: list[str]) -> Diagnosis:
    """Turn a forced tool call into a Diagnosis. Shared by the model-driven arms."""
    payload: dict[str, Any] = {}
    for call in turn.tool_calls:
        if call["name"] == DIAGNOSIS_TOOL["name"]:
            payload = call["input"]
            break

    if not payload:
        # The schema was forced, so an empty payload means something went wrong
        # upstream rather than the model declining. Escalate rather than invent.
        return Diagnosis(
            escalate=True,
            steps=steps,
            tool_calls=tool_calls,
            reasoning=turn.text or "model returned no diagnosis",
            input_tokens=turn.usage.input_tokens,
            output_tokens=turn.usage.output_tokens,
        )

    healthy = bool(payload.get("healthy"))
    service = (payload.get("root_cause_service") or "").strip()
    # An answer cannot be both healthy and a named cause. Trusting `healthy` is
    # the conservative reading: it is the claim the model made explicitly.
    if healthy:
        service = ""

    return Diagnosis(
        root_cause_service=service,
        root_cause_class=normalise_class(payload.get("root_cause_class", "")),
        confidence=float(payload.get("confidence") or 0.0),
        healthy=healthy,
        escalate=bool(payload.get("escalate")),
        steps=steps,
        tool_calls=tool_calls,
        reasoning=payload.get("reasoning", ""),
        remediation=[
            ProposedAction(
                action=item.get("action", ""),
                target=item.get("target", ""),
                rationale=item.get("rationale", ""),
            )
            for item in payload.get("remediation", []) or []
        ],
        input_tokens=turn.usage.input_tokens,
        output_tokens=turn.usage.output_tokens,
    )
