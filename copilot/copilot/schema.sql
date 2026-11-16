-- Copilot Postgres schema (PLAN.md 2.1). Idempotent: safe to apply on every start.

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

-- NOTE: changing the embedding dimension requires re-creating doc_chunks on an existing
-- database (CREATE TABLE IF NOT EXISTS will not alter it), then re-indexing the docs.
CREATE TABLE IF NOT EXISTS doc_chunks (
  id           bigserial PRIMARY KEY,
  source_file  text NOT NULL,           -- e.g. docs/Runbook.md
  anchor       text,                    -- e.g. worker-crash-loop; NULL if none
  heading      text NOT NULL,
  content      text NOT NULL,
  content_hash text NOT NULL,           -- file hash, for DP-3
  embedding    vector(384) NOT NULL     -- BAAI/bge-small-en-v1.5 dims (local fastembed)
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

-- DP-4: one row per incident the copilot has seen.
CREATE TABLE IF NOT EXISTS incidents_seen (
  id           uuid PRIMARY KEY,
  opened_at    timestamptz NOT NULL,
  source       text NOT NULL,           -- alert | detector | question
  trigger      jsonb NOT NULL,          -- raw alert or detection event
  diagnosis    jsonb,                   -- Diagnosis, or NULL if fallback only
  fallback     jsonb,                   -- raw evidence + top runbook matches
  status       text NOT NULL,           -- running | diagnosed | needs_human | failed
  verdict      text,                    -- correct | partly | wrong, set by human
  verdict_by   text,
  verdict_at   timestamptz,
  tokens_in    integer NOT NULL DEFAULT 0,
  tokens_out   integer NOT NULL DEFAULT 0,
  cost_usd     numeric(10,6) NOT NULL DEFAULT 0,
  latency_ms   integer,
  trace        jsonb                    -- InvestigationResult.trace: per-step agent/tool/ms
);

-- AC-3: every action transition records who and when.
CREATE TABLE IF NOT EXISTS actions (
  id           uuid PRIMARY KEY,
  incident_id  uuid NOT NULL REFERENCES incidents_seen(id),
  kind         text NOT NULL CHECK (kind IN ('restart_pods','scale')),
  deployment   text NOT NULL CHECK (deployment IN ('matching-engine','batcherd')),
  replicas     integer,                 -- scale only; bounded by SCALE_LIMITS
  reason       text NOT NULL,
  status       text NOT NULL CHECK (status IN ('proposed','approved','rejected','executed','failed')),
  proposed_at  timestamptz NOT NULL,
  decided_by   text,
  decided_at   timestamptz,
  executed_at  timestamptz,
  result       text
);

-- Existing databases: CREATE TABLE IF NOT EXISTS does not add columns.
ALTER TABLE incidents_seen ADD COLUMN IF NOT EXISTS trace jsonb;
