# Agent HTTP contract

The Go runner orchestrates episodes; the Python agent service diagnoses them.
This is the seam between them.

## Why the agent calls its own tools

The runner does not mediate tool calls. It hands over an alert and receives a
diagnosis, and everything in between happens inside the agent.

The agent's observation tools are strictly read-only, so nothing it does during
diagnosis needs supervising. **Write actions are different** and will not travel
this path: remediation goes through the Go gateway, which owns idempotency,
approval and compensation. Keeping observation unmediated and mutation mediated
is the same split as everywhere else in this project — Go owns side effects,
Python owns inference.

A useful consequence: the runner cannot leak anything to the agent beyond the
alert, because there is no other channel between them.

---

## `POST /diagnose`

### Request

```json
{
  "episode_id": "adFailure-L2-001",
  "alert": {
    "service": "ad",
    "summary": "error ratio on ad is 9.5% (baseline 0.0%), sustained 3m",
    "fired_at": "2026-07-26T16:04:11Z",
    "observed": {
      "signal": "error_ratio",
      "value": 0.0952,
      "baseline": 0.0,
      "unit": "ratio"
    }
  },
  "budget": { "max_steps": 20, "deadline_seconds": 300 }
}
```

`alert.service` is the **symptom** service — where the alarm fired. It is not
the answer, and for most scenarios it is not the root cause. `alert.summary` is
generated from measured metrics, never authored, so it cannot contain a hint the
metrics themselves do not.

Nothing else is provided. No scenario id meaning, no fault list, no service
inventory: the agent discovers the system through its tools, the way an oncall
engineer paged at 3am does.

### Response

```json
{
  "root_cause_service": "ad",
  "root_cause_class": "error",
  "confidence": 0.82,
  "healthy": false,
  "escalate": false,
  "steps": 7,
  "tool_calls": ["get_service_health", "get_service_topology", "find_error_traces"],
  "reasoning": "…",
  "remediation": [
    { "action": "restart_service", "target": "ad", "rationale": "…" }
  ],
  "input_tokens": 18422,
  "output_tokens": 1130
}
```

| Field | Meaning |
|---|---|
| `root_cause_service` | Service at fault. Scored by exact match, case and whitespace insensitive. |
| `root_cause_class` | One of `error` `latency` `resource` `connectivity` `health` `queue`. |
| `confidence` | 0–1. Not scored; recorded to check whether confidence tracks correctness. |
| `healthy` | The agent asserting nothing is wrong. Distinct from an empty `root_cause_service`, which means it failed to answer. |
| `escalate` | Handing off rather than concluding. Not a wrong answer, but tracked. |
| `steps` / `tool_calls` | Tool usage, for strategy analysis and the step-budget check. |
| `reasoning` | Free text, **never scored**. Read during Bad Case analysis. |
| `remediation` | Proposed actions. Not executed at this stage; the gateway will own that. |
| `input_tokens` / `output_tokens` | Feed cost-per-episode, which any real deployment has to answer for. |

`healthy` and `root_cause_service` are kept apart deliberately. "I checked and
the system is fine" and "I could not work it out" are different outcomes: the
first is correct on a healthy control and a missed fault otherwise, the second is
never correct. Collapsing them would make an agent that gives up look like one
that cleared the system.

### `GET /healthz`

```json
{ "ok": true, "brain": "random", "tools": 12 }
```

Polled by the runner before a run starts, so a misconfigured agent fails in the
first second rather than 40 minutes in.

---

## Brains

The service is a thin shell around a swappable *brain*. Same contract, same
tools, different decision procedure — which is what makes the control arms
comparable.

| Brain | What it does |
|---|---|
| `random` | Calls random tools, answers a random service. Not a baseline — a test of the harness. |
| `checklist` | Fixed sequence of tool calls, then one model call to conclude. **The control arm that matters.** |
| `oneshot` | One model call from the alert alone, no tools. |
| `react` | Tool loop, no structured hypothesis tracking. |
| `medic` | Hypothesis–verify loop with structured decisions. |

### Why `random` exists

It runs first, before any real brain is written.

A scoring framework can be wrong in ways that produce plausible numbers. Ground
truth wired to the wrong field, symptom echo counted backwards, discards silently
dropped from a denominator — every one of those yields a run that completes and
reports something believable. Measured against a random agent the expected
results are known in advance, and anything else means the harness is broken:

- Exact accuracy near `1/n_services` × `1/n_classes`, i.e. a few percent.
- Symptom echo near chance, not near zero.
- False alarms on nearly every healthy control.
- Discards only where a fault genuinely failed to fire.

If a random agent scores well, the bug is in the scoring, not in the luck.
