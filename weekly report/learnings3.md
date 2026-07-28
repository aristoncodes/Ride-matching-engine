# Learnings — Week 3 (Optimal Matching / MCMF)

The flagship algorithms week. Concepts and interview-ready takeaways.
Report: [week3.md](week3.md)

## 1. The Assignment Problem (recognize it by shape)
"Assign N things to M things, each used once, minimize total cost." Riders↔drivers, tasks↔workers,
orders↔couriers, ads↔slots — all the same problem. Standard solvers:
- **Hungarian (Kuhn–Munkres):** O(n³), purpose-built for square assignment. The textbook answer.
- **Min-Cost Max-Flow:** more general — model as flow, any MCMF solves it; handles N≠M for free.
- **Interview soundbite:** "Assignment is a special case of min-cost flow; modeling it as flow
  means rectangular inputs need no dummy padding."

## 2. Why greedy is wrong — memorize this example
Costs `{{1, 2}, {2, 100}}` (2 riders, 2 drivers).
- **Greedy** (rider 0 grabs its favorite, driver 0, cost 1) forces rider 1 → driver 1 = **101**.
- **Optimal**: rider 0 → driver 1 (2), rider 1 → driver 0 (2) = **4**.
One rider's locally-best choice wrecks another's. Greedy is locally optimal, globally terrible.
My demo showed 30–78% higher total distance for greedy on real batches.

## 3. Modeling assignment as a flow network (draw this)
```
source ──cap1,cost0──▶ rider ──cap1,cost=dist──▶ driver ──cap1,cost0──▶ sink
```
- **Unit capacity on each driver→sink edge** is what physically forbids matching a driver twice.
- Max-flow = min(N,M) matched. Min-cost among all max-flows = cheapest total.
- Understanding *why the capacity encodes the constraint* is the real skill.

## 4. How Min-Cost Max-Flow finds the optimum: Successive Shortest Paths
Repeat: find the **cheapest** source→sink path in the **residual graph**, push 1 unit, until no
path remains.
- **Residual / reverse edges carry negated cost** → pushing flow "backward" *refunds* cost, letting
  a later step *undo* an earlier greedy choice. That undo is why SSP reaches the true optimum and
  greedy can't.
- Shortest path here uses **SPFA** (queue-based Bellman-Ford), because residual edges are negative
  and plain Dijkstra can't handle negatives. There are no negative *cycles*, so it terminates.

## 5. Complexity — the follow-up you WILL be asked
- SSP MCMF: **O(F · V · E)**, F = min(N,M) augmentations × O(V·E) SPFA each.
- This bites: dense N=M=1000 solve ≈ 10 s in my demo. Two fixes:
  1. **Sparse candidates** — cut E from N·M to N·k using the quadtree's `kNearest`.
  2. **Dijkstra + Johnson's potentials** — reweight residual edges non-negative so Dijkstra works
     → O(F · E · log V). (Scheduled Week 5.)

## 6. The sparse tradeoff + a statistics trap to never fall for
Sparse (each rider → k=8 nearest drivers) cut solve time ~55×. BUT its *average distance per
match* looked **lower** than provably-optimal dense. Sparse did **not** beat optimal — impossible.
It **dropped the hardest-to-match riders** (their k nearest drivers were all taken), so it's only
averaging the easy ones. **Survivorship bias.**
- **Life/interview lesson:** when a metric improves after you drop data, ask *"did it get better,
  or did I stop measuring the hard cases?"* Comparing totals/averages across different denominators
  is how you lie with statistics, accidentally or not.

## 7. Integer costs, not floats (a correctness detail interviewers respect)
The solver uses `long long` costs (distance × 1000, rounded). Comparing shortest-path costs with
floating-point risks `a < b` being nondeterministic across platforms/runs. Flow and shortest-path
algorithms are stated over integers for exactly this reason.

## 8. Proving optimality — the discipline that caught nothing *because* it was right
Graph-matching bugs are **silent**: a wrong answer still looks like a valid assignment, just not the
cheapest. The only defense is an **independent oracle**: brute-force the true optimum for small N
and assert equality. 3000 random cases + edge cases = 29,683 assertions, 0 failures.
- **Interview angle:** *"How do you test an optimization algorithm?"* → brute-force oracle on small
  inputs + structural invariants (valid matching) on large ones + property tests.

## Quick self-test
- Why does modeling assignment as flow remove the need to pad N≠M matrices?
- What do the negated-cost reverse edges buy you, concretely?
- Why SPFA instead of Dijkstra inside this MCMF? What would let you switch back?
- Sparse showed a lower average distance than optimal — is it better? Why/why not?
- How would you unit-test a solver whose output you can't eyeball for correctness?
