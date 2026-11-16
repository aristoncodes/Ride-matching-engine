"""Postgres connection pool and schema setup for the copilot database."""

from __future__ import annotations

from importlib import resources

import asyncpg


async def connect(dsn: str) -> asyncpg.Pool:
    """Create a connection pool for the copilot database."""
    return await asyncpg.create_pool(dsn, min_size=1, max_size=5)


def _schema_sql() -> str:
    # Read from the installed package so this works outside the source tree.
    return resources.files("copilot").joinpath("schema.sql").read_text(encoding="utf-8")


def _grants_sql() -> str:
    return resources.files("copilot").joinpath("grants.sql").read_text(encoding="utf-8")


async def apply_schema(pool: asyncpg.Pool) -> None:
    """Apply schema.sql then grants.sql, as the owner. Idempotent.

    Grants for a role are skipped when that role does not exist (single-user local runs).
    """
    async with pool.acquire() as conn:
        await conn.execute(_schema_sql())
        await conn.execute(_grants_sql())
