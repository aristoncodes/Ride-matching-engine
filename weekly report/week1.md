# Week 1 — Repository Setup & Coordinate Generator

**Date:** Jul 9, 2026 · **Phase:** 1 (Core Algorithmic Engine, C++) · **Status:** ✅ Complete

## What this week was about
Every later stage — the quadtree, the matcher, the load tests — needs data to run on:
thousands of rider and driver positions. Week 1 builds the tool that produces that data,
in a way that is **reproducible** (same input → identical output) and **machine-readable**
(pipeable straight into the next program).

## What was built
- **`matching_engine/src/generator.cpp`** — a standalone CLI generator of random rider and
  driver coordinates on a square grid. No dependency on any other part of the engine.

## What it does
| Flag | Meaning | Default |
|------|---------|---------|
| `-N, --riders <int>`  | number of riders  | 10000 |
| `-M, --drivers <int>` | number of drivers | 10000 |
| `-g, --grid <float>`  | grid side length  | 100000 |
| `-s, --seed <uint>`   | RNG seed (reproducible) | random |
| `-f, --format <fmt>`  | `text` / `csv` / `json` | text |
| `-h, --help`          | usage | — |

- **Reproducibility:** a fixed `--seed` gives byte-identical output every run. A run *without*
  a seed picks a random one and **prints it to stderr**, so even a "random" run can be
  reproduced later.
- **Clean data channel:** coordinates go to **stdout**; run parameters/seed go to **stderr**.
  That separation is what makes `./generator | ./next_program` work — stdout stays pure data.
- **Unambiguous IDs:** riders are `R0, R1, …`, drivers are `D0, D1, …`, so a combined set
  never has an ID collision.
- The same seed yields identical coordinates across all three output formats (the number and
  order of RNG draws is fixed at 2 per entity).

## Definition of done — all met
- ✅ Everything parameterized via CLI (no recompiling to change dataset size)
- ✅ Reproducibility mode via `--seed`
- ✅ Machine-readable output (csv/json, JSON verified to parse)
- ✅ Type-prefixed, collision-free IDs
- ✅ Pipeable stdout

## How to run
```bash
cd matching_engine/build
cmake .. && make generator
./generator -N 5 -M 3 -s 42 -f csv
```

## What was intentionally NOT done
The interim matching demo was kept **out** of the generator (moved to `match_demo.cpp`) so the
generator stays single-purpose: it generates data, nothing else.

→ Learnings for this week: [learnings1.md](learnings1.md)
