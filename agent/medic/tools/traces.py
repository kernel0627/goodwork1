"""Trace tools: the dependency graph and individual request paths.

The topology comes from Jaeger's measured call data, never from a hand-written
map. That is a deliberate constraint. A hardcoded graph would be the answer to
half of every attribution question -- "the symptom is on checkout, and checkout
calls payment" is most of the reasoning -- so shipping one would inflate the
agent's apparent competence, and the ablation that withholds this tool would no
longer measure anything.
"""

from __future__ import annotations

import json
import os
import time
from collections import defaultdict
from typing import Any

import httpx

from .base import (
    MAX_SERIES,
    MAX_TRACES,
    ToolError,
    ToolResult,
    cap,
    render_table,
    require,
    truncate_line,
)

# Jaeger is not published on the host; it sits behind Envoy under this base path.
# Measured: /jaeger/api/... returns 404, /jaeger/ui/api/... returns 200.
DEFAULT_JAEGER_URL = os.getenv("JAEGER_URL", "http://localhost:8080/jaeger/ui")


class Jaeger:
    def __init__(self, base_url: str = DEFAULT_JAEGER_URL, timeout: float = 20.0):
        self.base_url = base_url.rstrip("/")
        self._client = httpx.Client(timeout=timeout)

    def _get(self, path: str, params: Any = None) -> Any:
        resp = self._client.get(f"{self.base_url}{path}", params=params)
        resp.raise_for_status()
        body = resp.json()
        if body.get("errors"):
            raise ToolError(f"jaeger: {body['errors']}")
        return body.get("data")

    def services(self) -> list[str]:
        return sorted(self._get("/api/services") or [])

    def dependencies(self, lookback_hours: int = 1) -> list[dict[str, Any]]:
        end_ms = int(time.time() * 1000)
        return (
            self._get(
                "/api/dependencies",
                {"endTs": end_ms, "lookback": lookback_hours * 3600 * 1000},
            )
            or []
        )

    def traces(
        self,
        service: str,
        limit: int = 10,
        lookback: str = "1h",
        tags: dict[str, str] | None = None,
        min_duration: str | None = None,
    ) -> list[dict[str, Any]]:
        params: dict[str, Any] = {
            "service": service,
            "limit": limit,
            "lookback": lookback,
        }
        if tags:
            params["tags"] = json.dumps(tags)
        if min_duration:
            params["minDuration"] = min_duration
        return self._get("/api/traces", params) or []

    def trace(self, trace_id: str) -> list[dict[str, Any]]:
        return self._get(f"/api/traces/{trace_id}") or []

    def close(self) -> None:
        self._client.close()


def _service_of(trace: dict[str, Any], span: dict[str, Any]) -> str:
    process = (trace.get("processes") or {}).get(span.get("processID"), {})
    return process.get("serviceName", "?")


def _tag(span: dict[str, Any], key: str) -> Any:
    for tag in span.get("tags") or []:
        if tag.get("key") == key:
            return tag.get("value")
    return None


def _span_failed(span: dict[str, Any]) -> bool:
    """Whether a span looks like a failure.

    Several conventions are checked because the SUT is polyglot: six languages
    and two generations of semantic convention produce different error markers on
    otherwise identical failures.
    """
    if _tag(span, "error") in (True, "true"):
        return True
    if _tag(span, "otel.status_code") == "ERROR":
        return True
    status = _tag(span, "http.status_code") or _tag(span, "http.response.status_code")
    if status and str(status).startswith("5"):
        return True
    grpc = _tag(span, "rpc.grpc.status_code")
    if grpc not in (None, 0, "0"):
        return True
    return False


class TopologyTool:
    name = "get_service_topology"
    description = (
        "The service dependency graph, measured from actual traces over the last "
        "hour, with call counts per edge. Use it to work out which services could "
        "be responsible for a symptom seen somewhere else."
    )

    def __init__(self, jaeger: Jaeger):
        self.jaeger = jaeger

    def schema(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "service": {
                    "type": "string",
                    "description": "Optional: show only edges touching this service.",
                },
                "lookback_hours": {
                    "type": "integer",
                    "description": "How far back to aggregate. Default 1.",
                },
            },
        }

    def run(self, **kwargs: Any) -> ToolResult:
        service = kwargs.get("service")
        hours = int(kwargs.get("lookback_hours") or 1)
        edges = self.jaeger.dependencies(hours)

        # Self-edges are internal spans within one service, not dependencies.
        # Keeping them would bury the real graph: measured, they are the six
        # largest edges by call count.
        useful = [
            e
            for e in edges
            if e.get("parent") != e.get("child") and e.get("callCount", 0) > 0
        ]
        if service:
            useful = [
                e for e in useful if service in (e.get("parent"), e.get("child"))
            ]

        useful.sort(key=lambda e: -e.get("callCount", 0))
        shown, dropped = cap(useful, MAX_SERIES)
        rows = [
            (f"{e['parent']} -> {e['child']}", f"{e['callCount']} calls")
            for e in shown
        ]
        scope = f" touching {service}" if service else ""
        return ToolResult(
            ok=True,
            content=f"{len(useful)} dependency edges{scope} (last {hours}h, "
            f"self-edges excluded)\n" + render_table(rows, dropped),
            raw={"edges": useful},
            truncated=dropped > 0,
        )


class ErrorTracesTool:
    name = "find_error_traces"
    description = (
        "Recent traces containing a failed span, summarised by which service "
        "failed and on which operation. This is usually the fastest way to move "
        "from 'something is failing' to 'this service is failing on this call'."
    )

    def __init__(self, jaeger: Jaeger):
        self.jaeger = jaeger

    def schema(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "service": {
                    "type": "string",
                    "description": "Entry-point service to search from, e.g. 'frontend'.",
                },
                "lookback": {
                    "type": "string",
                    "description": "Time range such as '15m' or '1h'. Default 15m.",
                },
                "limit": {"type": "integer", "description": "Max traces. Default 10."},
            },
            "required": ["service"],
        }

    def run(self, **kwargs: Any) -> ToolResult:
        service = require(kwargs, "service")
        lookback = kwargs.get("lookback") or "15m"
        limit = min(int(kwargs.get("limit") or 10), MAX_TRACES)

        traces = self.jaeger.traces(
            service, limit=limit, lookback=lookback, tags={"error": "true"}
        )
        if not traces:
            return ToolResult(
                ok=True,
                content=(
                    f"No traces tagged with an error for {service} in the last "
                    f"{lookback}.\nThis SUT does not mark every failure with an "
                    f"error tag, so absence here is weak evidence. Cross-check "
                    f"with get_service_health."
                ),
                raw={"service": service, "traces": []},
            )

        # Aggregating by (service, operation) rather than listing traces: the
        # useful answer is which call fails, and 100-span traces would flood the
        # context otherwise.
        failures: dict[tuple[str, str], int] = defaultdict(int)
        examples: dict[tuple[str, str], str] = {}
        for trace in traces:
            for span in trace.get("spans") or []:
                if not _span_failed(span):
                    continue
                key = (
                    _service_of(trace, span),
                    span.get("operationName", "?"),
                )
                failures[key] += 1
                examples.setdefault(key, trace["traceID"])

        if not failures:
            return ToolResult(
                ok=True,
                content=f"{len(traces)} traces matched but no span in them looks failed.",
                raw={"service": service, "traces": [t["traceID"] for t in traces]},
            )

        ordered = sorted(failures.items(), key=lambda kv: -kv[1])
        shown, dropped = cap(ordered, MAX_SERIES)
        rows = [
            (
                truncate_line(f"{svc}  {op}", 58),
                f"{count} failed spans   e.g. trace {examples[(svc, op)][:16]}",
            )
            for (svc, op), count in shown
        ]
        return ToolResult(
            ok=True,
            content=f"failed spans across {len(traces)} traces from {service} "
            f"(last {lookback})\n" + render_table(rows, dropped),
            raw={
                "service": service,
                "failures": [
                    {"service": s, "operation": o, "count": c, "example": examples[(s, o)]}
                    for (s, o), c in ordered
                ],
            },
            truncated=dropped > 0,
        )


class TraceDetailTool:
    name = "get_trace"
    description = (
        "The span tree of one trace: which service called which, how long each "
        "took, and which failed. Use after find_error_traces to see the exact "
        "call path of a failure."
    )

    def __init__(self, jaeger: Jaeger):
        self.jaeger = jaeger

    def schema(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "trace_id": {"type": "string", "description": "Trace ID."},
                "max_spans": {
                    "type": "integer",
                    "description": "Cap on spans shown. Default 40.",
                },
            },
            "required": ["trace_id"],
        }

    def run(self, **kwargs: Any) -> ToolResult:
        trace_id = require(kwargs, "trace_id")
        max_spans = min(int(kwargs.get("max_spans") or 40), 80)

        traces = self.jaeger.trace(trace_id)
        if not traces:
            return ToolResult.failure(f"trace {trace_id} not found")
        trace = traces[0]
        spans = trace.get("spans") or []

        # Slowest first, and failed spans always kept. A 100-span trace truncated
        # by arrival order would very likely drop the failure being investigated.
        failed = [s for s in spans if _span_failed(s)]
        rest = sorted(
            (s for s in spans if not _span_failed(s)),
            key=lambda s: -s.get("duration", 0),
        )
        selected = failed + rest
        shown, dropped = cap(selected, max_spans)

        rows = []
        for span in shown:
            marker = "FAIL " if _span_failed(span) else "     "
            ms = span.get("duration", 0) / 1000.0
            rows.append(
                (
                    truncate_line(
                        f"{marker}{_service_of(trace, span)}  {span.get('operationName','?')}",
                        58,
                    ),
                    f"{ms:.1f} ms",
                )
            )
        header = (
            f"trace {trace_id[:16]}  {len(spans)} spans, {len(failed)} failed\n"
            f"(failed spans first, then slowest)\n"
        )
        return ToolResult(
            ok=True,
            content=header + render_table(rows, dropped),
            raw={
                "trace_id": trace_id,
                "span_count": len(spans),
                "failed_count": len(failed),
            },
            truncated=dropped > 0,
        )


def register_trace_tools(registry, jaeger: Jaeger):
    for tool in (TopologyTool(jaeger), ErrorTracesTool(jaeger), TraceDetailTool(jaeger)):
        registry.register(tool)
    return registry


__all__ = [
    "Jaeger",
    "register_trace_tools",
    "TopologyTool",
    "ErrorTracesTool",
    "TraceDetailTool",
]
