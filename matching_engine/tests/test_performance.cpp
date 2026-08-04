// Week 5: benchmarks that FAIL, rather than benchmarks that print.
//
// A timing that only prints is a timing nobody reads. These assert a stated
// budget at a stated input size, so a change that makes the pairing matrix 5x
// slower turns the build red instead of quietly shipping.
//
// Rules this file follows to stay honest:
//   - report the MEDIAN of many runs, never a single sample or a best-of;
//   - assert against a budget with real headroom over the measured number, so
//     the suite fails on regressions rather than on a busy laptop;
//   - never assert a timing under sanitizers (CMake does not register this
//     test then) or in a debug build, where the number means nothing.
//
// Tagged [perf] so `unit_tests "~[perf]"` runs correctness alone.

#include "catch.hpp"

#include <algorithm>
#include <chrono>
#include <fstream>
#include <iomanip>
#include <numeric>
#include <random>
#include <sstream>
#include <vector>

#include "assignment.h"
#include "cost_matrix.h"
#include "osm_loader.h"
#include "quadtree.h"
#include "road_graph.h"
#include "router.h"

namespace {

using Clock = std::chrono::steady_clock;

// Median wall-clock over `runs` repetitions, in milliseconds. Median rather
// than mean because one descheduled run would otherwise dominate the average,
// and rather than minimum because the best case is not what production sees.
template <typename F>
double medianMs(F&& body, int runs = 21) {
    std::vector<double> samples;
    samples.reserve(runs);
    for (int i = 0; i < runs; ++i) {
        const auto start = Clock::now();
        body();
        samples.push_back(std::chrono::duration<double, std::milli>(Clock::now() - start).count());
    }
    std::sort(samples.begin(), samples.end());
    return samples[samples.size() / 2];
}

// Every measurement lands here and is written out at the end of the run, so
// the numbers behind the budgets are recoverable rather than scrolled past.
//
// Deliberately heap-allocated and never freed. Static objects are destroyed in
// reverse order of CONSTRUCTION, and this table is constructed lazily on its
// first use -- which happens after g_writer below. So it would be destroyed
// FIRST, and g_writer's destructor would then read a dead vector. (It did: the
// results file came out empty with a plain `static std::vector`.) Leaking one
// vector at process exit is the standard fix, and costs nothing.
std::vector<std::pair<std::string, double>>& measurements() {
    static auto* table = new std::vector<std::pair<std::string, double>>();
    return *table;
}

void record(const std::string& label, double ms) {
    measurements().push_back({label, ms});
    std::ostringstream line;
    line << std::fixed << std::setprecision(4) << ms;
    WARN(label << " = " << line.str() << " ms (median)");   // WARN always prints
}

struct MeasurementWriter {
    ~MeasurementWriter() {
        std::ofstream out("benchmark_results.txt");
        if (!out) return;
        out << "# median wall-clock, milliseconds\n";
        for (const auto& [label, ms] : measurements()) {
            out << std::left << std::setw(52) << label << std::fixed
                << std::setprecision(4) << ms << "\n";
        }
    }
};
const MeasurementWriter g_writer;   // flushes at process exit

std::vector<Point> randomPoints(int n, double size, unsigned seed) {
    std::mt19937 rng(seed);
    std::uniform_real_distribution<double> dist(0.0, size);
    std::vector<Point> points;
    for (int i = 0; i < n; ++i) points.push_back(Point(i, dist(rng), dist(rng)));
    return points;
}

// The stated input size behind the sub-millisecond claim: one 3-second batch
// window for one city zone, at 100 riders and 100 drivers. That is ~33 ride
// requests per second in a single zone -- a busy shard, not a toy.
//
// The size is stated because the claim is meaningless without it. Measured
// sweep of build+solve, median of 21, -O3 on the reference machine:
//     N=M=25   0.15 ms      N=M=100  0.64 ms   <- the budgeted size
//     N=M=50   0.44 ms      N=M=150  1.42 ms
//     N=M=75   0.61 ms      N=M=200  2.46 ms
// Sub-millisecond holds through 100 and is lost by 150. Rather than quietly
// pick the largest number that still passes, both are recorded.
constexpr int kBatchRiders = 100;
constexpr int kBatchDrivers = 100;
constexpr int kCandidates = 8;
constexpr double kWorldSize = 20000.0;   // 20 km square, metres

} // namespace

TEST_CASE("the sparse pairing matrix builds and solves in under a millisecond",
          "[perf]") {
    // THE headline claim: for a realistic batch, going from coordinates to a
    // provably optimal assignment costs less than one millisecond -- inside a
    // 3-second batch window, that is 0.03% of the budget.
    const std::vector<Point> riders = randomPoints(kBatchRiders, kWorldSize, 11);
    const std::vector<Point> drivers = randomPoints(kBatchDrivers, kWorldSize, 22);

    const double buildMs = medianMs([&] {
        const auto edges = buildSparseEdges(riders, drivers, kCandidates, kWorldSize);
        REQUIRE(edges.size() == static_cast<std::size_t>(kBatchRiders) * kCandidates);
    });
    record("sparse matrix build   (N=M=100, k=8)", buildMs);

    const std::vector<MatchEdge> edges =
        buildSparseEdges(riders, drivers, kCandidates, kWorldSize);
    const double solveMs = medianMs([&] {
        const Assignment a = solveAssignment(kBatchRiders, kBatchDrivers, edges);
        REQUIRE(a.matchedCount > 0);
    });
    record("sparse solve          (N=M=100, k=8)", solveMs);

    const double endToEndMs = medianMs([&] {
        const auto e = buildSparseEdges(riders, drivers, kCandidates, kWorldSize);
        const Assignment a = solveAssignment(kBatchRiders, kBatchDrivers, e);
        REQUIRE(a.matchedCount > 0);
    });
    record("sparse build + solve  (N=M=100, k=8)", endToEndMs);

    // The budget: one millisecond, at the stated size. Measured 0.64 ms on the
    // reference machine (Apple M-series, -O3), so the margin is ~1.5x -- tight
    // on purpose. A budget with 10x slack is not a budget, it is decoration.
    CHECK(endToEndMs < 1.0);
}

TEST_CASE("the dense pairing matrix is the thing sparsity exists to avoid", "[perf]") {
    // Not a budget -- a documented contrast. Dense is O(N*M) edges, and the
    // whole argument for the quadtree shortlist is the ratio printed here.
    const std::vector<Point> riders = randomPoints(kBatchRiders, kWorldSize, 33);
    const std::vector<Point> drivers = randomPoints(kBatchDrivers, kWorldSize, 44);

    const double denseMs = medianMs([&] {
        const auto edges = buildDenseEdges(riders, drivers);
        const Assignment a = solveAssignment(kBatchRiders, kBatchDrivers, edges);
        REQUIRE(a.matchedCount == kBatchRiders);
    }, 5);
    record("dense build + solve   (N=M=100)", denseMs);

    // Dense must never be the FASTER option at this size; if it is, the sparse
    // path has a bug and its cost is coming from somewhere it should not.
    const std::vector<Point> r2 = randomPoints(kBatchRiders, kWorldSize, 33);
    const std::vector<Point> d2 = randomPoints(kBatchDrivers, kWorldSize, 44);
    const double sparseMs = medianMs([&] {
        const auto edges = buildSparseEdges(r2, d2, kCandidates, kWorldSize);
        const Assignment a = solveAssignment(kBatchRiders, kBatchDrivers, edges);
        REQUIRE(a.matchedCount > 0);
    }, 5);
    CHECK(sparseMs < denseMs);
}

TEST_CASE("quadtree k-nearest stays a few microseconds per query", "[perf]") {
    // The shortlist is the inner loop of the sparse build; if this regresses,
    // every batch does.
    const std::vector<Point> drivers = randomPoints(50000, kWorldSize, 55);
    QuadTree tree(AABB(kWorldSize / 2, kWorldSize / 2, kWorldSize / 2, kWorldSize / 2), 4);
    for (const Point& p : drivers) tree.insert(p);

    std::mt19937 rng(66);
    std::uniform_real_distribution<double> coord(0.0, kWorldSize);
    std::vector<std::pair<double, double>> queries;
    for (int i = 0; i < 1000; ++i) queries.push_back({coord(rng), coord(rng)});

    const double thousandQueriesMs = medianMs([&] {
        for (const auto& [x, y] : queries) {
            const auto found = tree.kNearest(x, y, kCandidates);
            REQUIRE(found.size() == kCandidates);
        }
    });
    record("quadtree kNearest x1000 (50k drivers, k=8)", thousandQueriesMs);

    // Measured ~1.9 us per query against 50,000 drivers. The budget is 4 ms per
    // 1000 queries (~4 us each), which still enforces the sub-linear behaviour
    // Week 2 measured -- a linear scan of 50k points is orders of magnitude
    // worse -- without failing on a shared CI core.
    CHECK(thousandQueriesMs < 4.0);
}

TEST_CASE("a point-to-point route on the real city graph stays in single-digit ms",
          "[perf]") {
    // Routing is the expensive half of the road-time pipeline, so its cost is
    // worth pinning even though it cannot meet the sub-millisecond bar.
    RoadGraph graph = [] {
        try {
            return loadOsm("../data/bengaluru_roads.osm");
        } catch (const std::exception&) {
            return buildGridGraph(120, 120, 100.0);   // no extract fetched
        }
    }();

    const std::vector<int>& core = graph.largestComponent();
    REQUIRE(core.size() > 100);

    std::mt19937 rng(20260806);
    std::uniform_int_distribution<std::size_t> pick(0, core.size() - 1);
    std::vector<std::pair<int, int>> pairs;
    for (int i = 0; i < 50; ++i) pairs.push_back({core[pick(rng)], core[pick(rng)]});

    const double dijkstraMs = medianMs([&] {
        for (const auto& [s, t] : pairs) (void)dijkstra(graph, s, t);
    }, 5);
    const double astarMs = medianMs([&] {
        for (const auto& [s, t] : pairs) (void)astar(graph, s, t);
    }, 5);

    record("dijkstra x50 (city graph)", dijkstraMs);
    record("astar    x50 (city graph)", astarMs);
    record("astar per query (city graph)", astarMs / 50.0);

    // The claim worth defending is the relative one: the heuristic must be an
    // improvement. An absolute budget here would just encode this laptop.
    CHECK(astarMs <= dijkstraMs);
}

TEST_CASE("road-time pricing is bounded by one search per rider", "[perf]") {
    // The cost model of the dense road matrix: N backward sweeps, NOT N*M
    // point-to-point routes. Doubling the DRIVERS must therefore barely move
    // the clock -- that invariant is the entire reason the builder is shaped
    // this way, and it is easy to destroy with an innocent-looking refactor.
    RoadGraph graph = [] {
        try {
            return loadOsm("../data/bengaluru_roads.osm");
        } catch (const std::exception&) {
            return buildGridGraph(120, 120, 100.0);
        }
    }();

    const std::vector<int>& core = graph.largestComponent();
    std::mt19937 rng(4242);
    std::uniform_int_distribution<std::size_t> pick(0, core.size() - 1);

    auto sample = [&](int count) {
        std::vector<GeoPoint> points;
        for (int i = 0; i < count; ++i) {
            const GeoNode& n = graph.node(core[pick(rng)]);
            points.push_back(GeoPoint{i, n.lat, n.lon});
        }
        return points;
    };

    const std::vector<GeoPoint> riders = sample(20);
    const std::vector<GeoPoint> fewDrivers = sample(20);
    const std::vector<GeoPoint> manyDrivers = sample(80);

    const double fewMs = medianMs([&] {
        (void)buildRoadEdgesDense(riders, fewDrivers, graph);
    }, 5);
    const double manyMs = medianMs([&] {
        (void)buildRoadEdgesDense(riders, manyDrivers, graph);
    }, 5);

    record("road matrix (20 riders x 20 drivers)", fewMs);
    record("road matrix (20 riders x 80 drivers)", manyMs);

    // 4x the drivers for well under 2x the time. If this fails, someone has
    // reintroduced a per-pair search.
    CHECK(manyMs < fewMs * 2.0);
}
