"""Shared test setup. Tests marked `pg` need Postgres at COPILOT_TEST_DSN and skip without it."""

from __future__ import annotations

import os

import pytest


def pytest_configure(config: pytest.Config) -> None:
    config.addinivalue_line("markers", "pg: needs Postgres at COPILOT_TEST_DSN")


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    if os.environ.get("COPILOT_TEST_DSN"):
        return
    skip = pytest.mark.skip(reason="COPILOT_TEST_DSN not set")
    for item in items:
        if "pg" in item.keywords:
            item.add_marker(skip)
