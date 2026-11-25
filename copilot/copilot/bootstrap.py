"""One-time database setup: apply the schema and grants, then index the docs for the runbook agent.

  COPILOT_OWNER_DSN=postgresql://owner:...@host/db uv run python -m copilot.bootstrap

Run it from a developer machine against a fresh database (a local container or Neon). It needs
the owner DSN, because it creates tables and grants. It is safe to re-run: the schema is
idempotent and indexing replaces each file's chunks. The roles (copilot_app, copilot_ro) must
exist first: see deploy/gcp/README.md. Prints counts only, never DSNs or document text.
"""

from __future__ import annotations

import argparse
import asyncio
import os
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]


async def run(root: Path) -> int:
    dsn = os.environ.get("COPILOT_OWNER_DSN", "").strip()
    if not dsn:
        print("COPILOT_OWNER_DSN is not set (the owner or superuser DSN).", file=sys.stderr)
        return 2

    from copilot import db
    from copilot.embed import LocalEmbedder
    from copilot.ingest.docs import index_docs

    pool = await db.connect(dsn)
    try:
        await db.apply_schema(pool)
        print("schema and grants applied")
        chunks = await index_docs(pool, LocalEmbedder(), root)
        print(f"indexed {chunks} chunks from {root.name}/docs")
    finally:
        await pool.close()
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Apply the copilot schema and index the docs")
    ap.add_argument("--root", type=Path, default=REPO_ROOT, help="repo root that holds docs/")
    args = ap.parse_args(argv)
    return asyncio.run(run(args.root))


if __name__ == "__main__":
    raise SystemExit(main())
