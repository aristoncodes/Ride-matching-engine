// Week 3 showcase: the real, globally-optimal matcher, compared against the
// greedy baseline it replaces. Demonstrates three things the checkpoint asks
// for on ONE dataset:
//   1. Optimal (MCMF) total cost <= greedy total cost — always.
//   2. Sparse (k-nearest candidates) is far cheaper to build and solve than
//      dense, at a small, measurable cost-quality gap.
//   3. Overflow: when riders > drivers, the leftover riders come back -1.
//
// This is a demo/benchmark, not a test — assignment_test.cpp is the correctness
// anchor. Numbers here are illustrative and reproducible (fixed seed).

#include <iostream>
#include <vector>
#include <random>
#include <chrono>
#include <cmath>
#include <iomanip>
#include <limits>

#include "quadtree.h"
#include "assignment.h"
#include "cost_matrix.h"

namespace {

double realDistance(const Point& a, const Point& b) {
    double dx = a.x - b.x, dy = a.y - b.y;
    return std::sqrt(dx * dx + dy * dy);
}

// The greedy baseline this week replaces: each rider grabs its nearest still-
// free driver, in rider order. Locally optimal, globally not. Returns total
// real (unscaled) distance of the matches it makes.
double greedyTotalDistance(const std::vector<Point>& riders,
                           const std::vector<Point>& drivers) {
    std::vector<char> taken(drivers.size(), 0);
    double total = 0.0;
    for (const auto& r : riders) {
        int best = -1;
        double bestD = std::numeric_limits<double>::max();
        for (int j = 0; j < static_cast<int>(drivers.size()); ++j) {
            if (taken[j]) continue;
            double d = realDistance(r, drivers[j]);
            if (d < bestD) { bestD = d; best = j; }
        }
        if (best != -1) { taken[best] = 1; total += bestD; }
    }
    return total;
}

// Convert scaled integer solver cost back to real distance units.
double unscale(long long scaledCost) { return scaledCost / COST_SCALE; }

template <typename F>
double timeMs(F&& fn) {
    auto start = std::chrono::steady_clock::now();
    fn();
    auto end = std::chrono::steady_clock::now();
    return std::chrono::duration<double, std::milli>(end - start).count();
}

void runScenario(const char* title, int N, int M, int k, double grid, std::mt19937& rng) {
    std::uniform_real_distribution<double> dis(0.0, grid);
    std::vector<Point> riders, drivers;
    riders.reserve(N); drivers.reserve(M);
    for (int i = 0; i < N; ++i) riders.emplace_back(i, dis(rng), dis(rng));
    for (int j = 0; j < M; ++j) drivers.emplace_back(j, dis(rng), dis(rng));

    std::cout << "\n== " << title << " (N=" << N << " riders, M=" << M
              << " drivers, k=" << k << ") ==\n";

    // Greedy baseline.
    double greedy = greedyTotalDistance(riders, drivers);

    // Optimal, dense.
    Assignment dense;
    std::vector<MatchEdge> denseEdges;
    double denseBuildMs = timeMs([&] { denseEdges = buildDenseEdges(riders, drivers); });
    double denseSolveMs = timeMs([&] { dense = solveAssignment(N, M, denseEdges); });

    // Optimal, sparse (k nearest).
    Assignment sparse;
    std::vector<MatchEdge> sparseEdges;
    double sparseBuildMs = timeMs([&] { sparseEdges = buildSparseEdges(riders, drivers, k, grid); });
    double sparseSolveMs = timeMs([&] { sparse = solveAssignment(N, M, sparseEdges); });

    // Average distance PER MATCHED RIDER. This is the fair cross-method
    // comparison: sparse may match slightly fewer riders (a rider whose k
    // nearest drivers are all taken goes unmatched), so its *total* can look
    // deceptively low. Average-per-match controls for that.
    auto avg = [](double total, int matched) {
        return matched > 0 ? total / matched : 0.0;
    };
    int greedyMatched = std::min(N, M);

    std::cout << std::fixed << std::setprecision(2);
    std::cout << "  greedy        : total " << std::setw(11) << greedy
              << "  avg/match " << std::setw(8) << avg(greedy, greedyMatched)
              << "  matched " << greedyMatched << "\n";
    std::cout << "  optimal dense : total " << std::setw(11) << unscale(dense.totalCost)
              << "  avg/match " << std::setw(8) << avg(unscale(dense.totalCost), dense.matchedCount)
              << "  matched " << dense.matchedCount
              << "  edges " << denseEdges.size()
              << "  build " << denseBuildMs << "ms solve " << denseSolveMs << "ms\n";
    std::cout << "  optimal sparse: total " << std::setw(11) << unscale(sparse.totalCost)
              << "  avg/match " << std::setw(8) << avg(unscale(sparse.totalCost), sparse.matchedCount)
              << "  matched " << sparse.matchedCount
              << "  edges " << sparseEdges.size()
              << "  build " << sparseBuildMs << "ms solve " << sparseSolveMs << "ms\n";

    double improvement = greedy > 0 ? (greedy - unscale(dense.totalCost)) / greedy * 100.0 : 0.0;
    std::cout << "  -> optimal beats greedy by " << improvement << "%  on total distance\n";

    if (N > M) {
        int unmatched = 0;
        for (int d : dense.riderToDriver) if (d == -1) ++unmatched;
        std::cout << "  -> overflow: " << unmatched
                  << " rider(s) left unmatched (policy: re-queue or reject upstream)\n";
    }
}

} // namespace

int main() {
    std::mt19937 rng(2026); // fixed seed => reproducible demo
    const double GRID = 10000.0;

    runScenario("balanced",        200, 200, 8, GRID, rng);
    runScenario("more riders",     300, 100, 8, GRID, rng);   // overflow case
    runScenario("dense vs sparse", 600, 600, 8, GRID, rng);   // cost/speed tradeoff

    std::cout << "\nNote: dense-solve time grows steeply (SPFA min-cost-flow is O(F*V*E)); the\n"
                 "sparse path is what holds low latency at scale. Dijkstra-with-potentials\n"
                 "is the further speedup, scheduled as a Week 5 optimization.\n";
    return 0;
}
