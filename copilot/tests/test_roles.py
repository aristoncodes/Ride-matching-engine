"""Role-isolation tests: the copilot roles must not read ground truth. Need initdb roles."""

from __future__ import annotations

import os
from collections.abc import AsyncIterator
from urllib.parse import quote, urlsplit, urlunsplit

import asyncpg
import pytest
import pytest_asyncio

from copilot.db import apply_schema, connect

pytestmark = pytest.mark.pg

# Role passwords come from the same env vars the compose file uses.
_PW_ENV = {
    "copilot_app": "COPILOT_APP_PASSWORD",
    "copilot_ro": "COPILOT_RO_PASSWORD",
    "fault_injector": "FAULT_INJECTOR_PASSWORD",
}


def _dsn_for(role: str) -> str:
    parts = urlsplit(os.environ["COPILOT_TEST_DSN"])
    pw = os.environ.get(_PW_ENV[role], "local-dev-only")
    host = parts.hostname or "localhost"
    netloc = f"{role}:{quote(pw, safe='')}@{host}" + (f":{parts.port}" if parts.port else "")
    return urlunsplit((parts.scheme, netloc, parts.path, parts.query, parts.fragment))


@pytest_asyncio.fixture
async def owner() -> AsyncIterator[asyncpg.Connection]:
    pool = await connect(os.environ["COPILOT_TEST_DSN"])
    await apply_schema(pool)
    async with pool.acquire() as conn:
        missing = [
            r
            for r in _PW_ENV
            if not await conn.fetchval("SELECT 1 FROM pg_roles WHERE rolname = $1", r)
        ]
        if missing:
            await pool.close()
            pytest.skip(f"roles missing (run via compose initdb): {', '.join(missing)}")
        yield conn
    await pool.close()


@pytest.mark.asyncio
@pytest.mark.parametrize("role", ["copilot_ro", "copilot_app"])
async def test_copilot_roles_cannot_read_truth(owner: asyncpg.Connection, role: str) -> None:
    conn = await asyncpg.connect(_dsn_for(role))
    try:
        with pytest.raises(asyncpg.InsufficientPrivilegeError):
            await conn.fetch("SELECT * FROM incidents_truth")
    finally:
        await conn.close()


@pytest.mark.asyncio
async def test_fault_injector_can_insert_truth(owner: asyncpg.Connection) -> None:
    conn = await asyncpg.connect(_dsn_for("fault_injector"))
    try:
        await conn.execute(
            "INSERT INTO incidents_truth (run_id, fault, target, start_time)"
            " VALUES ('test-roles', 'test', 'none', now())"
            " ON CONFLICT (run_id, fault) DO NOTHING"
        )
    finally:
        await conn.close()
        await owner.execute("DELETE FROM incidents_truth WHERE run_id = 'test-roles'")
