// Cost matrix: the boundary between geometry and the solver.
//
// Everything upstream of here deals in coordinates; everything downstream
// deals in integers. Bugs in this file are the expensive kind, because the
// solver will faithfully find the optimal answer to the wrong question.

#include "catch.hpp"

#include <cmath>
#include <random>
#include <set>

#include "assignment.h"
#include "cost_matrix.h"
#include "oracles.h"
#include "osm_loader.h"
#include "road_graph.h"
#include "router.h"

namespace {

std::vector<Point> pointsAt(const std::vector<std::pair<double, double>>& coords) {
    std::vector<Point> points;
    for (int i = 0; i < static_cast<int>(coords.size()); ++i) {
        points.push_back(Point(i, coords[i].first, coords[i].second));
    }
    return points;
}

// Look up the cost of a specific pair in an edge list; -1 if absent.
long long costOf(const std::vector<MatchEdge>& edges, int rider, int driver) {
    for (const MatchEdge& e : edges) {
        if (e.rider == rider && e.driver == driver) return e.cost;
    }
    return -1;
}

} // namespace

TEST_CASE("dense euclidean edges cover every pair, priced by distance", "[cost]") {
    const std::vector<Point> riders = pointsAt({{0.0, 0.0}, {10.0, 0.0}});
    const std::vector<Point> drivers = pointsAt({{3.0, 4.0}, {0.0, 0.0}});

    const std::vector<MatchEdge> edges = buildDenseEdges(riders, drivers);
    REQUIRE(edges.size() == 4);

    REQUIRE(costOf(edges, 0, 0) == llround(5.0 * COST_SCALE));   // 3-4-5 triangle
    REQUIRE(costOf(edges, 0, 1) == 0);                           // same spot
    REQUIRE(costOf(edges, 1, 1) == llround(10.0 * COST_SCALE));
}

TEST_CASE("empty rider or driver lists produce no edges", "[cost][edge]") {
    const std::vector<Point> some = pointsAt({{1.0, 1.0}});
    REQUIRE(buildDenseEdges({}, some).empty());
    REQUIRE(buildDenseEdges(some, {}).empty());
    REQUIRE(buildDenseEdges({}, {}).empty());

    REQUIRE(buildSparseEdges({}, some, 3, 100.0).empty());
    REQUIRE(buildSparseEdges(some, {}, 3, 100.0).empty());
    REQUIRE(buildSparseEdges(some, some, 0, 100.0).empty());     // k = 0
    REQUIRE(buildSparseEdges(some, some, -1, 100.0).empty());
}

TEST_CASE("sparse edges are the k cheapest of the dense ones", "[cost]") {
    std::mt19937 rng(4004);
    std::uniform_real_distribution<double> coord(0.0, 100.0);

    std::vector<Point> riders, drivers;
    for (int i = 0; i < 30; ++i) riders.push_back(Point(i, coord(rng), coord(rng)));
    for (int j = 0; j < 40; ++j) drivers.push_back(Point(j, coord(rng), coord(rng)));

    constexpr int k = 5;
    const std::vector<MatchEdge> dense = buildDenseEdges(riders, drivers);
    const std::vector<MatchEdge> sparse = buildSparseEdges(riders, drivers, k, 100.0);

    REQUIRE(sparse.size() == riders.size() * k);

    for (int i = 0; i < static_cast<int>(riders.size()); ++i) {
        // Collect this rider's dense costs and its sparse costs.
        std::vector<long long> denseCosts, sparseCosts;
        for (const MatchEdge& e : dense) if (e.rider == i) denseCosts.push_back(e.cost);
        for (const MatchEdge& e : sparse) if (e.rider == i) sparseCosts.push_back(e.cost);

        REQUIRE(sparseCosts.size() == k);
        std::sort(denseCosts.begin(), denseCosts.end());
        std::sort(sparseCosts.begin(), sparseCosts.end());
        denseCosts.resize(k);
        // Same multiset of costs: the shortlist really is the k nearest.
        REQUIRE(sparseCosts == denseCosts);

        // And no driver appears twice for one rider.
        std::set<int> seen;
        for (const MatchEdge& e : sparse) {
            if (e.rider == i) REQUIRE(seen.insert(e.driver).second);
        }
    }
}

TEST_CASE("k larger than the driver pool returns every driver, once", "[cost][edge]") {
    const std::vector<Point> riders = pointsAt({{5.0, 5.0}});
    const std::vector<Point> drivers = pointsAt({{1.0, 1.0}, {9.0, 9.0}, {5.0, 6.0}});
    const std::vector<MatchEdge> edges = buildSparseEdges(riders, drivers, 99, 10.0);
    REQUIRE(edges.size() == 3);

    std::set<int> seen;
    for (const MatchEdge& e : edges) REQUIRE(seen.insert(e.driver).second);
}

TEST_CASE("coincident riders and drivers cost nothing and still match", "[cost][edge]") {
    // Everyone at the same spot: all costs 0, and any complete assignment is
    // optimal. The point is that the pipeline survives it.
    std::vector<Point> riders, drivers;
    for (int i = 0; i < 5; ++i) riders.push_back(Point(i, 50.0, 50.0));
    for (int j = 0; j < 5; ++j) drivers.push_back(Point(j, 50.0, 50.0));

    const std::vector<MatchEdge> edges = buildDenseEdges(riders, drivers);
    for (const MatchEdge& e : edges) REQUIRE(e.cost == 0);

    const Assignment a = solveAssignment(5, 5, edges);
    REQUIRE(a.matchedCount == 5);
    REQUIRE(a.totalCost == 0);

    const std::vector<MatchEdge> sparse = buildSparseEdges(riders, drivers, 3, 100.0);
    REQUIRE(sparse.size() == 15);
}

// ---------------------------------------------------------------------------
// Road-time costs
// ---------------------------------------------------------------------------

TEST_CASE("dense road edges are priced in seconds, in the driver->rider direction",
          "[cost][road]") {
    const RoadGraph graph = buildGridGraph(8, 8, 100.0, 12.9700, 77.5900, 10.0);

    std::vector<GeoPoint> riders, drivers;
    for (int v : {0, 9, 27}) {
        riders.push_back(GeoPoint{static_cast<int>(riders.size()),
                                  graph.node(v).lat, graph.node(v).lon});
    }
    for (int v : {63, 40, 5, 18}) {
        drivers.push_back(GeoPoint{static_cast<int>(drivers.size()),
                                   graph.node(v).lat, graph.node(v).lon});
    }

    const std::vector<MatchEdge> edges = buildRoadEdgesDense(riders, drivers, graph);
    REQUIRE(edges.size() == riders.size() * drivers.size());

    // Every cost must equal an independently routed driver -> rider trip.
    for (const MatchEdge& e : edges) {
        const int riderNode = graph.nearestNode(riders[e.rider].lat, riders[e.rider].lon);
        const int driverNode = graph.nearestNode(drivers[e.driver].lat, drivers[e.driver].lon);
        const RouteResult route = astar(graph, driverNode, riderNode);
        REQUIRE(route.found());
        REQUIRE(e.cost == std::llround(route.travelSeconds * COST_SCALE));
    }
}

TEST_CASE("road costs really are directional", "[cost][road][edge]") {
    // A one-way pair: the driver can reach the rider, but not the reverse.
    // If the builder ever routed rider -> driver by mistake, this flips.
    RoadGraphBuilder b;
    const int driverNode = b.addNode(1, 12.9700, 77.5900);
    const int riderNode = b.addNode(2, 12.9710, 77.5900);
    b.addArc(driverNode, riderNode, 100.0, 10.0);        // one-way, towards the rider
    const RoadGraph graph(b);

    const std::vector<GeoPoint> riders = {{0, 12.9710, 77.5900}};
    const std::vector<GeoPoint> drivers = {{0, 12.9700, 77.5900}};

    const std::vector<MatchEdge> forward = buildRoadEdgesDense(riders, drivers, graph);
    REQUIRE(forward.size() == 1);
    REQUIRE(forward[0].cost == std::llround(10.0 * COST_SCALE));

    // Swap the roles: now the "driver" is downstream and cannot reach the rider.
    const std::vector<MatchEdge> backward = buildRoadEdgesDense(drivers, riders, graph);
    REQUIRE(backward.empty());
}

TEST_CASE("an unreachable driver contributes no edge and leaves the rider unmatched",
          "[cost][road][edge][overflow]") {
    // Two islands. The only driver is on the wrong one -- the rider must come
    // back unmatched rather than paired with someone who can never arrive.
    RoadGraphBuilder b;
    const int a1 = b.addNode(1, 12.9700, 77.5900);
    const int a2 = b.addNode(2, 12.9710, 77.5900);
    const int b1 = b.addNode(3, 12.9900, 77.6100);
    const int b2 = b.addNode(4, 12.9910, 77.6100);
    b.addArc(a1, a2, 100.0, 10.0);
    b.addArc(a2, a1, 100.0, 10.0);
    b.addArc(b1, b2, 100.0, 10.0);
    b.addArc(b2, b1, 100.0, 10.0);
    const RoadGraph graph(b);

    const std::vector<GeoPoint> riders = {{0, 12.9700, 77.5900}};   // island A
    const std::vector<GeoPoint> drivers = {{0, 12.9900, 77.6100}};  // island B

    const std::vector<MatchEdge> edges = buildRoadEdgesDense(riders, drivers, graph);
    REQUIRE(edges.empty());

    const Assignment a = solveAssignment(1, 1, edges);
    REQUIRE(a.matchedCount == 0);
    REQUIRE(a.riderToDriver[0] == -1);
}

TEST_CASE("empty batches produce no road edges", "[cost][road][edge]") {
    const RoadGraph graph = buildGridGraph(4, 4, 100.0);
    const std::vector<GeoPoint> some = {{0, 12.9700, 77.5900}};
    REQUIRE(buildRoadEdgesDense({}, some, graph).empty());
    REQUIRE(buildRoadEdgesDense(some, {}, graph).empty());
    REQUIRE(buildRoadEdgesSparse({}, some, graph, 4).empty());
    REQUIRE(buildRoadEdgesSparse(some, {}, graph, 4).empty());
    REQUIRE(buildRoadEdgesSparse(some, some, graph, 0).empty());
}

TEST_CASE("sparse road edges are a subset of the dense ones, at identical prices",
          "[cost][road]") {
    // Sparsity must only ever REMOVE candidates. If a surviving edge has a
    // different price than its dense twin, the two builders disagree about
    // what a trip costs, and the sparse solve is optimising a different problem.
    const RoadGraph graph = buildGridGraph(10, 10, 100.0);
    const std::vector<int>& core = graph.largestComponent();

    std::mt19937 rng(20260806);
    std::uniform_int_distribution<std::size_t> pick(0, core.size() - 1);

    std::vector<GeoPoint> riders, drivers;
    for (int i = 0; i < 12; ++i) {
        const GeoNode& n = graph.node(core[pick(rng)]);
        riders.push_back(GeoPoint{i, n.lat, n.lon});
    }
    for (int j = 0; j < 15; ++j) {
        const GeoNode& n = graph.node(core[pick(rng)]);
        drivers.push_back(GeoPoint{j, n.lat, n.lon});
    }

    constexpr int k = 4;
    const std::vector<MatchEdge> dense = buildRoadEdgesDense(riders, drivers, graph);
    const std::vector<MatchEdge> sparse = buildRoadEdgesSparse(riders, drivers, graph, k);

    REQUIRE(sparse.size() <= dense.size());
    for (const MatchEdge& e : sparse) {
        REQUIRE(costOf(dense, e.rider, e.driver) == e.cost);
    }

    // Each rider keeps at most k candidates.
    for (int i = 0; i < static_cast<int>(riders.size()); ++i) {
        const auto count = std::count_if(sparse.begin(), sparse.end(),
                                         [i](const MatchEdge& e) { return e.rider == i; });
        REQUIRE(count <= k);
    }
}

TEST_CASE("the road-time optimum is never beaten by the distance optimum, on road time",
          "[cost][road]") {
    // The claim Week 4 rests on. Both assignments are scored with the SAME
    // road-time matrix, so the one that was actually optimised for it must win
    // -- or the cost matrix and the solver disagree about what they measure.
    const RoadGraph graph = buildGridGraph(14, 14, 100.0);
    const std::vector<int>& core = graph.largestComponent();

    std::mt19937 rng(555);
    std::uniform_int_distribution<std::size_t> pick(0, core.size() - 1);

    constexpr int kBatch = 12;
    std::vector<GeoPoint> riders, drivers;
    std::vector<Point> flatRiders, flatDrivers;
    for (int i = 0; i < kBatch; ++i) {
        const GeoNode& n = graph.node(core[pick(rng)]);
        riders.push_back(GeoPoint{i, n.lat, n.lon});
        flatRiders.push_back(Point(i, n.lon * 100000.0, n.lat * 100000.0));
    }
    for (int j = 0; j < kBatch; ++j) {
        const GeoNode& n = graph.node(core[pick(rng)]);
        drivers.push_back(GeoPoint{j, n.lat, n.lon});
        flatDrivers.push_back(Point(j, n.lon * 100000.0, n.lat * 100000.0));
    }

    const std::vector<MatchEdge> roadEdges = buildRoadEdgesDense(riders, drivers, graph);
    const Assignment roadMatch = solveAssignment(kBatch, kBatch, roadEdges);
    const Assignment distanceMatch =
        solveAssignment(kBatch, kBatch, buildDenseEdges(flatRiders, flatDrivers));

    REQUIRE(roadMatch.matchedCount == kBatch);
    REQUIRE(distanceMatch.matchedCount == kBatch);

    long long distanceMatchRoadCost = 0;
    for (int i = 0; i < kBatch; ++i) {
        distanceMatchRoadCost += costOf(roadEdges, i, distanceMatch.riderToDriver[i]);
    }
    REQUIRE(roadMatch.totalCost <= distanceMatchRoadCost);
}
