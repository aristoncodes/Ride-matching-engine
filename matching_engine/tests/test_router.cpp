// Router (Week 4): Dijkstra, A*, and the one-to-all searches the matcher uses.
//
// The central claim being tested is A*'s: that a heuristic which never
// over-estimates changes only the ORDER of the search, never its answer. So
// every A* result is checked against Dijkstra, and Dijkstra itself is checked
// against Bellman-Ford, which shares none of its machinery.

#include "catch.hpp"

#include <cmath>
#include <random>
#include <set>

#include "oracles.h"
#include "osm_loader.h"
#include "road_graph.h"
#include "router.h"

namespace {

// A tiny hand-built graph, so the expected answers are arithmetic rather than
// the output of another algorithm.
//
//   0 --100m--> 1 --100m--> 2        all at 10 m/s, so 10 s per hop
//   |                       ^
//   +--------- 500m --------+        the direct road is longer in time
//
RoadGraph tinyGraph() {
    RoadGraphBuilder b;
    const int n0 = b.addNode(10, 12.9700, 77.5900);
    const int n1 = b.addNode(11, 12.9709, 77.5900);
    const int n2 = b.addNode(12, 12.9718, 77.5900);
    b.addArc(n0, n1, 100.0, 10.0);
    b.addArc(n1, n0, 100.0, 10.0);
    b.addArc(n1, n2, 100.0, 10.0);
    b.addArc(n2, n1, 100.0, 10.0);
    b.addArc(n0, n2, 500.0, 10.0);
    b.addArc(n2, n0, 500.0, 10.0);
    return RoadGraph(b);
}

// Sum the arc times along a path, verifying each consecutive pair is really
// joined by an arc. This is what makes "the path is real", not just "the
// number looks right" -- a router can return a correct cost with a path that
// teleports.
double walkPath(const RoadGraph& graph, const std::vector<int>& path) {
    double seconds = 0.0;
    for (std::size_t i = 0; i + 1 < path.size(); ++i) {
        bool linked = false;
        for (const RoadArc& arc : graph.outgoing(path[i])) {
            if (arc.to == path[i + 1]) {
                // Parallel arcs exist in OSM (a service road beside a main
                // road); the router will have used the cheapest.
                seconds += arc.travelSeconds;
                linked = true;
                break;
            }
        }
        REQUIRE(linked);
    }
    return seconds;
}

} // namespace

TEST_CASE("haversine is symmetric, zero at a point, and roughly right", "[router][geo]") {
    REQUIRE(haversineMeters(12.97, 77.59, 12.97, 77.59) == Approx(0.0));
    REQUIRE(haversineMeters(12.97, 77.59, 12.98, 77.60) ==
            Approx(haversineMeters(12.98, 77.60, 12.97, 77.59)));

    // One degree of latitude is ~111.2 km anywhere on the globe.
    REQUIRE(haversineMeters(0.0, 0.0, 1.0, 0.0) == Approx(111194.9).epsilon(0.001));

    // A degree of longitude shrinks by cos(latitude) -- the correction the
    // node index and the sparse cost matrix both depend on.
    REQUIRE(haversineMeters(60.0, 0.0, 60.0, 1.0) ==
            Approx(haversineMeters(0.0, 0.0, 0.0, 1.0) * std::cos(60.0 * M_PI / 180.0))
                .epsilon(0.001));
}

TEST_CASE("the router prefers two short hops over one long road", "[router]") {
    const RoadGraph graph = tinyGraph();

    const RouteResult route = dijkstra(graph, 0, 2);
    REQUIRE(route.found());
    REQUIRE(route.travelSeconds == Approx(20.0));       // 100m + 100m at 10 m/s
    REQUIRE(route.lengthMeters == Approx(200.0));
    REQUIRE(route.path == std::vector<int>{0, 1, 2});   // not the 500m direct arc

    const RouteResult guided = astar(graph, 0, 2);
    REQUIRE(guided.travelSeconds == Approx(route.travelSeconds));
    REQUIRE(guided.path == route.path);
}

TEST_CASE("source == target costs nothing", "[router][edge]") {
    const RoadGraph graph = tinyGraph();
    for (int v = 0; v < graph.numNodes(); ++v) {
        const RouteResult route = dijkstra(graph, v, v);
        REQUIRE(route.found());
        REQUIRE(route.travelSeconds == Approx(0.0));
        REQUIRE(route.lengthMeters == Approx(0.0));
        REQUIRE(route.path == std::vector<int>{v});

        const RouteResult guided = astar(graph, v, v);
        REQUIRE(guided.travelSeconds == Approx(0.0));
        REQUIRE(guided.path == std::vector<int>{v});
    }
}

TEST_CASE("out-of-range endpoints throw rather than read out of bounds", "[router][edge]") {
    const RoadGraph graph = tinyGraph();
    REQUIRE_THROWS_AS(dijkstra(graph, -1, 0), std::out_of_range);
    REQUIRE_THROWS_AS(dijkstra(graph, 0, graph.numNodes()), std::out_of_range);
    REQUIRE_THROWS_AS(astar(graph, graph.numNodes(), 0), std::out_of_range);
}

TEST_CASE("a one-way street is only traversable one way", "[router][edge]") {
    RoadGraphBuilder b;
    const int a = b.addNode(1, 12.9700, 77.5900);
    const int c = b.addNode(2, 12.9710, 77.5900);
    b.addArc(a, c, 100.0, 10.0);          // a -> c only
    const RoadGraph graph(b);

    REQUIRE(dijkstra(graph, a, c).found());
    REQUIRE_FALSE(dijkstra(graph, c, a).found());
    REQUIRE_FALSE(astar(graph, c, a).found());
    REQUIRE(dijkstra(graph, c, a).travelSeconds == kUnreachable);
    REQUIRE(dijkstra(graph, c, a).path.empty());

    // Only {a} and {c} are strongly connected here, so the "largest" SCC is a
    // single node -- exactly the degenerate case the demos must not sample from.
    REQUIRE(graph.largestComponent().size() == 1);
}

TEST_CASE("disconnected components are reported unreachable, not zero",
          "[router][edge]") {
    RoadGraphBuilder b;
    const int a = b.addNode(1, 12.9700, 77.5900);
    const int c = b.addNode(2, 12.9710, 77.5900);
    const int d = b.addNode(3, 12.9900, 77.6100);   // an island
    const int e = b.addNode(4, 12.9910, 77.6100);
    b.addArc(a, c, 100.0, 10.0);
    b.addArc(c, a, 100.0, 10.0);
    b.addArc(d, e, 100.0, 10.0);
    b.addArc(e, d, 100.0, 10.0);
    const RoadGraph graph(b);

    REQUIRE_FALSE(dijkstra(graph, a, d).found());
    REQUIRE_FALSE(astar(graph, a, d).found());

    const std::vector<double> tree = shortestPathTree(graph, a);
    REQUIRE(tree[c] == Approx(10.0));
    REQUIRE(tree[d] == kUnreachable);
    REQUIRE(tree[e] == kUnreachable);

    // Two components of two; whichever is chosen, it has two nodes.
    REQUIRE(graph.largestComponent().size() == 2);
}

TEST_CASE("a single-node graph is routable to itself and nowhere else",
          "[router][edge]") {
    RoadGraphBuilder b;
    b.addNode(1, 12.97, 77.59);
    const RoadGraph graph(b);

    REQUIRE(graph.numNodes() == 1);
    REQUIRE(graph.numArcs() == 0);
    REQUIRE(dijkstra(graph, 0, 0).travelSeconds == Approx(0.0));
    REQUIRE(graph.nearestNode(12.97, 77.59) == 0);
    REQUIRE(graph.nearestNode(0.0, 0.0) == 0);   // falls back to the scan
}

TEST_CASE("a graph with no nodes is rejected at construction", "[router][edge]") {
    RoadGraphBuilder b;
    REQUIRE_THROWS_AS(RoadGraph(b), std::invalid_argument);
}

TEST_CASE("self-loops and zero-speed arcs are dropped", "[router][edge]") {
    RoadGraphBuilder b;
    const int a = b.addNode(1, 12.97, 77.59);
    const int c = b.addNode(2, 12.98, 77.59);
    b.addArc(a, a, 50.0, 10.0);    // self-loop: never on a shortest path
    b.addArc(a, c, 100.0, 0.0);    // zero speed: not traversable
    REQUIRE(b.numArcs() == 0);
}

TEST_CASE("Dijkstra agrees with Bellman-Ford on a grid", "[router]") {
    // Bellman-Ford shares no code with Dijkstra: no heap, no settled set, no
    // early exit. If both say the same thing, the answer is the graph's, not
    // an artefact of one implementation.
    const RoadGraph graph = buildGridGraph(15, 15, 100.0);

    std::mt19937 rng(2718);
    std::uniform_int_distribution<int> pick(0, graph.numNodes() - 1);
    for (int trial = 0; trial < 10; ++trial) {
        const int source = pick(rng);
        const std::vector<double> reference = oracle::shortestPathsByBellmanFord(graph, source);
        const std::vector<double> got = shortestPathTree(graph, source);

        REQUIRE(got.size() == reference.size());
        for (int v = 0; v < graph.numNodes(); ++v) {
            if (reference[v] == kUnreachable) {
                REQUIRE(got[v] == kUnreachable);
            } else {
                REQUIRE(got[v] == Approx(reference[v]).epsilon(1e-9));
            }
        }
    }
}

TEST_CASE("A* returns exactly Dijkstra's cost on every pair of a grid", "[router]") {
    // The headline claim of the week, checked exhaustively on a graph small
    // enough to afford all-pairs.
    const RoadGraph graph = buildGridGraph(9, 9, 120.0);

    double worstError = 0.0;
    for (int source = 0; source < graph.numNodes(); ++source) {
        for (int target = 0; target < graph.numNodes(); ++target) {
            const RouteResult reference = dijkstra(graph, source, target);
            const RouteResult guided = astar(graph, source, target);

            REQUIRE(reference.found() == guided.found());
            if (!reference.found()) continue;

            worstError = std::max(worstError,
                                  std::abs(reference.travelSeconds - guided.travelSeconds));
            REQUIRE(guided.travelSeconds == Approx(reference.travelSeconds).epsilon(1e-12));

            // A* must also never settle MORE nodes than Dijkstra on a graph
            // where the heuristic is informative -- if it does, the heuristic
            // is actively misleading the search.
            REQUIRE(guided.nodesSettled <= reference.nodesSettled);
        }
    }
    REQUIRE(worstError < 1e-9);
}

TEST_CASE("returned paths are real, connected, and cost what is claimed", "[router]") {
    const RoadGraph graph = buildGridGraph(12, 12, 100.0);

    std::mt19937 rng(1414);
    std::uniform_int_distribution<int> pick(0, graph.numNodes() - 1);

    for (int trial = 0; trial < 100; ++trial) {
        const int source = pick(rng);
        const int target = pick(rng);
        const RouteResult route = astar(graph, source, target);
        REQUIRE(route.found());

        REQUIRE(route.path.front() == source);
        REQUIRE(route.path.back() == target);

        // Walking the path arc by arc must reproduce the reported cost.
        REQUIRE(walkPath(graph, route.path) == Approx(route.travelSeconds).epsilon(1e-9));

        // A shortest path never revisits a node (all arc costs are positive).
        const std::set<int> unique(route.path.begin(), route.path.end());
        REQUIRE(unique.size() == route.path.size());
    }
}

TEST_CASE("grid distances are Manhattan, because a grid has no diagonals", "[router]") {
    // An analytic check, not a comparison against another algorithm: on a
    // rows x cols lattice the fastest route between two cells is |dr| + |dc|
    // hops, whatever order they are taken in.
    constexpr int kSide = 10;
    const RoadGraph graph = buildGridGraph(kSide, kSide, 100.0, 12.97, 77.59, 10.0);

    // Time for one hop, read off the graph rather than assumed: degrees-to-metres
    // conversion means a "100 m" edge is 100 m only to within rounding.
    const double hopSeconds = graph.outgoing(0).front().travelSeconds;

    std::mt19937 rng(161803);
    std::uniform_int_distribution<int> cell(0, kSide - 1);
    for (int trial = 0; trial < 200; ++trial) {
        const int r1 = cell(rng), c1 = cell(rng), r2 = cell(rng), c2 = cell(rng);
        const int source = r1 * kSide + c1;
        const int target = r2 * kSide + c2;
        const int hops = std::abs(r1 - r2) + std::abs(c1 - c2);

        const RouteResult route = dijkstra(graph, source, target);
        REQUIRE(route.found());
        REQUIRE(static_cast<int>(route.path.size()) == hops + 1);
        // Vertical and horizontal hops differ by a hair (cos-latitude), so
        // compare within a percent rather than exactly.
        REQUIRE(route.travelSeconds == Approx(hops * hopSeconds).epsilon(0.01));
    }
}

TEST_CASE("backward search answers 'time TO here' for every node at once", "[router]") {
    const RoadGraph graph = buildGridGraph(10, 10, 100.0);

    std::mt19937 rng(97);
    std::uniform_int_distribution<int> pick(0, graph.numNodes() - 1);
    const int destination = pick(rng);

    const std::vector<double> toDestination =
        shortestPathTree(graph, destination, SearchDirection::Backward);

    // Every entry must equal a forward search run from that node -- this is
    // the identity the dense road cost matrix is built on, so if it is wrong
    // every ETA in the matcher is wrong with it.
    for (int trial = 0; trial < 40; ++trial) {
        const int from = pick(rng);
        const RouteResult forward = dijkstra(graph, from, destination);
        if (forward.found()) {
            REQUIRE(toDestination[from] == Approx(forward.travelSeconds).epsilon(1e-9));
        } else {
            REQUIRE(toDestination[from] == kUnreachable);
        }
    }
}

TEST_CASE("backward and forward differ on a one-way network", "[router][edge]") {
    // On an undirected graph the two directions coincide, which would let a
    // direction bug pass unnoticed. This graph is a one-way cycle: going with
    // the flow is one hop, going against it is three.
    RoadGraphBuilder b;
    std::vector<int> ring;
    for (int i = 0; i < 4; ++i) ring.push_back(b.addNode(i + 1, 12.97 + i * 0.001, 77.59));
    for (int i = 0; i < 4; ++i) b.addArc(ring[i], ring[(i + 1) % 4], 100.0, 10.0);
    const RoadGraph graph(b);

    REQUIRE(dijkstra(graph, 0, 1).travelSeconds == Approx(10.0));
    REQUIRE(dijkstra(graph, 1, 0).travelSeconds == Approx(30.0));

    const std::vector<double> forward = shortestPathTree(graph, 0, SearchDirection::Forward);
    const std::vector<double> backward = shortestPathTree(graph, 0, SearchDirection::Backward);
    REQUIRE(forward[1] == Approx(10.0));    // 0 -> 1
    REQUIRE(backward[1] == Approx(30.0));   // 1 -> 0
}

TEST_CASE("the hot-origin cache matches a fresh search", "[router]") {
    const RoadGraph graph = buildGridGraph(12, 12, 100.0);

    const std::vector<int> origins = {0, 17, 55, graph.numNodes() - 1};
    const SourceDistanceCache cache(graph, origins, SearchDirection::Backward);

    REQUIRE(cache.numOrigins() == static_cast<int>(origins.size()));
    for (int slot = 0; slot < cache.numOrigins(); ++slot) {
        REQUIRE(cache.at(slot, origins[slot]) == Approx(0.0));
        for (int v = 0; v < graph.numNodes(); v += 7) {
            const RouteResult fresh = dijkstra(graph, v, origins[slot]);
            if (fresh.found()) {
                REQUIRE(cache.at(slot, v) == Approx(fresh.travelSeconds).epsilon(1e-9));
            } else {
                REQUIRE(cache.at(slot, v) == kUnreachable);
            }
        }
    }
}

TEST_CASE("an empty origin list builds an empty cache", "[router][edge]") {
    const RoadGraph graph = buildGridGraph(4, 4, 100.0);
    const SourceDistanceCache cache(graph, {});
    REQUIRE(cache.numOrigins() == 0);
}

TEST_CASE("nearestNode snaps to the closest junction in metres", "[router][geo]") {
    const RoadGraph graph = buildGridGraph(10, 10, 200.0, 12.9700, 77.5900);

    // A query sitting exactly on a node returns that node.
    for (int v = 0; v < graph.numNodes(); v += 11) {
        const GeoNode& n = graph.node(v);
        const int snapped = graph.nearestNode(n.lat, n.lon);
        REQUIRE(haversineMeters(n.lat, n.lon,
                                graph.node(snapped).lat, graph.node(snapped).lon) ==
                Approx(0.0).margin(1e-6));
    }

    // A query in the middle of a block snaps to one of the surrounding nodes,
    // and specifically to the nearest one measured in metres -- checked
    // against a linear scan, because the quadtree works in a scaled projection.
    std::mt19937 rng(5150);
    double minLat, minLon, maxLat, maxLon;
    graph.bounds(minLat, minLon, maxLat, maxLon);
    std::uniform_real_distribution<double> latDist(minLat, maxLat);
    std::uniform_real_distribution<double> lonDist(minLon, maxLon);

    for (int trial = 0; trial < 200; ++trial) {
        const double lat = latDist(rng), lon = lonDist(rng);
        const int snapped = graph.nearestNode(lat, lon);

        double best = std::numeric_limits<double>::max();
        for (int v = 0; v < graph.numNodes(); ++v) {
            best = std::min(best, haversineMeters(lat, lon, graph.node(v).lat, graph.node(v).lon));
        }
        REQUIRE(haversineMeters(lat, lon, graph.node(snapped).lat, graph.node(snapped).lon) ==
                Approx(best).epsilon(1e-6));
    }
}
