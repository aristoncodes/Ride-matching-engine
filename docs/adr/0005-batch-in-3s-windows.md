# ADR-0005: Batch ride requests in 3-second windows

## Status
🟡 Proposed — follows directly from the "Batching over Streaming" tenet; to be validated when the batcher is built (Week 12).

## Context
The C++ solver produces a globally optimal assignment over a *set* of riders and drivers. Matching one rider at a time (pure streaming) would (a) forfeit global optimality and (b) incur a cross-language gRPC call per request — the exact overhead flagged as our top risk. We need to trade a small, bounded latency for far better matches and far fewer cross-language calls.

## Options considered
1. **Pure streaming (match on arrival)** — lowest per-request latency, but no global optimization and maximum cross-language call volume.
2. **Fixed-size batching (match every K requests)** — good amortization, but latency is unbounded when traffic is light (a request could wait a long time for the Kth to arrive).
3. **Time-windowed batching (every 3 seconds)** — bounds worst-case latency to the window, groups enough riders/drivers for meaningful optimization, and amortizes gRPC overhead. This is the "Batching over Streaming for Math" tenet.
4. **Hybrid dual-trigger (window OR max batch size)** — the refinement: flush on 3s *or* when a batch grows large, protecting both latency and memory under spikes.

## Decision
**Time-windowed batching with a default 3-second window**, implemented with a **dual-trigger flush** (window elapsed OR max batch size reached). The window is configurable.

## Consequences
- ➕ Enables global optimization (the whole point of the C++ engine).
- ➕ Amortizes the cross-language call overhead — the mitigation for the serialization risk.
- ➕ Bounded worst-case matching latency (≈ the window).
- ➖ Adds up to ~3s of intentional latency before a match — acceptable per the B2B use case (see PRD assumptions), but must be stated to clients.
- ➖ The batcher becomes a critical component needing its own metrics (batch size, latency, match rate) and backpressure handling under overload.
