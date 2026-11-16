"""Runbook search tool (AG-4): pgvector cosine search over doc_chunks."""

from __future__ import annotations

from typing import Any

from copilot.embed import Embedder

# Only public docs. docs/copilot/ holds the answer key and must never be returned.
SEARCH_SQL = (
    "SELECT source_file, anchor, heading, content, "
    "1 - (embedding <=> $1::vector) AS score "
    "FROM doc_chunks "
    "WHERE source_file LIKE 'docs/%' AND source_file NOT LIKE '%copilot/%' "
    "ORDER BY embedding <=> $1::vector LIMIT $2"
)


async def search_runbook(pool: Any, embedder: Embedder, query: str, k: int = 5) -> list[dict]:
    """Return [{source_file, anchor, heading, content, score}] for the top k chunks."""
    if isinstance(k, bool) or not isinstance(k, int) or not 1 <= k <= 10:
        raise ValueError("k must be an integer in 1..10")
    if not isinstance(query, str) or not query.strip():
        raise ValueError("query must be a non-empty string")
    (vec,) = await embedder.embed([query])
    literal = "[" + ",".join(repr(float(v)) for v in vec) + "]"
    rows = await pool.fetch(SEARCH_SQL, literal, k)
    return [
        {
            "source_file": r["source_file"],
            "anchor": r["anchor"],
            "heading": r["heading"],
            "content": r["content"],
            "score": float(r["score"]),
        }
        for r in rows
    ]
