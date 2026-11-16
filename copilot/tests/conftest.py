"""Shared test setup. Tests marked `pg` need Postgres at COPILOT_TEST_DSN and skip without it."""

from __future__ import annotations

import os

import pytest


def pytest_configure(config: pytest.Config) -> None:
    config.addinivalue_line("markers", "pg: needs Postgres at COPILOT_TEST_DSN")
    # The pg tests DELETE whole tables (doc_chunks, logs, actions). Refuse any
    # database whose name does not end in "_test", so a run can never wipe
    # real local data. A past run did exactly that to the doc index.
    dsn = os.environ.get("COPILOT_TEST_DSN")
    if dsn:
        from urllib.parse import urlsplit

        name = urlsplit(dsn).path.lstrip("/")
        if not name.endswith("_test"):
            raise pytest.UsageError(
                f"COPILOT_TEST_DSN must point at a database named *_test (got {name!r}). "
                "The pg tests delete table contents."
            )


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    if os.environ.get("COPILOT_TEST_DSN"):
        return
    skip = pytest.mark.skip(reason="COPILOT_TEST_DSN not set")
    for item in items:
        if "pg" in item.keywords:
            item.add_marker(skip)
