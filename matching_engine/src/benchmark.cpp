// Week 2 stretch goal: put a real number behind "the quadtree is fast."
// Compares QuadTree::nearestNeighbor against a brute-force O(N) linear scan
// for the same queries, at several dataset sizes, and prints the speedup.

#include <iostream>
#include <vector>
#include <random>
#include <chrono>
#include <cmath>
#include <iomanip>
#include "quadtree.h"

namespace {

// The brute-force oracle every correctness check and this benchmark measures
// against: scan every point, no indexing.
Point bruteForceNearest(const std::vector<Point>& points, double x, double y) {
    const Point* best = nullptr;
    double bestDistSq = std::numeric_limits<double>::max();
    for (const auto& p : points) {
        double dx = p.x - x, dy = p.y - y;
        double distSq = dx * dx + dy * dy;
        if (distSq < bestDistSq) {
            bestDistSq = distSq;
            best = &p;
        }
    }
    return *best;
}

void runBenchmark(int numPoints, int numQueries, double gridSize) {
    std::mt19937 gen(42); // fixed seed: comparable, reproducible runs
    std::uniform_real_distribution<double> dis(0.0, gridSize);

    std::vector<Point> points;
    points.reserve(numPoints);
    for (int i = 0; i < numPoints; ++i) points.emplace_back(i, dis(gen), dis(gen));

    AABB worldBounds(gridSize / 2.0, gridSize / 2.0, gridSize / 2.0, gridSize / 2.0);
    QuadTree tree(worldBounds, 4);
    for (const auto& p : points) tree.insert(p);

    std::vector<Point> queries;
    queries.reserve(numQueries);
    for (int i = 0; i < numQueries; ++i) queries.emplace_back(i, dis(gen), dis(gen));

    // Both loops accumulate a checksum of the matched IDs. Two reasons:
    // (1) an unused return value is dead code the optimizer is free to
    //     delete entirely (which silently zeroed brute-force's time here),
    //     and reading a volatile sink defeats that; (2) if quadtree and
    //     brute-force ever disagree, the checksums diverge — a cheap
    //     correctness check riding along with the timing.
    volatile long long bfChecksum = 0;
    volatile long long qtChecksum = 0;

    auto bfStart = std::chrono::steady_clock::now();
    for (const auto& q : queries) bfChecksum += bruteForceNearest(points, q.x, q.y).id;
    auto bfEnd = std::chrono::steady_clock::now();
    double bfMs = std::chrono::duration<double, std::milli>(bfEnd - bfStart).count();

    auto qtStart = std::chrono::steady_clock::now();
    for (const auto& q : queries) {
        auto match = tree.nearestNeighbor(q.x, q.y);
        if (match) qtChecksum += match->id;
    }
    auto qtEnd = std::chrono::steady_clock::now();
    double qtMs = std::chrono::duration<double, std::milli>(qtEnd - qtStart).count();

    std::cout << std::fixed << std::setprecision(3)
              << "N=" << std::setw(7) << numPoints
              << "  brute-force=" << std::setw(9) << bfMs << " ms"
              << "  quadtree=" << std::setw(9) << qtMs << " ms"
              << "  speedup=" << std::setw(8) << (bfMs / qtMs) << "x"
              << (bfChecksum == qtChecksum ? "  [match]" : "  [MISMATCH]") << "\n";
}

} // namespace

int main() {
    const int    NUM_QUERIES = 2000;
    const double GRID_SIZE   = 100000.0;

    std::cout << NUM_QUERIES << " nearest-neighbor queries per dataset size:\n";
    for (int n : {100, 1000, 10000, 100000, 500000}) {
        runBenchmark(n, NUM_QUERIES, GRID_SIZE);
    }
    return 0;
}
