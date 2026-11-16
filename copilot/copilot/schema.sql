-- Copilot Postgres schema (PLAN.md 2.1). Idempotent: safe to apply on every start.
-- Only the logs table for now; other tables arrive with their tickets.

CREATE TABLE IF NOT EXISTS logs (
  id          bigserial PRIMARY KEY,
  ts          timestamptz NOT NULL,
  service     text NOT NULL,            -- ingestd | requestd | batcherd
  level       text NOT NULL,
  request_id  text,
  event       text,
  error       text,
  fields      jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS logs_ts_idx ON logs (ts);
CREATE INDEX IF NOT EXISTS logs_request_id_idx ON logs (request_id);

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS doc_chunks (
  id           bigserial PRIMARY KEY,
  source_file  text NOT NULL,           -- e.g. docs/Runbook.md
  anchor       text,                    -- e.g. worker-crash-loop; NULL if none
  heading      text NOT NULL,
  content      text NOT NULL,
  content_hash text NOT NULL,           -- file hash, for DP-3
  embedding    vector(768) NOT NULL     -- text-embedding-005 dims (assumption, §6)
);
CREATE INDEX IF NOT EXISTS doc_chunks_embedding_idx ON doc_chunks USING hnsw (embedding vector_cosine_ops);
