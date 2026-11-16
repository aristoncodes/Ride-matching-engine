"""Tests for the Embedder protocol, Markdown chunking, and docs indexing.

DB tests need a Postgres with pgvector at COPILOT_TEST_DSN. They run `schema.sql`
and DELETE all rows from doc_chunks, so point the DSN at a throwaway database only.
"""

from __future__ import annotations

import math
import os
from pathlib import Path

import asyncpg
import pytest

from copilot.embed import Embedder, FakeEmbedder, LocalEmbedder
from copilot.ingest.docs import chunk_markdown, index_docs, is_indexable, select_docs

FIXTURE = """# Guide

## <a id="dropped-requests"></a>Dropped requests (Sev-1)

Check the counter.

```bash
# not a heading
echo hi
```

### Sub detail

detail text

## Queue Backlog!

queue text
"""

DSN = os.environ.get("COPILOT_TEST_DSN")
requires_pg = pytest.mark.skipif(not DSN, reason="COPILOT_TEST_DSN not set")


def _by_heading(chunks, heading):
    return next(c for c in chunks if c.heading == heading)


def test_anchor_taken_from_a_id_tag() -> None:
    chunks = chunk_markdown("docs/x.md", FIXTURE)
    assert _by_heading(chunks, "Dropped requests (Sev-1)").anchor == "dropped-requests"


def test_slug_fallback_when_no_a_id() -> None:
    chunks = chunk_markdown("docs/x.md", FIXTURE)
    assert _by_heading(chunks, "Queue Backlog!").anchor == "queue-backlog"


def test_fenced_hash_lines_are_not_headings() -> None:
    chunks = chunk_markdown("docs/x.md", FIXTURE)
    assert [c.heading for c in chunks] == [
        "Guide",
        "Dropped requests (Sev-1)",
        "Sub detail",
        "Queue Backlog!",
    ]


def test_heading_kept_in_content_and_section_bounds() -> None:
    chunks = chunk_markdown("docs/x.md", FIXTURE)
    dropped = _by_heading(chunks, "Dropped requests (Sev-1)")
    assert dropped.content.startswith('## <a id="dropped-requests"></a>Dropped requests')
    assert "Check the counter." in dropped.content
    assert "### Sub detail" not in dropped.content  # stops at the next heading of any level
    assert "detail text" not in dropped.content
    assert "queue text" not in dropped.content
    assert all(c.source_file == "docs/x.md" for c in chunks)


def test_h1_chunk_does_not_span_h2_sections() -> None:
    text = "# Title\n\nintro line\n\n## Section Two\n\nsection body\n"
    chunks = chunk_markdown("docs/y.md", text)
    assert [c.heading for c in chunks] == ["Title", "Section Two"]
    title = _by_heading(chunks, "Title")
    assert "intro line" in title.content
    assert "Section Two" not in title.content
    assert "section body" not in title.content


def test_select_docs_excludes_docs_copilot_dir(tmp_path: Path) -> None:
    (tmp_path / "docs" / "copilot").mkdir(parents=True)
    (tmp_path / "docs" / "Runbook.md").write_text("# Runbook\n", encoding="utf-8")
    (tmp_path / "docs" / "copilot" / "PRD.md").write_text("# PRD\n", encoding="utf-8")
    selected = select_docs(tmp_path)
    assert [p.name for p in selected] == ["Runbook.md"]


def test_is_indexable_relative_and_absolute_paths(tmp_path: Path) -> None:
    root = tmp_path
    assert not is_indexable(root / "docs" / "copilot" / "PRD.md", root)
    assert not is_indexable(root / "docs" / "copilot" / "sub" / "x.md", root)
    assert is_indexable(root / "docs" / "Runbook.md", root)
    assert is_indexable(root / "docs" / "api" / "copilot" / "x.md", root)
    rel = Path(os.path.relpath(root))
    assert not is_indexable(root / "docs" / "copilot" / "PRD.md", rel)


def test_fake_embedder_is_deterministic_and_normalised() -> None:
    import asyncio

    emb = FakeEmbedder(dim=8)
    assert isinstance(emb, Embedder)
    vecs = asyncio.run(emb.embed(["alpha", "alpha", "beta"]))
    assert len(vecs) == 3
    assert all(len(v) == 8 for v in vecs)
    assert vecs[0] == vecs[1]
    assert vecs[0] != vecs[2]
    for v in vecs:
        assert math.isclose(sum(x * x for x in v), 1.0, rel_tol=1e-9)


def test_local_embedder_dim_matches_schema() -> None:
    assert LocalEmbedder.dim == 384
    assert FakeEmbedder().dim == 384


@pytest.mark.pg
@requires_pg
@pytest.mark.asyncio
async def test_index_docs_replaces_chunks(tmp_path: Path) -> None:
    docs = tmp_path / "docs"
    docs.mkdir()
    (docs / "a.md").write_text(FIXTURE, encoding="utf-8")
    (docs / "b.md").write_text('# B\n\n## <a id="x"></a>X\n\nbody\n', encoding="utf-8")
    schema = Path(__file__).resolve().parents[1] / "copilot" / "schema.sql"

    pool = await asyncpg.create_pool(DSN)
    try:
        async with pool.acquire() as conn:
            await conn.execute(schema.read_text(encoding="utf-8"))
            await conn.execute("DELETE FROM doc_chunks")

        emb = FakeEmbedder()
        first = await index_docs(pool, emb, tmp_path)
        second = await index_docs(pool, emb, tmp_path)
        assert first == second == 4 + 2  # a.md: 4 headings; b.md: 2 headings

        async with pool.acquire() as conn:
            count = await conn.fetchval("SELECT count(*) FROM doc_chunks")
            anchor = await conn.fetchval(
                "SELECT anchor FROM doc_chunks WHERE source_file = 'docs/a.md' "
                "AND heading = 'Dropped requests (Sev-1)'"
            )
        assert count == first
        assert anchor == "dropped-requests"
    finally:
        await pool.close()
