// Assignment solver (Week 3) under Catch2. Ported from the hand-rolled
// assignment_test.cpp main(), keeping every check that file made and adding
// the structural invariants as named cases.
//
// The reason this suite exists at all: a matching bug is invisible by
// inspection. A suboptimal assignment is still a legal assignment -- every
// driver used at most once, a plausible total. Only an independent optimum
// can tell the two apart.

#include "catch.hpp"

#include <random>
#include <set>

#include "assignment.h"
#include "oracles.h"

namespace {

// Structural legality, independent of cost: in-range indices, no driver
// double-booked, matchedCount consistent with the array.
void requireLegal(const Assignment& a, int n, int m) {
    REQUIRE(static_cast<int>(a.riderToDriver.size()) == n);
    std::set<int> usedDrivers;
    int matched = 0;
    for (int i = 0; i < n; ++i) {
        const int d = a.riderToDriver[i];
        if (d == -1) continue;
        ++matched;
        REQUIRE(d >= 0);
        REQUIRE(d < m);
        REQUIRE(usedDrivers.insert(d).second);   // the no-double-booking invariant
    }
    REQUIRE(matched == a.matchedCount);
}

} // namespace

TEST_CASE("empty inputs produce an empty assignment", "[assignment][edge]") {
    const Assignment a = solveAssignment(0, 0, {});
    REQUIRE(a.matchedCount == 0);
    REQUIRE(a.totalCost == 0);
    REQUIRE(a.riderToDriver.empty());
}

TEST_CASE("riders with no drivers all come back unmatched", "[assignment][edge]") {
    const Assignment a = solveAssignment(3, 0, {});
    REQUIRE(a.matchedCount == 0);
    REQUIRE(a.riderToDriver == std::vector<int>{-1, -1, -1});
    REQUIRE(a.totalCost == 0);
}

TEST_CASE("drivers with no riders is a no-op", "[assignment][edge]") {
    const Assignment a = solveAssignment(0, 5, {});
    REQUIRE(a.matchedCount == 0);
    REQUIRE(a.riderToDriver.empty());
}

TEST_CASE("the 1x1 case", "[assignment][edge]") {
    const Assignment a = solveAssignment(1, 1, {{0, 0, 7}});
    REQUIRE(a.matchedCount == 1);
    REQUIRE(a.totalCost == 7);
    REQUIRE(a.riderToDriver[0] == 0);
}

TEST_CASE("zero-cost edges are matched, not ignored", "[assignment][edge]") {
    // A driver already at the rider's door costs 0. Any code that treats 0 as
    // "no edge" -- a very easy sentinel mistake -- fails here.
    const Assignment a = solveAssignment(2, 2, {{0, 0, 0}, {0, 1, 5}, {1, 0, 5}, {1, 1, 0}});
    REQUIRE(a.matchedCount == 2);
    REQUIRE(a.totalCost == 0);
    REQUIRE(a.riderToDriver[0] == 0);
    REQUIRE(a.riderToDriver[1] == 1);
}

TEST_CASE("more riders than drivers: exactly M matched, the rest reported as -1",
          "[assignment][edge][overflow]") {
    const std::vector<std::vector<long long>> cost = {{1, 9}, {9, 1}, {2, 2}};
    const Assignment a = solveAssignment(3, 2, oracle::denseEdges(cost));

    requireLegal(a, 3, 2);
    REQUIRE(a.matchedCount == 2);
    REQUIRE(a.totalCost == oracle::minCostMaxMatching(cost));

    const int unmatched = static_cast<int>(
        std::count(a.riderToDriver.begin(), a.riderToDriver.end(), -1));
    REQUIRE(unmatched == 1);
}

TEST_CASE("more drivers than riders: every rider matched, spare drivers idle",
          "[assignment][edge][overflow]") {
    const std::vector<std::vector<long long>> cost = {{5, 1, 8}};
    const Assignment a = solveAssignment(1, 3, oracle::denseEdges(cost));
    REQUIRE(a.matchedCount == 1);
    REQUIRE(a.riderToDriver[0] == 1);        // the cheapest driver
    REQUIRE(a.totalCost == 1);
}

TEST_CASE("a rider with no candidate edges stays unmatched", "[assignment][edge][overflow]") {
    // This is the sparse path's rejection mechanism: rider 1 has no edges even
    // though driver 1 is free. It must not be force-matched to a driver it was
    // never a candidate for.
    const Assignment a = solveAssignment(2, 2, {{0, 0, 3}, {0, 1, 4}});
    REQUIRE(a.matchedCount == 1);
    REQUIRE(a.riderToDriver[1] == -1);
}

TEST_CASE("the greedy trap: nearest-first is 25x worse than optimal", "[assignment]") {
    // rider0: d0=1, d1=2 | rider1: d0=2, d1=100
    // Greedy in rider order takes d0 for rider0 (its nearest), stranding
    // rider1 with d1 at 100, for 101. The optimum swaps them, for 4.
    const std::vector<std::vector<long long>> cost = {{1, 2}, {2, 100}};
    const Assignment a = solveAssignment(2, 2, oracle::denseEdges(cost));
    REQUIRE(a.totalCost == 4);
    REQUIRE(a.totalCost == oracle::minCostMaxMatching(cost));
}

TEST_CASE("optimal on 3000 random matrices, checked against brute force", "[assignment]") {
    std::mt19937 rng(12345);   // fixed seed: a failure is reproducible
    std::uniform_int_distribution<int> dimDist(0, 8);
    std::uniform_int_distribution<long long> costDist(0, 50);

    for (int trial = 0; trial < 3000; ++trial) {
        const int n = dimDist(rng);
        const int m = dimDist(rng);
        std::vector<std::vector<long long>> cost(n, std::vector<long long>(m));
        for (int i = 0; i < n; ++i) {
            for (int j = 0; j < m; ++j) cost[i][j] = costDist(rng);
        }

        const Assignment got = solveAssignment(n, m, oracle::denseEdges(cost));

        requireLegal(got, n, m);

        // On a complete matrix, max matching is always min(N, M).
        REQUIRE(got.matchedCount == std::min(n, m));

        // The optimality claim itself.
        const long long optimum = (std::min(n, m) == 0) ? 0 : oracle::minCostMaxMatching(cost);
        REQUIRE(got.totalCost == optimum);

        // And the reported total must be what the returned pairs actually cost --
        // a solver can find the right answer and report the wrong number.
        long long recomputed = 0;
        for (int i = 0; i < n; ++i) {
            if (got.riderToDriver[i] != -1) recomputed += cost[i][got.riderToDriver[i]];
        }
        REQUIRE(recomputed == got.totalCost);
    }
}

TEST_CASE("optimal on random SPARSE edge sets", "[assignment]") {
    // The dense random test never produces a rider with no options. Sparse
    // inputs do, and they are what the k-nearest cost matrix actually generates.
    std::mt19937 rng(6789);
    std::uniform_int_distribution<int> dimDist(1, 7);
    std::uniform_int_distribution<long long> costDist(1, 40);
    std::bernoulli_distribution keepEdge(0.5);

    constexpr long long kNoEdge = 1'000'000;   // stands in for "impossible" in the oracle

    for (int trial = 0; trial < 1500; ++trial) {
        const int n = dimDist(rng);
        const int m = dimDist(rng);

        std::vector<std::vector<long long>> full(n, std::vector<long long>(m, kNoEdge));
        std::vector<MatchEdge> edges;
        for (int i = 0; i < n; ++i) {
            for (int j = 0; j < m; ++j) {
                if (!keepEdge(rng)) continue;
                const long long c = costDist(rng);
                full[i][j] = c;
                edges.push_back({i, j, c});
            }
        }

        const Assignment got = solveAssignment(n, m, edges);
        requireLegal(got, n, m);

        // Every matched pair must be an edge that was actually offered.
        for (int i = 0; i < n; ++i) {
            if (got.riderToDriver[i] != -1) REQUIRE(full[i][got.riderToDriver[i]] != kNoEdge);
        }

        // The oracle prices missing edges at kNoEdge, which is larger than any
        // real matching can accumulate. So its optimum either uses no forbidden
        // edge (and both agree), or the solver matched fewer riders on purpose.
        const long long oracleCost = oracle::minCostMaxMatching(full);
        if (oracleCost < kNoEdge) {
            REQUIRE(got.matchedCount == std::min(n, m));
            REQUIRE(got.totalCost == oracleCost);
        }
    }
}

TEST_CASE("both shortest-path engines return the same optimum", "[assignment][engine]") {
    // solveAssignment() now picks between SPFA and Dijkstra-with-potentials by
    // edge density (mcmf.cpp). That is a PERFORMANCE decision, and it is only
    // safe if the two engines are indistinguishable in their answers. If this
    // ever fails, the auto-selection is silently changing results depending on
    // how dense the batch happened to be -- the worst kind of bug, because it
    // would only appear at certain loads.
    std::mt19937 rng(31415);
    std::uniform_int_distribution<int> dimDist(1, 9);
    std::uniform_int_distribution<long long> costDist(0, 200);
    std::bernoulli_distribution keepEdge(0.6);

    for (int trial = 0; trial < 1200; ++trial) {
        const int n = dimDist(rng);
        const int m = dimDist(rng);

        std::vector<MatchEdge> edges;
        for (int i = 0; i < n; ++i) {
            for (int j = 0; j < m; ++j) {
                if (keepEdge(rng)) edges.push_back({i, j, costDist(rng)});
            }
        }

        const Assignment spfa = solveAssignment(n, m, edges, ShortestPathEngine::Spfa);
        const Assignment dijkstra = solveAssignment(n, m, edges, ShortestPathEngine::Dijkstra);
        const Assignment automatic = solveAssignment(n, m, edges);

        // The COST and the MATCH COUNT must agree exactly. The specific pairing
        // need not: when several assignments tie at the optimum, either engine
        // may legitimately return a different one.
        REQUIRE(spfa.totalCost == dijkstra.totalCost);
        REQUIRE(spfa.matchedCount == dijkstra.matchedCount);
        REQUIRE(automatic.totalCost == spfa.totalCost);
        REQUIRE(automatic.matchedCount == spfa.matchedCount);
    }
}

TEST_CASE("both engines agree on a dense matrix large enough to trip the threshold",
          "[assignment][engine]") {
    // The random test above builds tiny graphs that always land on the SPFA
    // side of the density threshold. This one is dense enough to select
    // Dijkstra automatically, so the Auto path is genuinely exercised on both
    // sides of the branch.
    constexpr int kSize = 60;   // 60 edges per node, over the 40 threshold
    std::mt19937 rng(2718);
    std::uniform_int_distribution<long long> costDist(1, 5000);

    std::vector<MatchEdge> edges;
    for (int i = 0; i < kSize; ++i) {
        for (int j = 0; j < kSize; ++j) edges.push_back({i, j, costDist(rng)});
    }

    const Assignment spfa = solveAssignment(kSize, kSize, edges, ShortestPathEngine::Spfa);
    const Assignment dijkstra = solveAssignment(kSize, kSize, edges, ShortestPathEngine::Dijkstra);
    const Assignment automatic = solveAssignment(kSize, kSize, edges);

    REQUIRE(spfa.matchedCount == kSize);
    REQUIRE(dijkstra.totalCost == spfa.totalCost);
    REQUIRE(automatic.totalCost == spfa.totalCost);
}

TEST_CASE("duplicate edges between the same pair do not double-book", "[assignment][edge]") {
    // Two builders can legitimately emit the same pair (dense plus a hotspot
    // override). The solver must treat it as one option at the cheaper price,
    // not as two units of capacity.
    const Assignment a = solveAssignment(1, 1, {{0, 0, 10}, {0, 0, 3}});
    REQUIRE(a.matchedCount == 1);
    REQUIRE(a.totalCost == 3);
}
