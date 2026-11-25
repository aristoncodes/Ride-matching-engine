"""OpenTelemetry tracing for agent steps (ticket 3.2).

No exporter is configured here, so the SDK default is a no-op. The Cloud Trace exporter is
ticket 3.3. Span attributes carry agent, tool, duration and status only: never prompt text or
model output.
"""

from __future__ import annotations

import os
import time

from opentelemetry import trace

_NAME = "copilot"


def configure_tracing() -> None:
    """Print spans to stdout when OTEL_TRACES_EXPORTER=console. Otherwise leave the no-op default.

    Spans carry only the attributes set in emit_step, so the console output holds no prompt
    text. Cloud Trace is ticket 3.3 and is not wired here.
    """
    if os.environ.get("OTEL_TRACES_EXPORTER", "").strip().lower() != "console":
        return
    if not isinstance(trace.get_tracer_provider(), trace.ProxyTracerProvider):
        return  # a provider is already set (tests, or an earlier call): do not replace it
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor, ConsoleSpanExporter

    provider = TracerProvider()
    provider.add_span_processor(BatchSpanProcessor(ConsoleSpanExporter()))
    trace.set_tracer_provider(provider)


def get_tracer() -> trace.Tracer:
    return trace.get_tracer(_NAME)


def emit_step(agent: str, tool: str | None, status: str, ms: int) -> None:
    """Record a finished step as a span that ends now and started `ms` ago."""
    end = time.time_ns()
    span = get_tracer().start_span(f"{agent}.step", start_time=end - ms * 1_000_000)
    span.set_attribute("agent", agent)
    span.set_attribute("tool", tool or "")
    span.set_attribute("duration_ms", ms)
    span.set_attribute("status", status)
    span.end(end_time=end)
