"""Loader for ``queries/signals.yaml``.

Mirrors ``gateway/internal/signals`` deliberately: both sides read the same file,
so the agent measures a service exactly the way the verifier does. Reimplementing
the queries here instead would mean reimplementing their mistakes, and the victim
would be the agent -- it would query a failing service, see zero errors, and
report the service healthy. Its competence would be measured against a bug in its
own instruments.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path
from typing import Mapping

import yaml

DEFAULT_RELATIVE_PATH = Path("queries/signals.yaml")

# Placeholders are %NAME%, not {name}: PromQL is full of braces, so brace-based
# templating would fight the query syntax.
_PLACEHOLDER = re.compile(r"%([A-Z_]+)%")


class SignalError(Exception):
    """Raised for an unknown signal or an unsatisfied placeholder."""


@dataclass(frozen=True)
class Signal:
    name: str
    summary: str
    unit: str
    promql: str
    min_delta: float = 0.0
    multi_series: bool = False


class SignalCatalog:
    def __init__(self, signals: Mapping[str, Signal], defaults: Mapping[str, str]):
        self._signals = dict(signals)
        self._defaults = {k.upper(): v for k, v in defaults.items()}

    # -- construction ----------------------------------------------------

    @classmethod
    def load(cls, path: str | Path) -> "SignalCatalog":
        path = Path(path)
        raw = yaml.safe_load(path.read_text())
        if not raw or not raw.get("signals"):
            raise SignalError(f"{path} declares no signals")

        signals: dict[str, Signal] = {}
        for name, body in raw["signals"].items():
            promql = (body or {}).get("promql", "").strip()
            if not promql:
                raise SignalError(f"{path}: signal {name!r} has no promql")
            signals[name] = Signal(
                name=name,
                summary=body.get("summary", ""),
                unit=body.get("unit", ""),
                promql=promql,
                min_delta=float(body.get("min_delta", 0) or 0),
                multi_series=bool(body.get("multi_series", False)),
            )
        return cls(signals, raw.get("defaults") or {})

    @classmethod
    def load_from_repo(cls, start: str | Path | None = None) -> "SignalCatalog":
        """Walk up from ``start`` to find the catalog, so callers work from anywhere."""
        here = Path(start or Path.cwd()).resolve()
        for candidate in (here, *here.parents):
            path = candidate / DEFAULT_RELATIVE_PATH
            if path.is_file():
                return cls.load(path)
        raise SignalError(
            f"could not find {DEFAULT_RELATIVE_PATH} walking up from {here}"
        )

    # -- access ----------------------------------------------------------

    def names(self) -> list[str]:
        return sorted(self._signals)

    def get(self, name: str) -> Signal:
        try:
            return self._signals[name]
        except KeyError:
            raise SignalError(
                f"unknown signal {name!r} (have {self.names()})"
            ) from None

    def query(self, name: str, **subs: str) -> str:
        """Render a signal's PromQL.

        An unsubstituted placeholder raises rather than being sent through. A
        literal ``%SERVICE%`` in PromQL does not fail -- it matches nothing and
        returns zero, which is indistinguishable from a healthy service.
        """
        signal = self.get(name)
        merged = dict(self._defaults)
        merged.update({k.upper(): str(v) for k, v in subs.items()})

        rendered = signal.promql
        for key, value in merged.items():
            rendered = rendered.replace(f"%{key}%", value)

        leftover = _PLACEHOLDER.search(rendered)
        if leftover:
            raise SignalError(
                f"signal {name!r} still contains {leftover.group(0)} after "
                f"substitution; an unsubstituted placeholder matches nothing and "
                f"returns zero, which looks exactly like a healthy service"
            )
        return rendered.strip()


_cached: SignalCatalog | None = None


def catalog() -> SignalCatalog:
    """Process-wide catalog, loaded once."""
    global _cached
    if _cached is None:
        _cached = SignalCatalog.load_from_repo(Path(__file__).resolve().parent)
    return _cached
