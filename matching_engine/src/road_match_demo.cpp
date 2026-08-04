// Week 4 integration: what does routing actually buy the MATCHER?
//
// Week 3 proved the assignment is optimal. Optimal *with respect to the cost
// matrix it was handed* -- and that matrix was straight-line distance. This
// demo asks the only question that matters to a rider: does pricing the matrix
// in real drive time change who gets sent to whom, and by how much?
//
// The method is the important part. Both assignments are scored under the SAME
// yardstick -- true road travel time -- because that is the thing a rider
// experiences. Scoring the euclidean assignment by euclidean distance would
// just prove that euclidean is good at being euclidean.
//
// Usage: road_match_demo [path/to/extract.osm] [numRiders] [numDrivers]

#include <chrono>
#include <cmath>
#include <cstdlib>
#include <iomanip>
#include <iostream>
#include <random>
#include <string>
#include <vector>

#include "assignment.h"
#include "cost_matrix.h"
#include "osm_loader.h"
#include "road_graph.h"
#include "router.h"

namespace {

using Clock = std::chrono::steady_clock;

double elapsedMs(Clock::time_point start) {
    return std::chrono::duration<double, std::milli>(Clock::now() - start).count();
}

// Score an assignment under true road time. kUnreachable in, kUnreachable out --
// we never quietly drop a pairing that cannot actually be driven.
struct Score {
    double totalSeconds = 0.0;
    double worstSeconds = 0.0;
    int matched = 0;
    int unreachablePairings = 0;
};

Score scoreByRoadTime(const Assignment& assignment,
                      const std::vector<GeoPoint>& riders,
                      const std::vector<GeoPoint>& drivers,
                      const RoadGraph& graph) {
    Score score;
    for (int i = 0; i < static_cast<int>(riders.size()); ++i) {
        const int j = assignment.riderToDriver[i];
        if (j < 0) continue;
        const int riderNode = graph.nearestNode(riders[i].lat, riders[i].lon);
        const int driverNode = graph.nearestNode(drivers[j].lat, drivers[j].lon);
        const RouteResult route = astar(graph, driverNode, riderNode);
        if (!route.found()) { ++score.unreachablePairings; continue; }
        score.totalSeconds += route.travelSeconds;
        score.worstSeconds = std::max(score.worstSeconds, route.travelSeconds);
        ++score.matched;
    }
    return score;
}

void printScore(const char* label, const Score& score) {
    std::cout << std::fixed << std::setprecision(1)
              << "  " << std::left << std::setw(26) << label << std::right
              << "matched " << std::setw(4) << score.matched
              << " | total " << std::setw(8) << score.totalSeconds / 60.0 << " min"
              << " | mean " << std::setw(6)
              << (score.matched ? score.totalSeconds / score.matched / 60.0 : 0.0) << " min"
              << " | worst " << std::setw(6) << score.worstSeconds / 60.0 << " min";
    if (score.unreachablePairings) {
        std::cout << "  (" << score.unreachablePairings << " unreachable)";
    }
    std::cout << "\n";
}

} // namespace

int main(int argc, char** argv) {
    const std::string path = (argc > 1) ? argv[1] : "../data/bengaluru_roads.osm";
    const int numRiders = (argc > 2) ? std::atoi(argv[2]) : 60;
    const int numDrivers = (argc > 3) ? std::atoi(argv[3]) : 60;

    std::cout << "=== Week 4: matching on real travel time vs straight-line distance ===\n\n";

    RoadGraph graph = [&] {
        try {
            return loadOsm(path);
        } catch (const std::exception& e) {
            std::cout << e.what() << "\n  falling back to a synthetic 100x100 grid.\n";
            return buildGridGraph(100, 100, 100.0);
        }
    }();

    double minLat, minLon, maxLat, maxLon;
    graph.bounds(minLat, minLon, maxLat, maxLon);
    std::cout << "Graph: " << graph.numNodes() << " nodes, " << graph.numArcs()
              << " arcs, SCC " << graph.largestComponent().size() << "\n"
              << "Batch: " << numRiders << " riders, " << numDrivers << " drivers\n\n";

    // Riders and drivers are placed on random points of the strongly connected
    // core rather than at random lat/lons: a driver dropped in the middle of
    // Cubbon Park would snap to whatever road happens to be closest and skew
    // the comparison with an artefact of the sampling, not of the algorithms.
    std::mt19937 rng(20260730);
    const std::vector<int>& core = graph.largestComponent();
    std::uniform_int_distribution<std::size_t> pick(0, core.size() - 1);

    std::vector<GeoPoint> riders, drivers;
    for (int i = 0; i < numRiders; ++i) {
        const GeoNode& n = graph.node(core[pick(rng)]);
        riders.push_back(GeoPoint{i, n.lat, n.lon});
    }
    for (int j = 0; j < numDrivers; ++j) {
        const GeoNode& n = graph.node(core[pick(rng)]);
        drivers.push_back(GeoPoint{j, n.lat, n.lon});
    }

    // --- Matrix A: straight-line distance, i.e. what Week 3 shipped ----------
    // Degrees are converted to metres so the two matrices are at least
    // comparable in scale; the point stands either way, since a monotonic
    // rescaling cannot change which assignment is optimal.
    std::vector<Point> flatRiders, flatDrivers;
    const double lonScale = std::cos((minLat + maxLat) / 2.0 * 3.14159265358979323846 / 180.0);
    constexpr double kMetersPerDegree = 111320.0;
    for (int i = 0; i < numRiders; ++i) {
        flatRiders.push_back(Point(i, riders[i].lon * lonScale * kMetersPerDegree,
                                   riders[i].lat * kMetersPerDegree));
    }
    for (int j = 0; j < numDrivers; ++j) {
        flatDrivers.push_back(Point(j, drivers[j].lon * lonScale * kMetersPerDegree,
                                    drivers[j].lat * kMetersPerDegree));
    }

    auto start = Clock::now();
    const std::vector<MatchEdge> euclideanEdges = buildDenseEdges(flatRiders, flatDrivers);
    const double euclideanBuildMs = elapsedMs(start);
    start = Clock::now();
    const Assignment euclideanMatch = solveAssignment(numRiders, numDrivers, euclideanEdges);
    const double euclideanSolveMs = elapsedMs(start);

    // --- Matrix B: real road travel time ------------------------------------
    start = Clock::now();
    const std::vector<MatchEdge> roadEdges = buildRoadEdgesDense(riders, drivers, graph);
    const double roadBuildMs = elapsedMs(start);
    start = Clock::now();
    const Assignment roadMatch = solveAssignment(numRiders, numDrivers, roadEdges);
    const double roadSolveMs = elapsedMs(start);

    // --- Matrix C: road time, sparse candidates -----------------------------
    const int k = 8;
    start = Clock::now();
    const std::vector<MatchEdge> sparseEdges = buildRoadEdgesSparse(riders, drivers, graph, k);
    const double sparseBuildMs = elapsedMs(start);
    start = Clock::now();
    const Assignment sparseMatch = solveAssignment(numRiders, numDrivers, sparseEdges);
    const double sparseSolveMs = elapsedMs(start);

    std::cout << "Cost matrices\n" << std::fixed << std::setprecision(1)
              << "  euclidean (dense)      : " << euclideanEdges.size() << " edges, build "
              << euclideanBuildMs << " ms, solve " << euclideanSolveMs << " ms\n"
              << "  road time (dense)      : " << roadEdges.size() << " edges, build "
              << roadBuildMs << " ms, solve " << roadSolveMs << " ms\n"
              << "  road time (k=" << k << " sparse): " << sparseEdges.size() << " edges, build "
              << sparseBuildMs << " ms, solve " << sparseSolveMs << " ms\n\n";

    std::cout << "Scored under TRUE road travel time (lower is better)\n";
    const Score euclideanScore = scoreByRoadTime(euclideanMatch, riders, drivers, graph);
    const Score roadScore = scoreByRoadTime(roadMatch, riders, drivers, graph);
    const Score sparseScore = scoreByRoadTime(sparseMatch, riders, drivers, graph);
    printScore("matched on distance", euclideanScore);
    printScore("matched on road time", roadScore);
    printScore("matched on road time (k=8)", sparseScore);
    if (sparseScore.matched < roadScore.matched) {
        std::cout << "  note: the sparse row matched " << roadScore.matched - sparseScore.matched
                  << " fewer rider(s), so its lower TOTAL is not a win --\n"
                     "        capping candidates at k means some riders have no reachable "
                     "candidate left.\n"
                     "        Compare the mean column, and treat the drop in matched count "
                     "as the price of k.\n";
    }

    int changedPairings = 0;
    for (int i = 0; i < numRiders; ++i) {
        if (euclideanMatch.riderToDriver[i] != roadMatch.riderToDriver[i]) ++changedPairings;
    }

    std::cout << "\nVerdict\n";
    if (euclideanScore.matched && roadScore.matched) {
        const double saved = euclideanScore.totalSeconds - roadScore.totalSeconds;
        std::cout << std::setprecision(1)
                  << "  pairings changed       : " << changedPairings << " / " << numRiders
                  << " (" << 100.0 * changedPairings / numRiders << "%)\n"
                  << "  total waiting saved    : " << saved / 60.0 << " min ("
                  << (euclideanScore.totalSeconds > 0
                          ? 100.0 * saved / euclideanScore.totalSeconds : 0.0)
                  << "% of the distance-matched total)\n"
                  << "  worst-case wait        : " << euclideanScore.worstSeconds / 60.0
                  << " min -> " << roadScore.worstSeconds / 60.0 << " min\n";
        if (roadScore.worstSeconds > euclideanScore.worstSeconds) {
            std::cout <<
                "  ^ the worst single wait got WORSE while the total improved. That is not a\n"
                "    bug: the solver minimises the SUM, and a sum has no opinion about its\n"
                "    largest term -- it will happily make one rider wait 9 minutes to save\n"
                "    thirty seconds each for twenty others. If the product needs a ceiling on\n"
                "    any individual wait, that is a constraint to add to the model (drop edges\n"
                "    above a cutoff, or minimise a convex function of wait), not something\n"
                "    optimality gives away for free.\n";
        }
    }

    // Sanity check, not a decoration: the road-time assignment is the optimum
    // OF the road-time matrix, so nothing scored by road time may beat it.
    // If this ever fires, the cost matrix and the scorer disagree about what
    // they are measuring -- exactly the bug this comparison exists to catch.
    if (roadScore.matched >= euclideanScore.matched &&
        roadScore.totalSeconds > euclideanScore.totalSeconds + 1e-6) {
        std::cerr << "\nFAIL: the road-time optimum lost to the distance match on road time.\n";
        return 1;
    }
    std::cout << "\nPASS: road-time matching is optimal under the metric riders feel.\n";
    return 0;
}
