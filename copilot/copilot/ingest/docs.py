"""DP-2: chunk Markdown docs by heading and index them into doc_chunks."""

from __future__ import annotations

import hashlib
import os
import re
from dataclasses import dataclass
from pathlib import Path

import asyncpg

from copilot.embed import Embedder

EMBED_BATCH = 64

_HEADING_RE = re.compile(r"^(#{1,6})[ \t]+(.*?)[ \t]*$")
_ANCHOR_RE = re.compile(r'<a\s+id="([^"]+)"\s*>\s*</a>')
_ANCHOR_LINE_RE = re.compile(r'^\s*<a\s+id="([^"]+)"\s*>\s*</a>\s*$')
_FENCE_RE = re.compile(r"^\s{0,3}(```|~~~)")

# Directories directly under docs/ that must never reach the RAG corpus: docs/copilot holds
# the PRD (which contains the fault-to-diagnosis answer key) and internal planning notes.
EXCLUDED_DIR_PARTS = frozenset({"copilot"})

_INSERT_SQL = (
    "INSERT INTO doc_chunks "
    "(source_file, anchor, heading, content, content_hash, embedding) "
    "VALUES ($1, $2, $3, $4, $5, $6::vector)"
)


@dataclass
class Chunk:
    source_file: str
    anchor: str | None
    heading: str
    content: str


@dataclass
class _Heading:
    line: int
    level: int
    title: str
    anchor: str | None


def _slug(title: str) -> str:
    """GitHub-style heading slug: lowercase, drop punctuation, spaces to hyphens."""
    s = re.sub(r"[^\w\- ]", "", title.strip().lower())
    return s.replace(" ", "-")


def chunk_markdown(source_file: str, text: str) -> list[Chunk]:
    """Split Markdown into one chunk per heading.

    Each chunk's content starts with its heading line and runs to the next heading
    of any level, so no chunk repeats another section's text. Headings inside
    fenced code blocks are ignored.
    """
    lines = text.splitlines()
    heads: list[_Heading] = []
    fence: str | None = None

    for i, line in enumerate(lines):
        fm = _FENCE_RE.match(line)
        if fm:
            marker = fm.group(1)
            if fence is None:
                fence = marker
            elif marker == fence:
                fence = None
            continue
        if fence is not None:
            continue

        m = _HEADING_RE.match(line)
        if not m:
            continue
        rest = m.group(2)
        am = _ANCHOR_RE.search(rest)
        anchor = am.group(1) if am else None
        title = re.sub(r"\s+#+$", "", _ANCHOR_RE.sub("", rest).strip()).strip()

        if anchor is None:
            # The runbook style puts the anchor on the heading line; also accept it
            # on the first non-blank line under the heading.
            j = i + 1
            while j < len(lines) and not lines[j].strip():
                j += 1
            if j < len(lines):
                lm = _ANCHOR_LINE_RE.match(lines[j])
                if lm:
                    anchor = lm.group(1)

        heads.append(_Heading(line=i, level=len(m.group(1)), title=title, anchor=anchor))

    chunks: list[Chunk] = []
    first_line = heads[0].line if heads else len(lines)
    preamble = "\n".join(lines[:first_line]).strip()
    if preamble:
        chunks.append(Chunk(source_file, None, "", preamble))

    for idx, h in enumerate(heads):
        end = heads[idx + 1].line if idx + 1 < len(heads) else len(lines)
        content = "\n".join(lines[h.line : end]).strip()
        anchor = h.anchor or _slug(h.title) or None
        chunks.append(Chunk(source_file, anchor, h.title, content))

    return chunks


def _vector_literal(vec: list[float]) -> str:
    return "[" + ",".join(repr(float(x)) for x in vec) + "]"


def is_indexable(path: Path, root: Path) -> bool:
    """False for files under docs/<excluded dir>/. Works for relative or absolute paths."""
    parts = Path(os.path.abspath(path)).relative_to(os.path.abspath(root)).parts
    if len(parts) < 3 or parts[0] != "docs":
        return True
    return parts[1].lower() not in EXCLUDED_DIR_PARTS


def select_docs(root: Path, glob: str = "docs/**/*.md") -> list[Path]:
    """Sorted files matched by glob that pass is_indexable."""
    return sorted(p for p in root.glob(glob) if p.is_file() and is_indexable(p, root))


async def index_docs(
    pool: asyncpg.Pool,
    embedder: Embedder,
    root: Path,
    glob: str = "docs/**/*.md",
) -> int:
    """Full re-index: chunk and embed every matched file, then replace its rows.

    Each file's old chunks are deleted and new ones inserted in one transaction.
    Returns the number of chunks written. Incremental indexing (DP-3) is out of scope.
    """
    paths = select_docs(root, glob)

    files: list[tuple[str, str, list[Chunk]]] = []
    for path in paths:
        data = path.read_bytes()
        rel = path.relative_to(root).as_posix()
        chunks = chunk_markdown(rel, data.decode("utf-8"))
        files.append((rel, hashlib.sha256(data).hexdigest(), chunks))

    texts = [c.content for _, _, chunks in files for c in chunks]
    vectors: list[list[float]] = []
    for start in range(0, len(texts), EMBED_BATCH):
        vectors.extend(await embedder.embed(texts[start : start + EMBED_BATCH]))
    if len(vectors) != len(texts):
        raise RuntimeError(f"embedder returned {len(vectors)} vectors for {len(texts)} chunks")
    if any(len(v) != embedder.dim for v in vectors):
        raise RuntimeError(f"embedder returned vectors that are not dim {embedder.dim}")

    total = 0
    offset = 0
    async with pool.acquire() as conn:
        for rel, file_hash, chunks in files:
            rows = []
            for c in chunks:
                rows.append(
                    (
                        c.source_file,
                        c.anchor,
                        c.heading,
                        c.content,
                        file_hash,
                        _vector_literal(vectors[offset]),
                    )
                )
                offset += 1
            async with conn.transaction():
                await conn.execute("DELETE FROM doc_chunks WHERE source_file = $1", rel)
                if rows:
                    await conn.executemany(_INSERT_SQL, rows)
            total += len(rows)
    return total
