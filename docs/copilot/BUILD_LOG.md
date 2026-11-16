# Incident Copilot: build log

Running record of decisions, actions, and problems, kept for interview preparation.
Entries are in order. "Decided by" says who made the call: the owner (you) or me.

Branch: `feat/incident-copilot` (created from `main`; the pre-existing uncommitted work on `main` was carried over and left untouched).

---

## Part A: Planning (Phase 0)

### A1. Read the spec and the repo before planning
- **What:** Read `CLAUDE.md`, `KICKOFF_PROMPT.md`, and the PRD PDF. Checked which files existed.
- **Found:** `docs/copilot/PRD.md` did not exist, nor `copilot/`, nor `.claude/agents/`.
- **Why it matters:** CLAUDE.md says "read the PRD before planning", but the file wasn't in the repo. I saved the PDF content as `docs/copilot/PRD.md` so every later ticket can reference it by path.

### A2. Substitutions for agent types that don't exist here
- **Decided by:** me, out of necessity.
- The kickoff names `pantry-scout`, `sous-chef-python`, `sous-chef-infra`, `line-cook`, `test-runner`, and `reviewer`. This session only has `claude`, `claude-code-guide`, `Explore`, `general-purpose`, `Plan`, `statusline-setup`.
- **Choice:** `Explore` for read-only scouting. `general-purpose` for implementation tickets, with the cook rules (scope, 15-line reply, no commits) written into each prompt.
- **Consequence:** the model tier per role (Sonnet, Haiku, Opus) in CLAUDE.md is not enforced. Each cook runs on the default model. Worth stating honestly in the interview.

### A3. Phase 0 scouting: four parallel read-only reports
- **What:** Metrics inventory, alert rules, runbook sections, and logger/admin/scripts. Ran in parallel.
- **Result:** 15 metrics, 8 alert rules (no severity, no runbook links), 6 incident anchors, scripts summarized.

### A4. Plan written, then stopped for approval
- **What:** `docs/copilot/PLAN.md`: package layout, binding contracts (Postgres schema, `Diagnosis` model, tool signatures, API models), and the ticket table for three phases.
- **Repo facts that changed the plan** (listed in PLAN.md section 0):
  1. No dropped-request metric exists (`ride_requests_dropped_total` in the runbook does not match any metric).
  2. Alert rules had no severity or runbook anchors.
  3. Runbook anchor is `worker-crash-loop`; the PRD spelled it `worker-crashloop`.
  4. No Redis runbook section.
  5. Runbook says high latency at 3.5 s; the SLO constant is 5 s.
  6. Traffic burst and slow solver both look like high latency.
  7. **Admin ports differ per service**: 6060 (ingestd), 6061 (requestd), 6062 (batcherd). CLAUDE.md and the PRD both say 6060 for all three. This is a documentation error I found by reading the code.

---

## Part B: Decisions (approved by you: "take your decisions and choose the best")

| # | Question | Choice | Reason | Tradeoff |
|---|---|---|---|---|
| D1 | `dropped-requests` has no metric | Map the runbook entry to the dead-letter alert (`increase(ridematch_queue_dead_lettered[10m]) > 0`) | Keeps engine changes at zero, as CLAUDE.md requires. Dead-lettered requests are the closest real signal to dropped requests | Not identical. A request can be dropped without being dead-lettered. |
| D2 | Redis stall has no runbook section | Add a `redis-stall` section to `docs/Runbook.md` | The PRD needs every fault to map to a runbook section, and you asked me to make everything | Changes a document the team owns; I said so in the plan. |
| D3 | 3.5 s runbook vs 5 s SLO for latency | 3.5 s is the diagnosis threshold in the runbook; the 5 s SLO stays the paging threshold in alerts | The runbook is what the copilot cites, so its number should be the one it uses | Two thresholds for one symptom can confuse on-call. |
| D4 | Embedding model | Vertex `text-embedding-005`, 768 dimensions | Current Google text-embedding model on Vertex; fits a pgvector column | Dimension is an assumption; must be confirmed against the live API. |
| D5 | GCP setup | Deferred to Phase 3 | Local kind first, per PRD deployment stages | Needs project ID and region from you later. |
| D6 | Redis alert rule | **Removed** (see C6) | It could never fire | Redis stalls are not alertable yet. |
| D7 | Commit granularity | One commit per ticket, but only for files that are entirely ours | Your existing uncommitted edits in `requestd`/`batcherd` `main.go` must not be swept into our commits without your review | Ticket 1.1 stays uncommitted for now (see C4). |

---

## Part C: What happened, in order

### C1. Branch created
- `git checkout -b feat/incident-copilot`. Existing modified files came along untouched.

### C2. Ticket 1.4 (copilot scaffold): committed `161d32c`
- Cook created `copilot/` with pyproject (hatchling), ruff config, Dockerfile, compose (pgvector + Prometheus), `.env.example` with placeholder names only, a smoke test.
- **Problem:** `uv` was not installed, so the cook could not run the acceptance check.
- **Decision (me):** `uv` is a build tool the project needs, so I installed it with Homebrew (`uv 0.13.8`). Installing project tooling counts as setup, not a system change.
- **Result:** `uv sync`, `ruff check` (passed), `pytest` (1 passed). Committed.

### C3. Ticket 1.1 (JSON logging): done, **not committed**
- Changed the logger in `ingestd`, `requestd`, `batcherd` `main.go` from `NewTextHandler` to `NewJSONHandler`, with a comment explaining why (the DP-1 loader parses these lines).
- `go build ./...` passes.
- **Problems found:**
  - `go test ./...` has one failure in `internal/locations`: `TestNearbyManyIsFasterThanSequential` (a performance assertion; it reports pipelined 0.95× vs sequential). It needs local Redis and is unrelated to logging. **Not yet confirmed whether it also fails on `main`.**
  - The ticket says "keep `request_id`", but `request_id` isn't set in these three files. It appears only in `internal/queue/redis_stream.go` and `internal/api/errors.go`. The ticket's premise was wrong.
  - `golangci-lint` is not installed, so the linter hasn't run.
- **Why not committed:** `requestd` and `batcherd` `main.go` already had your uncommitted edits. Committing them would include your unreviewed changes. `ingestd/main.go` was clean, but I'm keeping the ticket as one unit.
- **Open:** need your decision on whether to commit the logger change alone, or to review the existing edits first.

### C4. Ticket 1.2 (Prometheus, Alertmanager): committed `fa257b1`
- Cook wrote `k8s/base/prometheus.yaml`: Prometheus v2.54.1, Alertmanager v0.27.0, Role and ServiceAccount, and rules.
- **Scrape design:** pod-level `kubernetes_sd` pointing at pod IP on each admin port. Chosen over editing `go-services.yaml`, because those Services are NodePort, and exposing pprof on NodePort would open it on the node.
- **Problem:** `kubectl apply --dry-run` could not run (no reachable cluster). `promtool` isn't installed. Validation was YAML parsing only.
- **Mistake caught (C6):** the RedisErrors rule could never fire. Only `batcher.go` emits `DependencyErrorsTotal`, and only with `dependency="engine"`. I removed the rule before committing.
- **Known gaps:** Prometheus stores data in `emptyDir` (history lost on restart). The copilot Service doesn't exist yet, so webhook delivery to `copilot.ride-matching:8000` will fail until Phase 2.

### C5. Ticket 1.3 (fault injector) and runbook section: committed `fa257b1`
- `k8s/fault-inject.sh`: six faults plus `--none`. Each run writes the `incidents_truth` row **before** injecting, so no fault runs unrecorded. An EXIT trap restores the original replicas, CPU limit, and Redis pause state.
- Truth rows and expected runbook are never printed to stdout (the copilot must not be able to read them).
- `traffic_burst` uses the existing `infrastructure/cmd/loadtest`.
- **Checks run:** `bash -n` passes; usage and error paths exit correctly. **Not run:** shellcheck (not installed), any real fault (no cluster), the SQL path (no database).
- **Open concerns:** `ingestd` and `requestd` have HPAs with minReplicas 2. Scaling `ingestd` to 0 may fight the HPA. Needs a live check. `--none` runs record target `none`.

### C6. Mistakes and corrections
| Mistake | Found by | Fix |
|---|---|---|
| Redis alert rule that never fires | Cook's own report | Removed before commit |
| Plan said all three admin ports are 6060 | Cook reading `main.go` | PLAN.md corrected; CLAUDE.md still wrong (not changed; your file) |
| "Keep request_id" in ticket 1.1 with no request_id in those files | Cook | Open; needs decision |

### C8. Ticket 1.5 (logs loader): done, not committed
- Files: `copilot/schema.sql` (logs table and indexes, `IF NOT EXISTS`), `copilot/db.py` (asyncpg pool, schema applied from package resources), `copilot/ingest/logs.py` (line parser plus loader), tests.
- **Design decisions (cook's, I accepted them):**
  - Idempotency by delete-then-insert per (service, time window) of the file, in one transaction. Simple, and re-running a file doesn't duplicate rows. Tradeoff: it can wipe rows the file doesn't contain if the window overlaps. Documented in the module docstring.
  - Bulk insert with `copy_records_to_table`, not per-row inserts.
  - slog timestamps have nanosecond precision, which Python's `fromisoformat` rejects, so they're truncated to microseconds.
  - The `CREATE EXTENSION vector` line was left out (logs only). It goes in with the docs ticket.
- **Results:** `ruff` clean; 5 tests pass; 4 Postgres tests skipped.
- **Not verified:** the SQL and COPY path against a live database. Docker isn't running, so Postgres wasn't started. Needs a live run before merge.
- **Interview point:** the pure parser is tested without a database, so the logic is covered even when the DB is unavailable.

### C9. Ticket 1.7 (z-score detector): done, not committed
- Files: `copilot/copilot/detect/zscore.py` (PromQL constants, pure `detect()`, async Prometheus fetch, `to_diagnose_payload()` for AD-3), tests.
- **Design:** the detection logic is a pure function over (timestamp, value) pairs. Tests need no network. Rule: a point is deviant if |z| > 3 against the previous 30 points; an event fires after 3 consecutive deviant points.
- **Results:** `ruff` clean; 23 passed, 5 skipped (Postgres tests), 0 failed.
- **Choices I'm accepting for now:**
  - Tests use synthetic series and a mocked HTTP transport, not a recorded Prometheus fixture. Fine for unit tests.
  - Queue metrics are summed across tenants per timestamp.
- **Open design issue (interview-worthy):** deviant points stay in the rolling window. So a sustained level shift becomes the new normal after about 30 points, and the detector goes quiet. Excluding deviant points from the baseline would fix that, but it's a design choice to make deliberately.
- **Open:** `run_once()` has no test, because it builds its own HTTP client. Adding a client parameter would make it testable.
- **Open:** the `/diagnose` payload keys are the cook's choice. The API ticket (2.4) has to confirm them.

### C10. Ticket 1.6 (docs chunking and embeddings): done, not committed
- Files: `copilot/copilot/embed.py` (Embedder protocol, Vertex `text-embedding-005` at 768 dimensions, `FakeEmbedder` for tests, in-memory sha256 cache), `copilot/copilot/ingest/docs.py` (chunking, `index_docs`), tests.
- **Results:** 23 passed, 5 skipped (Postgres), `ruff` clean. Chunking the real `docs/Runbook.md` found all 7 expected anchors.
- **Bug found in my review of the output:** the H1 title chunk ran to the end of the file, so every section's text was also inside the title chunk. That duplicates text in the index and skews retrieval. Fix: each heading starts a new chunk, and a chunk ends at the next heading of any level. Sent to a cook (in progress at time of writing).
- **Schema decision:** the doc table and the pgvector extension are being merged into `schema.sql`, so there's one schema file, not two.
- **Open:** `index_docs` deletes and re-inserts per file, but never removes chunks for files that were deleted. Acceptable for now.
- **Open:** the DB test for `index_docs` hasn't run, because Postgres isn't up. It deletes all `doc_chunks` rows, so `COPILOT_TEST_DSN` must point at a throwaway database.
- **Correction to PLAN.md section 0, item 5:** the Redis-stall section does exist now (I added it in 1.3), so that gap is closed.

### C7. Tickets still running at time of writing
- 1.5 logs loader (Postgres loader with idempotent delete-then-insert per file)
- 1.6 docs chunking and embedding (Embedder protocol, fake embedder for tests)
- 1.7 z-score detector (pure function plus Prometheus HTTP fetch)

Results will be added here when each one reports.

---

## Part D: Interview notes

Concepts to be able to explain:
- **Why a critic and citations:** the PRD's unsupported-claim rate target (≤5%) only works if every claim names evidence.
- **Why ground truth is kept from the copilot:** the injector's truth table is what scores accuracy. If the copilot can read it, the score is meaningless.
- **Why actions need approval:** the copilot's ServiceAccount can delete pods and scale deployments, so a wrong diagnosis must not act on its own.
- **Why the Redis alert was removed:** an alert that never fires looks like coverage and isn't. The honest answer is that Redis failures aren't instrumented yet.
- **Why per-service admin ports matter:** a scrape config built from a wrong assumption (6060 everywhere) would silently miss two services.
- **Trade-off on pod-level scraping:** avoids exposing pprof on NodePort; costs more configuration.
- **Where the process deviated from the kickoff:** agent types didn't exist, so the "kitchen" model tiers in CLAUDE.md were not used as written.
