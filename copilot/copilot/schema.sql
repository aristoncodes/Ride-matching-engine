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
  fields      jsonb NOT NULL DEFAULT '{}',
  line_hash   text NOT NULL UNIQUE      -- sha256 of the raw log line; dedupes re-loads
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

-- Ground truth for evaluation. Only the fault injector writes it; the copilot must never
-- read it (the copilot's DB role gets no grant on it).
CREATE TABLE IF NOT EXISTS incidents_truth (
  run_id           text NOT NULL,
  fault            text NOT NULL,
  target           text NOT NULL,
  start_time       timestamptz NOT NULL,
  expected_runbook text,                -- anchor, or NULL for no-fault runs
  PRIMARY KEY (run_id, fault)
);
