"""Anthropic client wrapper.

Two things live here that every model-driven arm needs identically, so that a
difference between arms comes from their procedure rather than from their
plumbing: token accounting, and a forced-schema answer.

The answer is obtained by requiring a tool call rather than by asking for JSON in
prose. Parsing JSON out of free text fails occasionally, and those failures are
not random -- they cluster on the hard episodes where the model hedges and writes
around the answer. Losing exactly those would flatter every arm.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import Any

import anthropic

DEFAULT_MODEL = os.getenv("MEDIC_MODEL", "claude-sonnet-5")
CHEAP_MODEL = os.getenv("MEDIC_MODEL_CHEAP", "claude-haiku-4-5-20251001")


class LLMError(Exception):
    pass


@dataclass
class Usage:
    input_tokens: int = 0
    output_tokens: int = 0

    def add(self, other: "Usage") -> None:
        self.input_tokens += other.input_tokens
        self.output_tokens += other.output_tokens

    @property
    def total(self) -> int:
        return self.input_tokens + self.output_tokens


@dataclass
class Turn:
    """One model exchange, including any tool calls it requested."""

    text: str = ""
    tool_calls: list[dict[str, Any]] = field(default_factory=list)
    stop_reason: str = ""
    usage: Usage = field(default_factory=Usage)
    raw_content: list[Any] = field(default_factory=list)


class LLM:
    def __init__(self, model: str | None = None, max_tokens: int = 4096):
        api_key = os.getenv("ANTHROPIC_API_KEY")
        if not api_key:
            raise LLMError(
                "ANTHROPIC_API_KEY is not set. The random brain needs no key; "
                "every model-driven arm does."
            )
        self.model = model or DEFAULT_MODEL
        self.max_tokens = max_tokens
        self.client = anthropic.Anthropic(api_key=api_key)
        self.usage = Usage()

    def complete(
        self,
        system: str,
        messages: list[dict[str, Any]],
        tools: list[dict[str, Any]] | None = None,
        force_tool: str | None = None,
        max_tokens: int | None = None,
    ) -> Turn:
        kwargs: dict[str, Any] = {
            "model": self.model,
            "max_tokens": max_tokens or self.max_tokens,
            "system": system,
            "messages": messages,
        }
        if tools:
            kwargs["tools"] = tools
        if force_tool:
            kwargs["tool_choice"] = {"type": "tool", "name": force_tool}

        try:
            response = self.client.messages.create(**kwargs)
        except anthropic.APIError as exc:
            raise LLMError(f"{type(exc).__name__}: {exc}") from exc

        turn = Turn(
            stop_reason=response.stop_reason or "",
            usage=Usage(
                input_tokens=response.usage.input_tokens,
                output_tokens=response.usage.output_tokens,
            ),
            raw_content=list(response.content),
        )
        for block in response.content:
            if block.type == "text":
                turn.text += block.text
            elif block.type == "tool_use":
                turn.tool_calls.append(
                    {"id": block.id, "name": block.name, "input": block.input}
                )

        self.usage.add(turn.usage)
        return turn


# The schema every arm answers through. Shared so the answer format cannot become
# a confound: an arm with a more helpful schema would score better for reasons
# that have nothing to do with its reasoning.
DIAGNOSIS_TOOL: dict[str, Any] = {
    "name": "report_diagnosis",
    "description": "Report your conclusion about the alert.",
    "input_schema": {
        "type": "object",
        "properties": {
            "root_cause_service": {
                "type": "string",
                "description": (
                    "The service actually at fault. This is often NOT the service "
                    "the alert fired on -- a failing dependency shows up as errors "
                    "in its caller. Leave empty only if reporting healthy."
                ),
            },
            "root_cause_class": {
                "type": "string",
                "enum": ["error", "latency", "resource", "connectivity", "health", "queue"],
                "description": (
                    "error: requests failing. latency: requests slow. "
                    "resource: CPU or memory exhaustion. connectivity: a dependency "
                    "unreachable. health: a health or readiness check failing. "
                    "queue: asynchronous consumer lag."
                ),
            },
            "healthy": {
                "type": "boolean",
                "description": (
                    "True only if you checked and the system is genuinely fine and "
                    "the alert was spurious. This is a real possible answer, not a "
                    "way of declining to answer."
                ),
            },
            "escalate": {
                "type": "boolean",
                "description": "True if you cannot determine the cause and are handing off.",
            },
            "confidence": {
                "type": "number",
                "description": "0 to 1.",
            },
            "reasoning": {
                "type": "string",
                "description": (
                    "How the evidence led to this conclusion. Cite the specific "
                    "numbers you relied on."
                ),
            },
            "remediation": {
                "type": "array",
                "description": "Actions you would propose. Not executed.",
                "items": {
                    "type": "object",
                    "properties": {
                        "action": {"type": "string"},
                        "target": {"type": "string"},
                        "rationale": {"type": "string"},
                    },
                    "required": ["action", "target"],
                },
            },
        },
        "required": ["root_cause_class", "healthy", "escalate", "confidence", "reasoning"],
    },
}


# Shared by every model-driven arm. It describes the system and the job, and
# deliberately says nothing about which service is at fault or how many faults
# exist -- both would be leaks.
SYSTEM_PROMPT = """\
You are an on-call engineer for a microservice system. You have been paged.

Your job is to identify the ROOT CAUSE of the alert: which service is actually at
fault, and what kind of fault it is.

Facts about this system that are easy to get wrong:

- The alert fires on the service where the SYMPTOM appears. That is frequently
  not the service at fault. A failing dependency surfaces as errors or latency in
  whatever calls it, so a symptom on `checkout` may well be a fault in something
  checkout calls.

- Several services report NO server-side request metrics at all. For those,
  get_service_health returns all zeros, and those zeros mean nothing -- they are
  not evidence of health. Reach those services through a caller's client metrics
  instead.

- A metric query matching no series returns zero rather than an error. Zero can
  mean healthy, idle, or wrongly queried, and telling those apart is on you.

- An alert can be spurious. If the evidence says the system is fine, say so.
  Reporting healthy when it is healthy is a correct answer; inventing a plausible
  cause is not.

Reason from the evidence you gathered. Cite the specific numbers behind your
conclusion. When the evidence is genuinely insufficient, escalate rather than
guessing -- but escalating when the evidence was there is also a failure.
"""
