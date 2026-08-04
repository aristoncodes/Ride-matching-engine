// Week 4 showcase: routing on a real road network.
//
// Three things this has to prove, in order:
//   1. the graph really is the road network (nodes, one-ways, speeds, and a
//      strongly connected core we can route inside);
//   2. A* returns the SAME cost as Dijkstra on every pair -- the correctness
//      claim, because a fast router that quietly returns second-best routes is
//      worse than a slow correct one;
//   3. A* gets there by settling far fewer nodes -- the performance claim.
//
// Exits non-zero if (2) ever fails, so it doubles as a CTest anchor.
//
// Usage: route_demo [path/to/extract.osm] [numPairs]

#include <chrono>
#include <cmath>
#include <cstdlib>
#include <iomanip>
#include <iostream>
#include <random>
#include <string>
#include <vector>

#include "osm_loader.h"
#include "router.h"

namespace {

using Clock = std::chrono::steady_clock;

double elapsedMs(Clock::time_point start) {
    return std::chrono::duration<double, std::milli>(Clock::now() - start).count();
}

std::string minutesAndSeconds(double seconds) {
    const int total = static_cast<int>(std::llround(seconds));
    return std::to_string(total / 60) + "m " + std::to_string(total % 60) + "s";
}

} // namespace

int main(int argc, char** argv) {
    const std::string path = (argc > 1) ? argv[1] : "../data/bengaluru_roads.osm";
    const int numPairs = (argc > 2) ? std::atoi(argv[2]) : 200;

    std::cout << "=== Week 4: Dijkstra / A* on a real road graph ===\n\n";

    RoadGraph graph = [&] {
        try {
            std::cout << "Loading " << path << " ...\n";
            const auto start = Clock::now();
            RoadGraph g = loadOsm(path);
            std::cout << "  parsed in " << std::fixed << std::setprecision(0)
                      << elapsedMs(start) << " ms\n";
            return g;
        } catch (const std::exception& e) {
            // Falling back rather than dying: the demo should still run on a
            // machine that never fetched the extract.
            std::cout << "  " << e.what() << "\n"
                      << "  falling back to a synthetic 120x120 grid.\n";
            return buildGridGraph(120, 120, 100.0);
        }
    }();

    double minLat, minLon, maxLat, maxLon;
    graph.bounds(minLat, minLon, maxLat, maxLon);
    const std::vector<int>& core = graph.largestComponent();

    std::cout << "\nGraph\n"
              << "  nodes                  : " << graph.numNodes() << "\n"
              << "  directed arcs          : " << graph.numArcs() << "\n"
              << "  largest SCC            : " << core.size() << " nodes ("
              << std::fixed << std::setprecision(1)
              << (100.0 * core.size() / graph.numNodes()) << "% of the graph)\n"
              << "  fastest road           : " << std::setprecision(1)
              << graph.maxSpeedMps() * 3.6 << " km/h\n"
              << "  bounding box           : " << std::setprecision(4)
              << minLat << "," << minLon << " .. " << maxLat << "," << maxLon << "\n"
              << "  box diagonal           : " << std::setprecision(2)
              << haversineMeters(minLat, minLon, maxLat, maxLon) / 1000.0 << " km\n";

    if (core.size() < 2) {
        std::cerr << "\nFAIL: no strongly connected core to route inside.\n";
        return 1;
    }

    // Everything below draws from the strongly connected core, so every pair is
    // guaranteed routable in both directions. Sampling the raw node list would
    // instead measure how often the extract has dead-end stubs.
    std::mt19937 rng(20260730);
    std::uniform_int_distribution<std::size_t> pick(0, core.size() - 1);

    std::cout << "\nOne route in detail\n";
    {
        const int source = core[pick(rng)];
        const int target = core[pick(rng)];
        const RouteResult route = astar(graph, source, target);
        const GeoNode& a = graph.node(source);
        const GeoNode& b = graph.node(target);
        std::cout << std::fixed << std::setprecision(5)
                  << "  from                   : " << a.lat << ", " << a.lon
                  << "  (osm node " << a.osmId << ")\n"
                  << "  to                     : " << b.lat << ", " << b.lon
                  << "  (osm node " << b.osmId << ")\n";
        if (route.found()) {
            std::cout << std::setprecision(2)
                      << "  travel time            : " << minutesAndSeconds(route.travelSeconds)
                      << "\n"
                      << "  road distance          : " << route.lengthMeters / 1000.0 << " km\n"
                      << "  straight line          : "
                      << haversineMeters(a.lat, a.lon, b.lat, b.lon) / 1000.0 << " km\n"
                      << "  detour factor          : "
                      << route.lengthMeters / haversineMeters(a.lat, a.lon, b.lat, b.lon) << "x\n"
                      << "  junctions on the route : " << route.path.size() << "\n";
            std::cout << "  first 5 nodes          :";
            for (std::size_t i = 0; i < route.path.size() && i < 5; ++i) {
                std::cout << " " << graph.node(route.path[i]).osmId;
            }
            std::cout << (route.path.size() > 5 ? " ...\n" : "\n");
        } else {
            std::cout << "  unreachable (unexpected inside the SCC)\n";
        }
        // That detour factor is the entire argument for this week's work: it is
        // the multiplier by which straight-line distance was lying to the matcher.
    }

    std::cout << "\nA* vs Dijkstra over " << numPairs << " random pairs\n";

    long long dijkstraSettled = 0, astarSettled = 0;
    double dijkstraMs = 0.0, astarMs = 0.0;
    int mismatches = 0, unreachable = 0;
    double worstAbsoluteError = 0.0;
    double detourSum = 0.0;
    int detourSamples = 0;

    for (int i = 0; i < numPairs; ++i) {
        const int source = core[pick(rng)];
        const int target = core[pick(rng)];
        if (source == target) continue;

        auto start = Clock::now();
        const RouteResult reference = dijkstra(graph, source, target);
        dijkstraMs += elapsedMs(start);

        start = Clock::now();
        const RouteResult guided = astar(graph, source, target);
        astarMs += elapsedMs(start);

        if (!reference.found() || !guided.found()) {
            if (reference.found() != guided.found()) {
                std::cerr << "  MISMATCH: one algorithm found a route and the other did not ("
                          << source << " -> " << target << ")\n";
                ++mismatches;
            }
            ++unreachable;
            continue;
        }

        // Same optimum, so the same cost -- to within the last bits of a double
        // accumulated in a different order along the same arcs.
        const double error = std::abs(reference.travelSeconds - guided.travelSeconds);
        worstAbsoluteError = std::max(worstAbsoluteError, error);
        if (error > 1e-6) {
            std::cerr << std::setprecision(9)
                      << "  MISMATCH " << source << " -> " << target << ": dijkstra="
                      << reference.travelSeconds << "s astar=" << guided.travelSeconds << "s\n";
            ++mismatches;
        }

        dijkstraSettled += reference.nodesSettled;
        astarSettled += guided.nodesSettled;

        const GeoNode& a = graph.node(source);
        const GeoNode& b = graph.node(target);
        const double crow = haversineMeters(a.lat, a.lon, b.lat, b.lon);
        if (crow > 1.0) {
            detourSum += reference.lengthMeters / crow;
            ++detourSamples;
        }
    }

    std::cout << std::setprecision(2)
              << "  nodes settled  dijkstra: " << dijkstraSettled << "\n"
              << "  nodes settled  A*      : " << astarSettled << "\n"
              << "  search-space reduction : "
              << (astarSettled ? static_cast<double>(dijkstraSettled) / astarSettled : 0.0)
              << "x fewer nodes settled\n"
              << "  wall clock     dijkstra: " << dijkstraMs << " ms total, "
              << dijkstraMs / numPairs << " ms/query\n"
              << "  wall clock     A*      : " << astarMs << " ms total, "
              << astarMs / numPairs << " ms/query\n"
              << "  speedup                : " << (astarMs > 0 ? dijkstraMs / astarMs : 0.0) << "x\n"
              << "  unreachable pairs      : " << unreachable << "\n"
              << std::scientific
              << "  worst cost difference  : " << worstAbsoluteError << " s\n"
              << std::fixed;
    if (detourSamples) {
        std::cout << "  mean detour factor     : " << detourSum / detourSamples
                  << "x  <- how far off straight-line distance was\n";
    }
    // Wall-clock gains less than the node-count reduction, and that is expected:
    // A* does strictly more work per node (an extra heap key, more pushes as f
    // improves). The heuristic is also weak here -- admissibility forces
    // dividing by the network's FASTEST road (70 km/h) while most of the city
    // runs at 25-45, so every estimate is ~2x short and the search stays broad.
    // Sharpening it without losing admissibility is what ALT landmarks and
    // Contraction Hierarchies exist for.
    std::cout << "  (A* settles " << std::setprecision(2)
              << (astarSettled ? static_cast<double>(dijkstraSettled) / astarSettled : 0.0)
              << "x fewer nodes but does more work per node; see the note in "
                 "route_demo.cpp)\n";

    // Stretch: precompute one-to-all from hot origins. Backward, because the
    // question a hotspot actually asks is "how long until each driver gets
    // HERE", not "how long from here to each driver".
    std::cout << "\nHot-origin distance cache (stretch)\n";
    {
        std::vector<int> hotspots;
        for (int i = 0; i < 4; ++i) hotspots.push_back(core[pick(rng)]);

        const auto start = Clock::now();
        SourceDistanceCache cache(graph, hotspots, SearchDirection::Backward);
        const double buildMs = elapsedMs(start);

        int probes = 0, agreed = 0;
        double lookupMs = 0.0, freshMs = 0.0;
        for (int slot = 0; slot < cache.numOrigins(); ++slot) {
            for (int i = 0; i < 50; ++i) {
                const int from = core[pick(rng)];

                auto t0 = Clock::now();
                const double cached = cache.at(slot, from);
                lookupMs += elapsedMs(t0);

                t0 = Clock::now();
                const RouteResult fresh = dijkstra(graph, from, hotspots[slot]);
                freshMs += elapsedMs(t0);

                ++probes;
                const bool bothUnreachable = (cached == kUnreachable) && !fresh.found();
                if (bothUnreachable ||
                    (fresh.found() && std::abs(cached - fresh.travelSeconds) < 1e-6)) {
                    ++agreed;
                } else {
                    ++mismatches;
                    std::cerr << "  MISMATCH: cached=" << cached
                              << " fresh=" << fresh.travelSeconds << "\n";
                }
            }
        }
        std::cout << std::setprecision(3)
                  << "  origins cached         : " << cache.numOrigins()
                  << " (backward trees)\n"
                  << "  build cost             : " << buildMs << " ms\n"
                  << "  memory                 : "
                  << (static_cast<double>(cache.numOrigins()) * graph.numNodes() * sizeof(double))
                         / (1024.0 * 1024.0) << " MB\n"
                  << "  " << probes << " probes agree with a fresh Dijkstra: " << agreed
                  << "/" << probes << "\n"
                  << "  lookup total           : " << lookupMs << " ms\n"
                  << "  fresh-Dijkstra total   : " << freshMs << " ms\n";
    }

    std::cout << "\n";
    if (mismatches > 0) {
        std::cerr << "FAIL: " << mismatches << " cost mismatch(es). A* is not returning "
                     "the optimal route -- check the heuristic is admissible.\n";
        return 1;
    }
    std::cout << "PASS: A* cost == Dijkstra cost on every pair, and the cache agrees "
                 "with a fresh search.\n";
    return 0;
}
