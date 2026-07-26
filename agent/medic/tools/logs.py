"""Log tools, backed by container logs.

Reads `docker logs` rather than a log backend. The demo does ship OpenSearch and
the collector does export to it, but container logs are used anyway for three
reasons: it is what an oncall actually reaches for; it survives the collector's
log pipeline being broken, which is precisely when logs matter most; and it keeps
OpenSearch -- the single largest memory consumer in the SUT at 1 GB -- out of the
agent's dependency chain.
"""

from __future__ import annotations

import re
import shutil
import subprocess
from typing import Any

from .base import (
    MAX_LOG_LINES,
    ToolError,
    ToolResult,
    cap,
    require,
    truncate_line,
)

# Matched by substring rather than parsed. Six languages in this SUT means six
# log formats, and a parser for all of them would be a project of its own.
ERROR_TOKENS = (
    "error",
    "exception",
    "fail",
    "panic",
    "fatal",
    "unavailable",
    "timeout",
    "refused",
    "unhealthy",
)


def _docker_logs(container: str, since: str, tail: int) -> str:
    if shutil.which("docker") is None:
        raise ToolError("docker CLI not found on PATH")
    try:
        proc = subprocess.run(
            ["docker", "logs", container, "--since", since, "--tail", str(tail)],
            capture_output=True,
            text=True,
            timeout=30,
        )
    except subprocess.TimeoutExpired as exc:
        raise ToolError(f"docker logs {container} timed out") from exc
    if proc.returncode != 0:
        raise ToolError(
            f"docker logs {container} failed: {proc.stderr.strip() or 'unknown error'}"
        )
    # Containers here write to both streams, and which one gets used varies by
    # language runtime, so both are read.
    return proc.stdout + proc.stderr


class QueryLogsTool:
    name = "query_logs"
    description = (
        "Recent log lines from one service's container, optionally filtered by a "
        "regex. Logs carry the specific reason behind a metric: the metric says "
        "requests are failing, the log says which dependency refused the "
        "connection."
    )

    def schema(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "service": {
                    "type": "string",
                    "description": "Service name; matches the container name here.",
                },
                "pattern": {
                    "type": "string",
                    "description": "Optional case-insensitive regex to filter lines.",
                },
                "since": {
                    "type": "string",
                    "description": "How far back, e.g. '5m' or '1h'. Default 10m.",
                },
                "limit": {
                    "type": "integer",
                    "description": f"Max lines returned, capped at {MAX_LOG_LINES}.",
                },
            },
            "required": ["service"],
        }

    def run(self, **kwargs: Any) -> ToolResult:
        service = require(kwargs, "service")
        since = kwargs.get("since") or "10m"
        limit = min(int(kwargs.get("limit") or 30), MAX_LOG_LINES)
        pattern = kwargs.get("pattern")

        raw_output = _docker_logs(service, since, tail=2000)
        lines = [l for l in (x.strip() for x in raw_output.splitlines()) if l]

        if pattern:
            try:
                regex = re.compile(pattern, re.IGNORECASE)
            except re.error as exc:
                raise ToolError(f"bad regex {pattern!r}: {exc}") from exc
            lines = [l for l in lines if regex.search(l)]

        # Newest last is how logs read, but the newest are the interesting ones,
        # so the tail is kept rather than the head.
        kept = lines[-limit:] if len(lines) > limit else lines
        dropped = len(lines) - len(kept)

        if not kept:
            scope = f" matching {pattern!r}" if pattern else ""
            return ToolResult(
                ok=True,
                content=f"No log lines from {service}{scope} in the last {since}.",
                raw={"service": service, "lines": []},
            )

        body = "\n".join(truncate_line(l) for l in kept)
        header = f"{service} logs, last {since}"
        if pattern:
            header += f", matching {pattern!r}"
        if dropped:
            header += f" ({dropped} older lines omitted)"
        return ToolResult(
            ok=True,
            content=f"{header}\n{body}",
            raw={"service": service, "lines": kept},
            truncated=dropped > 0,
        )


class ErrorLogsTool:
    name = "find_error_logs"
    description = (
        "Log lines from one service that look like failures, matched on level "
        "tokens. Faster than composing a regex when the shape of the failure is "
        "not yet known."
    )

    def schema(self) -> dict[str, Any]:
        return {
            "type": "object",
            "properties": {
                "service": {"type": "string", "description": "Service name."},
                "since": {"type": "string", "description": "How far back. Default 10m."},
                "limit": {"type": "integer", "description": "Max lines. Default 25."},
            },
            "required": ["service"],
        }

    def run(self, **kwargs: Any) -> ToolResult:
        service = require(kwargs, "service")
        since = kwargs.get("since") or "10m"
        limit = min(int(kwargs.get("limit") or 25), MAX_LOG_LINES)

        raw_output = _docker_logs(service, since, tail=2000)
        hits = [
            line
            for line in (x.strip() for x in raw_output.splitlines())
            if line and any(token in line.lower() for token in ERROR_TOKENS)
        ]
        kept, dropped = cap(hits[::-1], limit)  # newest first

        if not kept:
            return ToolResult(
                ok=True,
                content=(
                    f"No error-looking lines from {service} in the last {since}.\n"
                    f"Matching is by keyword, so a failure logged without one of "
                    f"{', '.join(ERROR_TOKENS[:4])}… would be missed. Try "
                    f"query_logs without a filter."
                ),
                raw={"service": service, "lines": []},
            )
        body = "\n".join(truncate_line(l) for l in kept)
        return ToolResult(
            ok=True,
            content=f"{len(hits)} error-looking lines from {service} (last {since}), "
            f"newest first\n{body}"
            + (f"\n… {dropped} more omitted" if dropped else ""),
            raw={"service": service, "lines": kept, "total_matches": len(hits)},
            truncated=dropped > 0,
        )


def register_log_tools(registry):
    for tool in (QueryLogsTool(), ErrorLogsTool()):
        registry.register(tool)
    return registry


__all__ = ["register_log_tools", "QueryLogsTool", "ErrorLogsTool"]
