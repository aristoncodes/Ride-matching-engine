// Week 3 correctness anchor: prove solveAssignment() returns a genuinely
// optimal assignment, by checking it against a brute-force oracle on small
// inputs, plus a battery of hand-picked edge cases.
//
// Graph-matching bugs are silent: a wrong assignment still looks like a valid
// assignment (every driver used once, plausible total), it's just not the
// cheapest. The only way to catch that is to compute the true optimum an
// independent way and compare. For N, M <= 8 we can afford to brute-force it.

#include "assignment.h"

#include <vector>
#include <random>
#include <algorithm>
#include <iostream>
#include <limits>
#include <string>
#include <functional>

namespace {

int g_pass = 0;
int g_fail = 0;

void check(bool cond, const std::string& what) {
    if (cond) {
        ++g_pass;
    } else {
        ++g_fail;
        std::cout << "  FAIL: " << what << "\n";
    }
}

// ---- Brute-force oracle -------------------------------------------------
// Return the minimum total cost of a MAXIMUM matching on a dense complete
// N x M cost matrix (every rider connects to every driver). Because the
// matrix is complete, max matching size is always min(N, M).
//
// Recurse over riders. Each rider may take any unused driver, or stay
// unmatched; at the end we require exactly min(N, M) matches (max matching)
// and track the cheapest way to achieve it.
long long bruteForceMinCost(const std::vector<std::vector<long long>>& cost) {
    int n = static_cast<int>(cost.size());
    int m = n == 0 ? 0 : static_cast<int>(cost[0].size());
    int target = std::min(n, m);
    if (target == 0) return 0;

    std::vector<char> usedDriver(m, 0);
    long long best = std::numeric_limits<long long>::max();

    // rider index, drivers already used, cost so far, matches so far
    std::function<void(int, long long, int)> rec =
        [&](int rider, long long curCost, int matched) {
            // Prune: even matching every remaining rider can't reach target.
            if (matched + (n - rider) < target) return;
            if (curCost >= best) return;
            if (rider == n) {
                if (matched == target) best = std::min(best, curCost);
                return;
            }
            // Option 1: leave this rider unmatched.
            rec(rider + 1, curCost, matched);
            // Option 2: match to some free driver.
            for (int j = 0; j < m; ++j) {
                if (!usedDriver[j]) {
                    usedDriver[j] = 1;
                    rec(rider + 1, curCost + cost[rider][j], matched + 1);
                    usedDriver[j] = 0;
                }
            }
        };

    rec(0, 0, 0);
    return best;
}

// Turn a dense matrix into the edge list solveAssignment() consumes.
std::vector<MatchEdge> denseEdges(const std::vector<std::vector<long long>>& cost) {
    std::vector<MatchEdge> edges;
    for (int i = 0; i < static_cast<int>(cost.size()); ++i) {
        for (int j = 0; j < static_cast<int>(cost[i].size()); ++j) {
            edges.push_back({i, j, cost[i][j]});
        }
    }
    return edges;
}

// Validate that an Assignment is structurally legal (independent of cost):
// every matched driver is distinct, indices in range, totals self-consistent.
void checkStructure(const Assignment& a, int n, int m, const std::string& tag) {
    check(static_cast<int>(a.riderToDriver.size()) == n, tag + ": riderToDriver size");
    std::vector<char> seenDriver(m, 0);
    int matched = 0;
    for (int i = 0; i < n; ++i) {
        int d = a.riderToDriver[i];
        if (d == -1) continue;
        ++matched;
        check(d >= 0 && d < m, tag + ": driver index in range");
        check(!seenDriver[d], tag + ": no driver matched twice");
        if (d >= 0 && d < m) seenDriver[d] = 1;
    }
    check(matched == a.matchedCount, tag + ": matchedCount consistent");
}

// ---- Randomized optimality test ----------------------------------------
void randomizedTests() {
    std::mt19937 rng(12345); // fixed seed => reproducible
    std::uniform_int_distribution<int> dimDist(0, 8);
    std::uniform_int_distribution<long long> costDist(0, 50);

    int cases = 0;
    for (int t = 0; t < 3000; ++t) {
        int n = dimDist(rng);
        int m = dimDist(rng);
        std::vector<std::vector<long long>> cost(n, std::vector<long long>(m));
        for (int i = 0; i < n; ++i)
            for (int j = 0; j < m; ++j)
                cost[i][j] = costDist(rng);

        Assignment got = solveAssignment(n, m, denseEdges(cost));
        checkStructure(got, n, m, "rand");

        // Solver must match as many as possible on a complete matrix.
        check(got.matchedCount == std::min(n, m), "rand: matchedCount == min(N,M)");

        // And the total cost must equal the brute-force optimum.
        long long oracle = (std::min(n, m) == 0) ? 0 : bruteForceMinCost(cost);
        check(got.totalCost == oracle, "rand: totalCost == brute-force optimum");

        // Independently recompute cost from the returned assignment.
        long long recomputed = 0;
        for (int i = 0; i < n; ++i)
            if (got.riderToDriver[i] != -1)
                recomputed += cost[i][got.riderToDriver[i]];
        check(recomputed == got.totalCost, "rand: reported cost == recomputed");

        ++cases;
    }
    std::cout << "randomized: ran " << cases << " cases\n";
}

// ---- Explicit edge cases -----------------------------------------------
void edgeCaseTests() {
    // Empty on both sides.
    {
        Assignment a = solveAssignment(0, 0, {});
        check(a.matchedCount == 0 && a.totalCost == 0, "empty: nothing matched");
    }
    // Riders but no drivers -> all unmatched (overflow policy).
    {
        Assignment a = solveAssignment(3, 0, {});
        check(a.matchedCount == 0, "no drivers: none matched");
        check(a.riderToDriver == std::vector<int>({-1, -1, -1}), "no drivers: all -1");
    }
    // 1x1.
    {
        Assignment a = solveAssignment(1, 1, {{0, 0, 7}});
        check(a.matchedCount == 1 && a.totalCost == 7 && a.riderToDriver[0] == 0, "1x1");
    }
    // N > M: overflow. 3 riders, 2 drivers, distinct costs.
    {
        std::vector<std::vector<long long>> cost = {{1, 9}, {9, 1}, {2, 2}};
        Assignment a = solveAssignment(3, 2, denseEdges(cost));
        checkStructure(a, 3, 2, "N>M");
        check(a.matchedCount == 2, "N>M: exactly M matched");
        check(a.totalCost == bruteForceMinCost(cost), "N>M: optimal");
        int unmatched = 0;
        for (int d : a.riderToDriver) if (d == -1) ++unmatched;
        check(unmatched == 1, "N>M: exactly one rider left over");
    }
    // M > N: symmetric overflow (some drivers unused).
    {
        std::vector<std::vector<long long>> cost = {{5, 1, 8}};
        Assignment a = solveAssignment(1, 3, denseEdges(cost));
        check(a.matchedCount == 1, "M>N: exactly N matched");
        check(a.riderToDriver[0] == 1, "M>N: picks cheapest driver");
    }
    // Sparse: a rider with NO candidate edges must stay unmatched even though
    // drivers are free (this is how rejection/overflow works in the sparse path).
    {
        // 2 riders, 2 drivers, but rider 1 has no edges at all.
        std::vector<MatchEdge> edges = {{0, 0, 3}, {0, 1, 4}};
        Assignment a = solveAssignment(2, 2, edges);
        check(a.matchedCount == 1, "sparse-isolated: only reachable rider matched");
        check(a.riderToDriver[1] == -1, "sparse-isolated: edgeless rider unmatched");
    }
    // Greedy trap: the classic case where matching each rider to its own
    // nearest driver is globally suboptimal. Rider 0 and rider 1 both prefer
    // driver 0; the optimal solution gives driver 0 to whoever needs it most.
    {
        // rider0: d0=1, d1=100 ; rider1: d0=2, d1=3
        // Greedy (rider0 first) takes d0 for rider0 (1), forcing rider1->d1 (3) = 4.
        // Optimal: rider0->d1? No: rider0->d0=1, rider1->d1=3 total 4 vs
        //          rider0->d1=100... clearly 4 is optimal here; construct a real trap:
        std::vector<std::vector<long long>> cost = {{1, 2}, {2, 100}};
        // Greedy by rider: rider0->d0(1); rider1->d1(100) = 101.
        // Optimal: rider0->d1(2); rider1->d0(2) = 4.
        Assignment a = solveAssignment(2, 2, denseEdges(cost));
        check(a.totalCost == 4, "greedy-trap: optimal beats greedy (4, not 101)");
        check(a.totalCost == bruteForceMinCost(cost), "greedy-trap: matches oracle");
    }
}

} // namespace

int main() {
    std::cout << "== assignment solver correctness ==\n";
    edgeCaseTests();
    randomizedTests();

    std::cout << "----\n"
              << "passed: " << g_pass << "  failed: " << g_fail << "\n";
    return g_fail == 0 ? 0 : 1;
}
