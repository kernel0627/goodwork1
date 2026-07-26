"""Tests for the signal catalog and the read-only tool layer.

Tests needing a live SUT are skipped when Prometheus is unreachable, so the
suite stays runnable without `make sut-up`.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from medic.signals import SignalCatalog, SignalError, catalog  # noqa: E402
from medic.tools.base import (  # noqa: E402
    MAX_LINE_CHARS,
    Registry,
    ToolError,
    ToolResult,
    cap,
    render_table,
    truncate_line,
)
from medic.tools.prom import Prometheus, register_metric_tools  # noqa: E402
from medic.tools.traces import Jaeger, _span_failed, register_trace_tools  # noqa: E402
from medic.tools.logs import register_log_tools  # noqa: E402


# --------------------------------------------------------------------------
# signal catalog
# --------------------------------------------------------------------------


def test_catalog_loads():
    c = catalog()
    for name in (
        "error_ratio",
        "error_rate",
        "request_rate",
        "latency_p50",
        "latency_p99",
        "cpu",
        "memory",
        "consumer_lag",
        "client_error_ratio",
        "flag_evaluations",
    ):
        assert name in c.names(), f"{name} missing from signals.yaml"


def test_scalar_signals_declare_units_and_thresholds():
    """A threshold-free signal degenerates to `delta > 0`, which noise satisfies."""
    c = catalog()
    for name in c.names():
        sig = c.get(name)
        if sig.multi_series:
            continue
        assert sig.unit, f"{name} has no unit"
        if name != "flag_evaluations":
            assert sig.min_delta > 0, f"{name} has no min_delta"


def test_query_substitutes_and_applies_window_default():
    q = catalog().query("error_ratio", service="checkout")
    assert 'service_name="checkout"' in q
    assert "[2m]" in q
    assert "%" not in q


def test_query_rejects_unsubstituted_placeholder():
    """An unsubstituted %SERVICE% matches nothing and returns zero, which reads
    exactly like a healthy service -- so it must raise rather than run."""
    with pytest.raises(SignalError, match="placeholder|SERVICE"):
        catalog().query("error_ratio")
    with pytest.raises(SignalError):
        catalog().query("client_error_ratio", caller="checkout")  # RPC_PATTERN missing


def test_query_rejects_unknown_signal():
    with pytest.raises(SignalError):
        catalog().query("no_such_signal")


def test_error_signals_cover_every_semconv_family():
    """The SUT mixes four metric families with three status conventions.

    PromQL treats an absent label as empty, so filtering the new RPC family on
    `rpc_grpc_status_code!="0"` matches *every* series -- once measured, that made
    checkout's error rate equal its total request rate. Missing a family instead
    makes a failing service read as permanently healthy. Both failures are silent.
    """
    c = catalog()
    for name in ("error_rate", "error_ratio"):
        promql = c.get(name).promql
        for required in (
            'rpc_grpc_status_code!="0"',  # old RPC semconv: ad
            'rpc_response_status_code!="OK"',  # new RPC semconv: checkout, product-catalog
            'http_status_code=~"5.."',  # old HTTP semconv: frontend
            'http_response_status_code=~"5.."',  # new HTTP semconv: cart
        ):
            assert required in promql, f"{name} does not cover {required}"


def test_load_from_repo_finds_catalog_from_nested_dir():
    c = SignalCatalog.load_from_repo(Path(__file__).resolve().parent)
    assert c.names()


# --------------------------------------------------------------------------
# registry and truncation
# --------------------------------------------------------------------------


class _Dummy:
    name = "dummy"
    description = "test tool"

    def schema(self):
        return {"type": "object", "properties": {"x": {"type": "integer"}}}

    def run(self, **kwargs):
        if kwargs.get("boom"):
            raise RuntimeError("kaboom")
        if kwargs.get("bad"):
            raise ToolError("that argument is not allowed")
        return ToolResult(ok=True, content=f"x={kwargs.get('x')}")


def test_registry_dispatch_and_specs():
    reg = Registry()
    reg.register(_Dummy())
    assert reg.names() == ["dummy"]
    spec = reg.specs()[0]
    assert spec["name"] == "dummy" and "input_schema" in spec
    assert reg.call("dummy", {"x": 3}).content == "x=3"


def test_registry_reports_failures_as_readable_results():
    """Diagnosis is exactly when things fail; a crash would end the episode, while
    a message the model can read is itself an observation."""
    reg = Registry()
    reg.register(_Dummy())

    unknown = reg.call("nope")
    assert not unknown.ok and "no such tool" in unknown.content

    raised = reg.call("dummy", {"boom": True})
    assert not raised.ok and "RuntimeError" in raised.content

    declined = reg.call("dummy", {"bad": True})
    assert not declined.ok and "not allowed" in declined.content


def test_registry_without_supports_ablation():
    reg = Registry()
    reg.register(_Dummy())
    assert "dummy" not in reg.without("dummy").names()
    assert "dummy" in reg.names(), "without() must not mutate the original"


def test_registry_rejects_duplicate_registration():
    reg = Registry()
    reg.register(_Dummy())
    with pytest.raises(ValueError):
        reg.register(_Dummy())


def test_truncation_states_what_it_dropped():
    """An agent that cannot tell it saw a partial answer will over-conclude."""
    long_line = "x" * (MAX_LINE_CHARS + 100)
    assert "chars)" in truncate_line(long_line)
    assert len(truncate_line(long_line)) < len(long_line)

    kept, dropped = cap(list(range(30)), 10)
    assert len(kept) == 10 and dropped == 20
    assert "20 more omitted" in render_table([("a", "1")], dropped=20)


def test_render_table_handles_empty():
    assert render_table([]) == "(no data)"


# --------------------------------------------------------------------------
# span failure detection
# --------------------------------------------------------------------------


def _span(**tags):
    return {"tags": [{"key": k, "value": v} for k, v in tags.items()]}


def test_span_failure_detection_covers_each_convention():
    """Six languages and two semconv generations mark the same failure differently."""
    assert _span_failed(_span(error=True))
    assert _span_failed(_span(error="true"))
    assert _span_failed(_span(**{"otel.status_code": "ERROR"}))
    assert _span_failed(_span(**{"http.status_code": "503"}))
    assert _span_failed(_span(**{"http.response.status_code": "500"}))
    assert _span_failed(_span(**{"rpc.grpc.status_code": 14}))

    assert not _span_failed(_span(**{"http.status_code": "200"}))
    assert not _span_failed(_span(**{"rpc.grpc.status_code": 0}))
    assert not _span_failed(_span())


# --------------------------------------------------------------------------
# live SUT
# --------------------------------------------------------------------------


@pytest.fixture(scope="module")
def live_registry():
    prom = Prometheus()
    try:
        prom.scalar("vector(1)")
    except Exception as exc:  # noqa: BLE001
        pytest.skip(f"Prometheus unreachable ({exc}); run `make sut-up`")
    reg = Registry()
    register_metric_tools(reg, prom)
    register_trace_tools(reg, Jaeger())
    register_log_tools(reg)
    return reg


def test_all_tools_registered(live_registry):
    assert len(live_registry.names()) == 12


def test_every_metric_tool_answers(live_registry):
    for name, args in [
        ("list_services", {}),
        ("get_service_health", {"service": "checkout"}),
        ("get_endpoint_breakdown", {"service": "cart"}),
        ("get_client_calls", {"caller": "checkout"}),
        ("get_resource_usage", {"container": "ad"}),
        ("get_queue_lag", {}),
        ("get_service_topology", {}),
    ]:
        result = live_registry.call(name, args)
        assert result.ok, f"{name} failed: {result.content}"
        assert result.content.strip()


def test_unknown_service_is_distinguished_from_an_uninstrumented_one(live_registry):
    """Both produce all zeros. Conflated, a typo reads as evidence."""
    typo = live_registry.call("get_service_health", {"service": "paymen"})
    assert not typo.ok
    assert "payment" in typo.content, "should suggest the near match"

    real = live_registry.call("get_service_health", {"service": "payment"})
    assert real.ok
    assert "no server-side request metrics" in real.content


def test_topology_excludes_self_edges(live_registry):
    """Self-edges are internal spans, and measured they are the largest edges by
    call count -- left in, they bury the actual dependency graph."""
    result = live_registry.call("get_service_topology", {})
    assert result.ok
    for edge in result.raw["edges"]:
        assert edge["parent"] != edge["child"]


def test_promql_warns_that_no_series_is_not_health(live_registry):
    result = live_registry.call("promql", {"query": "medic_definitely_absent_metric"})
    assert result.ok
    assert "not necessarily good news" in result.content


def test_promql_surfaces_syntax_errors(live_registry):
    result = live_registry.call("promql", {"query": "sum(((("})
    assert not result.ok


def test_query_logs_reads_flagd_reload_events(live_registry):
    """Also the evidence tier-1 fault verification depends on."""
    result = live_registry.call(
        "query_logs", {"service": "flagd", "pattern": "filepath event", "since": "2h"}
    )
    assert result.ok


def test_query_logs_rejects_a_bad_regex(live_registry):
    result = live_registry.call("query_logs", {"service": "flagd", "pattern": "([unclosed"})
    assert not result.ok
    assert "regex" in result.content.lower()
