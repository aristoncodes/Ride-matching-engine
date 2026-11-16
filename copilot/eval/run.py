"""Evaluation harness (ticket 3.1). A script, not part of the service package.

  uv run python eval/run.py --runs-per-fault 5 --no-fault-runs 5 --critic on
  uv run python eval/run.py --dry-run --runs-per-fault 1 --no-fault-runs 1 --critic on
  uv run python eval/run.py --score eval/results/<a>.jsonl [<b>.jsonl ...]

For each run: start k8s/fault-inject.sh (one run), wait for the alert window, run the copilot
investigation with the real components, record the result. The diagnosis is never invented.
Ground truth (incidents_truth) is read only by the SCORER, with EVAL_TRUTH_DSN. Copilot code
never reads it, and the investigation trigger does not name the fault.

Results go to eval/results/<timestamp>.jsonl, one row per run. No prompt text is written.
Windows: each row carries window_start/window_end so the scorer can match it to its truth row.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import subprocess
import sys
import time
from datetime import UTC, datetime
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
sys.path.insert(0, str(HERE))
import score as scoring  # noqa: E402

FAULTS = tuple(scoring.EXPECTED_COMPONENT)
INJECTOR = ROOT / "k8s" / "fault-inject.sh"
RESULTS_DIR = HERE / "results"
INVESTIGATE_TIMEOUT_S = 60.0
TRIGGER = {
    "question": (
        "Something may be wrong with the ride-matching system right now. "
        "Diagnose any active incident from the last 10 minutes."
    )
}
# The injector gets only what it needs, never the model or database credentials of the copilot.
INJECTOR_ENV = (
    "PATH", "HOME", "USER", "TMPDIR", "KUBECONFIG", "GOPATH", "GOCACHE", "GOFLAGS",
    "PGHOST", "PGPORT", "PGUSER", "PGDATABASE", "PGPASSWORD", "PGPASSFILE", "PGSSLMODE",
    "FAULT_HOLD_SECONDS", "SETTLE_SECONDS", "BURST_RATE", "BURST_SECONDS", "BURST_DRIVERS",
    "SLOW_CPU_LIMIT",
)  # fmt: skip
REQUIRED = {
    "EVAL_TRUTH_DSN": "Postgres DSN for the incidents_truth reader (owner or fault_injector role)",
    "COPILOT_RO_DSN": "DSN for the copilot_ro role (agent reads)",
    "GROQ_API_KEY": "Groq API key for the model",
    "PGHOST": "libpq host for the fault injector's ground-truth writes",
    "PGUSER": "libpq user for the fault injector",
    "PGDATABASE": "libpq database for the fault injector",
}


def missing_config() -> list[str]:
    return [f"  {k}: {why}" for k, why in REQUIRED.items() if not os.environ.get(k, "").strip()]


def plan(runs_per_fault: int, no_fault_runs: int) -> list[str]:
    return [f for f in FAULTS for _ in range(runs_per_fault)] + [scoring.NO_FAULT] * no_fault_runs


def _now() -> datetime:
    return datetime.now(UTC)


def start_injector(fault: str) -> subprocess.Popen:
    args = [str(INJECTOR), "--runs", "1"] + (
        ["--none"] if fault == scoring.NO_FAULT else ["--fault", fault]
    )
    env = {k: os.environ[k] for k in INJECTOR_ENV if k in os.environ}
    return subprocess.Popen(  # noqa: S603
        args, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    )


async def one_run(fault: str, critic: str, comps, window_s: float, settle_s: float) -> dict:
    from copilot.agents.coordinator import investigate
    from copilot.agents.tools.runbook_tools import search_runbook
    from copilot.cost import cost_usd

    window_start = _now()
    proc = start_injector(fault)
    await asyncio.sleep(window_s)  # the alert window: let the fault show up in the metrics
    diagnosis = result = None
    t0 = time.monotonic()
    err = None
    try:
        result = await asyncio.wait_for(
            investigate(
                TRIGGER,
                model=comps.guarded_model,
                pool_ro=comps.pool_ro,
                prom_client=comps.prom_client,
                prom_base_url=comps.prom_base_url,
                embedder=comps.embedder,
                critic=critic == "on",
            ),
            timeout=INVESTIGATE_TIMEOUT_S,
        )
        diagnosis = result.diagnosis
    except Exception as exc:  # noqa: BLE001 - record the failure class, keep the run
        err = type(exc).__name__
    latency = time.monotonic() - t0

    top5: list[str] = []
    try:
        query = f"{diagnosis.summary} {diagnosis.root_cause}" if diagnosis else TRIGGER["question"]
        hits = await search_runbook(comps.pool_ro, comps.embedder, query, 5)
        top5 = [h["anchor"] for h in hits if h.get("anchor")]
    except Exception as exc:  # noqa: BLE001
        err = err or "retrieval:" + type(exc).__name__

    tin = result.tokens_in if result else 0
    tout = result.tokens_out if result else 0
    model = result.model if result else ""
    try:
        injector_rc = await asyncio.to_thread(proc.wait, 600)
    except subprocess.TimeoutExpired:
        proc.kill()
        injector_rc = -9
    window_end = _now()
    await asyncio.sleep(settle_s)
    return {
        "fault": fault,
        "critic": critic,
        "window_start": window_start.isoformat(),
        "window_end": window_end.isoformat(),
        "injector_rc": injector_rc,
        "diagnosis": diagnosis.model_dump() if diagnosis else None,
        "needs_human": bool(diagnosis and diagnosis.needs_human),
        "incident_opened": bool(
            diagnosis and not diagnosis.needs_human and diagnosis.runbook_anchor
        ),
        "critic_flag": bool(result.critic_enabled) if result else critic == "on",
        "retrieval_top5": top5,
        "latency_s": round(latency, 3),
        "tokens_in": tin,
        "tokens_out": tout,
        "model": model,
        "cost_usd": round(cost_usd(model, tin, tout), 6) if model else 0.0,
        "error": err,
    }


async def fetch_truth(dsn: str, since: datetime) -> list[dict]:
    import asyncpg

    conn = await asyncpg.connect(dsn)
    try:
        rows = await conn.fetch(
            "SELECT run_id, fault, target, start_time, expected_runbook "
            "FROM incidents_truth WHERE start_time >= $1",
            since,
        )
    finally:
        await conn.close()
    return [dict(r) for r in rows]


def load_results(paths: list[str]) -> list[dict]:
    rows = []
    for p in paths:
        for line in Path(p).read_text(encoding="utf-8").splitlines():
            if line.strip():
                rows.append(json.loads(line))
    return rows


async def report(rows: list[dict]) -> str:
    since = min(datetime.fromisoformat(r["window_start"]) for r in rows) - scoring.MATCH_SLOP
    truths = await fetch_truth(os.environ["EVAL_TRUTH_DSN"], since)
    return scoring.render_table(scoring.score_by_critic(rows, truths))


async def run_all(args: argparse.Namespace) -> int:
    from copilot.api.components import build_agent_components

    comps = await build_agent_components()
    RESULTS_DIR.mkdir(exist_ok=True)
    out = RESULTS_DIR / (_now().strftime("%Y%m%dT%H%M%SZ") + ".jsonl")
    rows = []
    with out.open("w", encoding="utf-8") as fh:
        for i, fault in enumerate(plan(args.runs_per_fault, args.no_fault_runs), 1):
            row = await one_run(fault, args.critic, comps, args.alert_window, args.settle)
            row["run_index"] = i
            rows.append(row)
            fh.write(json.dumps(row, default=str) + "\n")
            fh.flush()
            print(f"run {i} fault={fault} done")
    print(f"results: {out}")
    print(await report(rows))
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Copilot evaluation harness")
    ap.add_argument("--runs-per-fault", type=int, default=5)
    ap.add_argument("--no-fault-runs", type=int, default=5)
    ap.add_argument("--critic", choices=("on", "off"), required=False, default="on")
    ap.add_argument("--alert-window", type=float, default=45.0, help="seconds before diagnosing")
    ap.add_argument("--settle", type=float, default=60.0, help="seconds between runs")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--score", nargs="+", metavar="JSONL", help="score existing results files")
    args = ap.parse_args(argv)
    if args.runs_per_fault < 1 or args.no_fault_runs < 0:
        ap.error("--runs-per-fault must be >= 1 and --no-fault-runs >= 0")

    need = ["EVAL_TRUTH_DSN"] if args.score else None
    missing = (
        missing_config()
        if need is None
        else [m for m in missing_config() if m.strip().startswith(need[0])]
    )
    if missing:
        print("eval: missing required configuration, nothing was run:", file=sys.stderr)
        print("\n".join(missing), file=sys.stderr)
        return 2
    if args.score:
        print(asyncio.run(report(load_results(args.score))))
        return 0
    runs = plan(args.runs_per_fault, args.no_fault_runs)
    if args.dry_run:
        print(
            f"dry run: {len(runs)} planned runs, critic={args.critic}; cluster and model untouched"
        )
        for i, f in enumerate(runs, 1):
            print(f"  {i:3d}  {f}")
        return 0
    return asyncio.run(run_all(args))


if __name__ == "__main__":
    sys.exit(main())
