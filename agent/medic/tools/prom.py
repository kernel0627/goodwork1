"""Metric tools: what the agent can ask Prometheus.

All queries route through ``queries/signals.yaml``. That is not a style choice --
measuring an error rate on this SUT means covering four metric families with
three different status-label conventions, and getting it wrong returns a
confident zero rather than an error. If the agent wrote its own PromQL for this,
a failing service would read as healthy and the agent would be blamed for the
resulting misdiagnosis.

A raw ``promql`` escape hatch exists anyway, because a diagnosis the curated
signals cannot express should be possible rather than impossible. Which one the
agent reaches for is itself worth measuring.
"""

from __future__ import annotations

import math
import os
from typing import Any

import httpx

from ..signals import SignalCatalog, SignalError, catalog
from .base import (
    MAX_SERIES,
    ToolError,
    ToolResult,
    cap,
    render_table,
    require,
    truncate_line,
)

DEFAULT_PROM_URL = os.getenv("PROM_URL", "http://localhost:9090")


class Prometheus:
    """Minimal Prometheus HTTP API client."""

    def __init__(self, base_url: str = DEFAULT_PROM_URL, timeout: float = 15.0):
        self.base_url = base_url.rstrip("/")
        self._client = httpx.Client(timeout=timeout)

    def instant(self, query: str) -> list[tuple[dict[str, str], float]]:
        resp = self._client.get(
            f"{self.base_url}/api/v1/query", params={"query": query}
        )
        resp.raise_for_status()
        body = resp.json()
        if body.get("status") != "success":
            raise ToolError(
                f"prometheus rejected the query: {body.get('error', 'unknown error')}"
            )
        out = []
        for series in body["data"]["result"]:
            try:
                out.append((series["metric"], float(series["value"][1])))
            except (KeyError, IndexError, ValueError):
                continue
        return out

    def scalar(self, query: str) -> float:
        results = self.instant(query)
        if not results:
            return math.nan
        return results[0][1]

    def label_values(self, label: str, *matches: str) -> list[str]:
        params: list[tuple[str, str]] = [("match[]", m) for m in matches]
        resp = self._client.get(
            f"{self.base_url}/api/v1/label/{label}/values", params=params
        )
        resp.raise_for_status()
        body = resp.json()
        if body.get("status") != "success":
            raise ToolError(f"prometheus: {body.get('error')}")
        return sorted(body.get("data", []))

    def close(self) -> None:
        self._client.close()


def _fmt(value: float, unit: str) -> str:
    if math.isnan(value):
        return "no data"
    if unit == "ratio":
        return f"{value:.4f} ({value * 100:.2f}%)"
    if unit == "percent":
        return f"{value:.1f}%"
    if unit == "milliseconds":
        return f"{value:.0f} ms"
    if unit == "cores":
        return f"{value:.2f} cores"
    if unit.endswith("/second"):
        return f"{value:.4g}/s"
    return f"{value:.4g}"


class ServiceHealthTool:
    """Bundles the metrics a dashboard would show for one service.

    Bundled rather than one metric per call because these four are always read
    together: an error ratio without a request rate cannot distinguish "failing"
    from "idle", and a p99 without a p50 cannot distinguish "everything is slow"
    from "the tail is slow". Four separate calls would spend four turns
    assembling one observation.
    """

    name = "get_service_health"
    description = (
        "Current health of one service: error ratio, request rate, and p50/p99 "
        "latency. Start here. An error ratio of 0 with a request rate of 0 means "
        "the service is idle, not healthy."
    )

    def __init__(self, prom: Prometheus, signals: SignalCatalog | None = None):
        self.prom = prom
        self.signals = signals or catalog()

    def schema(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "service": {
                    "type": "string",
                    "description": "Service name, e.g. 'checkout' or 'product-catalog'.",
                },
                "window": {
                    "type": "string",
                    "description": "Rate window such as '2m' or '5m'. Defaults to 2m.",
                },
            },
            "required": ["service"],
        }

    def run(self, **kwargs: Any) -> ToolResult:
        service = require(kwargs, "service")
        window = kwargs.get("window") or "2m"

        # A misspelled service name produces exactly the same all-zero reading as
        # a real service with no server-side instrumentation. Left
        # indistinguishable, a typo reads as evidence and the agent moves on
        # having learned something false. So the name is checked first.
        known = self.prom.label_values("service_name")
        if service not in known:
            close = [s for s in known if service in s or s in service]
            hint = f" Did you mean: {', '.join(close)}?" if close else ""
            return ToolResult.failure(
                f"no service named {service!r} reports metrics.{hint} "
                f"Use list_services to see all {len(known)}.",
                known_services=known,
            )

        rows: list[tuple[str, str]] = []
        raw: dict[str, Any] = {"service": service, "window": window}
        for signal_name in ("error_ratio", "request_rate", "latency_p50", "latency_p99"):
            signal = self.signals.get(signal_name)
            query = self.signals.query(signal_name, service=service, window=window)
            value = self.prom.scalar(query)
            raw[signal_name] = None if math.isnan(value) else value
            rows.append((signal_name, _fmt(value, signal.unit)))

        note = ""
        if raw.get("request_rate") in (None, 0):
            note = (
                f"\nNOTE: {service} exists but reports no server-side request "
                "metrics, so these zeros say nothing about its health. Several "
                "services here are only observable from their callers: use "
                "get_client_calls on a caller, or get_resource_usage."
            )
        return ToolResult(
            ok=True,
            content=f"{service} (window {window})\n" + render_table(rows) + note,
            raw=raw,
        )


class EndpointBreakdownTool:
    """Per-endpoint request and error rates for one service.

    Worth its own tool because a service-wide aggregate hides a fault scoped to
    one endpoint. Measured on this SUT: cart serves GetCart at 0.67/s, AddItem at
    0.22/s and EmptyCart at 0.03/s, and one of its faults affects EmptyCart
    alone. In the aggregate that fault is a rounding error; per endpoint it is a
    total outage.
    """

    name = "get_endpoint_breakdown"
    description = (
        "Request and error rate per endpoint for one service. Use when a service "
        "looks mostly healthy in aggregate: a fault confined to one low-traffic "
        "endpoint barely moves the service-wide numbers."
    )

    def __init__(self, prom: Prometheus, signals: SignalCatalog | None = None):
        self.prom = prom
        self.signals = signals or catalog()

    def schema(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "service": {"type": "string", "description": "Service name."},
                "window": {"type": "string", "description": "Rate window, default 2m."},
            },
            "required": ["service"],
        }

    def run(self, **kwargs: Any) -> ToolResult:
        service = require(kwargs, "service")
        window = kwargs.get("window") or "2m"

        def by_endpoint(signal_name: str) -> dict[str, float]:
            query = self.signals.query(signal_name, service=service, window=window)
            out: dict[str, float] = {}
            for labels, value in self.prom.instant(query):
                key = labels.get("rpc_method") or labels.get("http_route") or "(unlabelled)"
                out[key] = out.get(key, 0.0) + value
            return out

        totals = by_endpoint("request_rate_by_endpoint")
        errors = by_endpoint("error_rate_by_endpoint")

        if not totals:
            return ToolResult(
                ok=True,
                content=f"{service}: no per-endpoint server metrics found.",
                raw={"service": service, "endpoints": {}},
            )

        ordered = sorted(totals.items(), key=lambda kv: -kv[1])
        shown, dropped = cap(ordered, MAX_SERIES)
        rows = []
        for endpoint, total in shown:
            failed = errors.get(endpoint, 0.0)
            ratio = failed / total if total else 0.0
            rows.append(
                (
                    truncate_line(endpoint, 52),
                    f"{total:.4g}/s   errors {failed:.4g}/s ({ratio * 100:.1f}%)",
                )
            )
        return ToolResult(
            ok=True,
            content=f"{service} endpoints (window {window})\n"
            + render_table(rows, dropped),
            raw={"service": service, "request_rate": totals, "error_rate": errors},
            truncated=dropped > 0,
        )


class ClientCallsTool:
    """A caller's outbound calls, optionally scoped to one target.

    The only way to see three of this SUT's services: payment, recommendation and
    email report no server-side request metrics at all -- measured at 0.000 req/s
    each -- so however completely they fail, their own instrumentation shows
    nothing. Their callers see it.

    Scoped by target method rather than by address: the client metrics carry
    ``server_address="172.18.0.15"``, a container IP that changes on restart,
    while ``rpc_method`` carries the stable ``oteldemo.PaymentService/Charge``.
    """

    name = "get_client_calls"
    description = (
        "What one service sees when calling its dependencies: per-target call "
        "rate, plus the error ratio for a specific target. Use this when a "
        "service reports no server-side metrics of its own, or to confirm a "
        "dependency is failing from the caller's point of view."
    )

    def __init__(self, prom: Prometheus, signals: SignalCatalog | None = None):
        self.prom = prom
        self.signals = signals or catalog()

    def schema(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "caller": {
                    "type": "string",
                    "description": "Service making the calls, e.g. 'checkout'.",
                },
                "target_pattern": {
                    "type": "string",
                    "description": (
                        "Optional regex over the target method to get an error "
                        "ratio for, e.g. 'oteldemo.PaymentService/.*'."
                    ),
                },
                "window": {"type": "string", "description": "Rate window, default 2m."},
            },
            "required": ["caller"],
        }

    def run(self, **kwargs: Any) -> ToolResult:
        caller = require(kwargs, "caller")
        window = kwargs.get("window") or "2m"
        pattern = kwargs.get("target_pattern")

        query = self.signals.query(
            "client_call_rate_by_peer", caller=caller, window=window
        )
        calls: dict[str, float] = {}
        for labels, value in self.prom.instant(query):
            calls[labels.get("rpc_method", "(unlabelled)")] = value

        raw: dict[str, Any] = {"caller": caller, "calls": calls}
        rows = [
            (truncate_line(method, 52), f"{rate:.4g}/s")
            for method, rate in sorted(calls.items(), key=lambda kv: -kv[1])
        ]
        shown, dropped = cap(rows, MAX_SERIES)
        content = f"{caller} outbound calls (window {window})\n" + render_table(
            shown, dropped
        )

        if pattern:
            ratio = self.prom.scalar(
                self.signals.query(
                    "client_error_ratio",
                    caller=caller,
                    rpc_pattern=pattern,
                    window=window,
                )
            )
            raw["target_pattern"] = pattern
            raw["client_error_ratio"] = None if math.isnan(ratio) else ratio
            content += f"\n\nerror ratio for {pattern}: {_fmt(ratio, 'ratio')}"

        return ToolResult(ok=True, content=content, raw=raw, truncated=dropped > 0)


class ResourceUsageTool:
    name = "get_resource_usage"
    description = (
        "CPU and memory for one container. Memory is a percentage of the "
        "container's configured limit, so values near 100 mean it is about to be "
        "OOM-killed. CPU is in cores and can exceed 1."
    )

    def __init__(self, prom: Prometheus, signals: SignalCatalog | None = None):
        self.prom = prom
        self.signals = signals or catalog()

    def schema(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "container": {
                    "type": "string",
                    "description": "Container name, which matches the service name in this system.",
                }
            },
            "required": ["container"],
        }

    def run(self, **kwargs: Any) -> ToolResult:
        container = require(kwargs, "container")
        rows = []
        raw: dict[str, Any] = {"container": container}
        for signal_name in ("cpu", "memory"):
            signal = self.signals.get(signal_name)
            value = self.prom.scalar(
                self.signals.query(signal_name, container=container)
            )
            raw[signal_name] = None if math.isnan(value) else value
            rows.append((signal_name, _fmt(value, signal.unit)))
        return ToolResult(
            ok=True, content=f"{container}\n" + render_table(rows), raw=raw
        )


class QueueLagTool:
    name = "get_queue_lag"
    description = (
        "Kafka consumer lag in records, total and per consuming service. Lag is "
        "how a problem on the asynchronous side of the system shows up: no "
        "request fails, work simply falls behind."
    )

    def __init__(self, prom: Prometheus, signals: SignalCatalog | None = None):
        self.prom = prom
        self.signals = signals or catalog()

    def schema(self) -> dict[str, Any]:
        return {"type": "object", "properties": {}}

    def run(self, **kwargs: Any) -> ToolResult:
        total = self.prom.scalar(self.signals.query("consumer_lag"))
        rows = [("total", _fmt(total, "records"))]
        raw: dict[str, Any] = {"total": None if math.isnan(total) else total}

        for service in ("accounting", "fraud-detection"):
            value = self.prom.scalar(
                self.signals.query("consumer_lag_by_service", service=service)
            )
            raw[service] = None if math.isnan(value) else value
            rows.append((service, _fmt(value, "records")))
        return ToolResult(ok=True, content=render_table(rows), raw=raw)


class ListServicesTool:
    name = "list_services"
    description = (
        "Every service reporting metrics. Use to learn what exists before "
        "guessing names."
    )

    def __init__(self, prom: Prometheus):
        self.prom = prom

    def schema(self) -> dict[str, Any]:
        return {"type": "object", "properties": {}}

    def run(self, **kwargs: Any) -> ToolResult:
        services = self.prom.label_values("service_name")
        return ToolResult(
            ok=True,
            content=f"{len(services)} services reporting metrics:\n"
            + "\n".join(f"  {s}" for s in services),
            raw={"services": services},
        )


class PromQLTool:
    """Raw PromQL, for questions the curated signals cannot express.

    Kept despite the risk, because a diagnosis the tool author did not anticipate
    should be awkward rather than impossible. The risk is real: PromQL fails
    quietly, and a mistyped label yields zero instead of an error. The
    description says so, and how often the agent falls back here is a result
    worth reporting.
    """

    name = "promql"
    description = (
        "Run an arbitrary PromQL instant query. Prefer the purpose-built tools: "
        "this system mixes four metric families with three different status-label "
        "conventions, and a query that gets that wrong returns 0 rather than an "
        "error -- a failing service will look healthy. Use this only for "
        "questions the other tools cannot express."
    )

    def __init__(self, prom: Prometheus):
        self.prom = prom

    def schema(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "query": {"type": "string", "description": "A PromQL instant query."}
            },
            "required": ["query"],
        }

    def run(self, **kwargs: Any) -> ToolResult:
        query = require(kwargs, "query")
        results = self.prom.instant(query)
        if not results:
            return ToolResult(
                ok=True,
                content=(
                    "0 series matched.\n"
                    "This is not necessarily good news: a mistyped label or metric "
                    "name also matches nothing. Verify the metric exists before "
                    "reading this as healthy."
                ),
                raw={"query": query, "series": []},
            )

        rows = []
        for labels, value in results:
            label_text = ",".join(
                f"{k}={v}" for k, v in sorted(labels.items()) if k != "__name__"
            )
            rows.append((truncate_line(label_text or "(no labels)", 60), f"{value:.6g}"))
        shown, dropped = cap(rows, MAX_SERIES)
        return ToolResult(
            ok=True,
            content=f"{len(results)} series\n" + render_table(shown, dropped),
            raw={
                "query": query,
                "series": [{"labels": l, "value": v} for l, v in results[:MAX_SERIES]],
            },
            truncated=dropped > 0,
        )


def register_metric_tools(registry, prom: Prometheus, signals: SignalCatalog | None = None):
    """Register every metric tool onto a registry."""
    sig = signals or catalog()
    for tool in (
        ListServicesTool(prom),
        ServiceHealthTool(prom, sig),
        EndpointBreakdownTool(prom, sig),
        ClientCallsTool(prom, sig),
        ResourceUsageTool(prom, sig),
        QueueLagTool(prom, sig),
        PromQLTool(prom),
    ):
        registry.register(tool)
    return registry


__all__ = [
    "Prometheus",
    "SignalError",
    "register_metric_tools",
    "ListServicesTool",
    "ServiceHealthTool",
    "EndpointBreakdownTool",
    "ClientCallsTool",
    "ResourceUsageTool",
    "QueueLagTool",
    "PromQLTool",
]
