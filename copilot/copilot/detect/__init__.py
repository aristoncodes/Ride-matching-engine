"""Anomaly detection over Prometheus metrics (PRD AD-1)."""

from copilot.detect.zscore import (
    AD1_QUERIES,
    DetectionEvent,
    detect,
    fetch_series,
    prometheus_url,
    run_once,
    to_diagnose_payload,
)

__all__ = [
    "AD1_QUERIES",
    "DetectionEvent",
    "detect",
    "fetch_series",
    "prometheus_url",
    "run_once",
    "to_diagnose_payload",
]
