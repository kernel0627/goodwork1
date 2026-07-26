"""Shared plumbing for the agent's read-only diagnostic tools.

Three concerns are handled here rather than in each tool.

**Truncation.** A single PromQL query can match hundreds of series and a
container can emit thousands of log lines a minute. Handing that to the model
would spend the context window on one call and leave nothing for the reasoning
the call was meant to support. Every result is capped, and the cap is *stated in
the output* -- an agent that cannot tell it saw a partial answer will conclude
things the data does not support.

**Errors as data.** A failed tool call returns a result the model can read
instead of raising. Diagnosis is exactly the situation where things fail: the
service being investigated may be the one that is down. "This call failed
because X" is a diagnostic observation, not a crash.

**Schemas.** Each tool declares its own JSON Schema, which is what gets handed to
the model. Parameters are validated before execution so a malformed call comes
back as a correctable message rather than a stack trace.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Callable, Iterable, Protocol


class ToolError(Exception):
    """Raised for a malformed call; converted to a readable result by the runner."""


@dataclass
class ToolResult:
    """What a tool hands back.

    ``content`` is what the model reads. ``raw`` keeps the structured form for
    the trace and for scoring, which must not depend on how output was rendered
    for a model.
    """

    ok: bool
    content: str
    raw: Any = None
    truncated: bool = False
    error: str | None = None
    meta: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def failure(cls, message: str, **meta: Any) -> "ToolResult":
        return cls(ok=False, content=f"ERROR: {message}", error=message, meta=meta)


class Tool(Protocol):
    name: str
    description: str

    def schema(self) -> dict[str, Any]: ...

    def run(self, **kwargs: Any) -> ToolResult: ...


# Caps chosen to keep any single call well under a few thousand tokens.
MAX_SERIES = 25
MAX_LOG_LINES = 60
MAX_LINE_CHARS = 400
MAX_TRACES = 15


def truncate_line(line: str, limit: int = MAX_LINE_CHARS) -> str:
    if len(line) <= limit:
        return line
    return line[:limit] + f" …(+{len(line) - limit} chars)"


def cap(items: list[Any], limit: int) -> tuple[list[Any], int]:
    """Return the first ``limit`` items and how many were dropped."""
    if len(items) <= limit:
        return items, 0
    return items[:limit], len(items) - limit


def render_table(rows: Iterable[tuple[str, str]], dropped: int = 0) -> str:
    """Render label/value pairs compactly, stating any omission."""
    rows = list(rows)
    if not rows:
        return "(no data)"
    width = min(max(len(label) for label, _ in rows), 60)
    body = "\n".join(f"{label:<{width}}  {value}" for label, value in rows)
    if dropped:
        body += f"\n… {dropped} more omitted (result truncated)"
    return body


class Registry:
    """Holds the tools available to an agent and dispatches calls to them.

    Registration is explicit so an ablation can withhold a tool -- the topology
    ablation, for instance -- simply by building a registry without it.
    """

    def __init__(self) -> None:
        self._tools: dict[str, Tool] = {}

    def register(self, tool: Tool) -> Tool:
        if tool.name in self._tools:
            raise ValueError(f"tool {tool.name!r} already registered")
        self._tools[tool.name] = tool
        return tool

    def __contains__(self, name: str) -> bool:
        return name in self._tools

    def names(self) -> list[str]:
        return sorted(self._tools)

    def without(self, *names: str) -> "Registry":
        """A copy lacking the named tools, for ablations."""
        clone = Registry()
        for name, tool in self._tools.items():
            if name not in names:
                clone._tools[name] = tool
        return clone

    def specs(self) -> list[dict[str, Any]]:
        """Tool definitions in the shape the Anthropic API expects."""
        return [
            {
                "name": tool.name,
                "description": tool.description,
                "input_schema": tool.schema(),
            }
            for tool in (self._tools[n] for n in self.names())
        ]

    def call(self, name: str, arguments: dict[str, Any] | None = None) -> ToolResult:
        tool = self._tools.get(name)
        if tool is None:
            return ToolResult.failure(
                f"no such tool {name!r}; available: {', '.join(self.names())}"
            )
        try:
            return tool.run(**(arguments or {}))
        except ToolError as exc:
            return ToolResult.failure(str(exc))
        except TypeError as exc:
            # Usually a wrong or missing argument. Reported so the model can fix
            # the call rather than being told only that something broke.
            return ToolResult.failure(f"bad arguments for {name}: {exc}")
        except Exception as exc:  # noqa: BLE001
            # The service under investigation may be the one that is down, so an
            # unexpected failure here is itself an observation.
            return ToolResult.failure(f"{type(exc).__name__}: {exc}")


def require(kwargs: dict[str, Any], key: str) -> Any:
    if key not in kwargs or kwargs[key] in (None, ""):
        raise ToolError(f"missing required argument {key!r}")
    return kwargs[key]


def as_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, default=str)
