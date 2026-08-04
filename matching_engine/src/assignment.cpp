#include "assignment.h"
#include "mcmf.h"

#include <tuple>

Assignment solveAssignment(int numRiders, int numDrivers,
                           const std::vector<MatchEdge>& edges,
                           ShortestPathEngine engine) {
    Assignment result;
    result.riderToDriver.assign(numRiders, -1);
    result.totalCost = 0;
    result.matchedCount = 0;

    if (numRiders <= 0 || numDrivers <= 0) {
        return result; // nothing to match
    }

    // Node layout:
    //   0                      : source
    //   [1 .. numRiders]       : riders
    //   [riderBase+numRiders .. ] : drivers
    //   last                   : sink
    const int source     = 0;
    const int riderBase  = 1;
    const int driverBase = 1 + numRiders;
    const int sink       = 1 + numRiders + numDrivers;
    const int numNodes   = sink + 1;

    MinCostMaxFlow mcmf(numNodes);

    for (int i = 0; i < numRiders; ++i) {
        mcmf.addEdge(source, riderBase + i, 1, 0);
    }
    for (int j = 0; j < numDrivers; ++j) {
        mcmf.addEdge(driverBase + j, sink, 1, 0);
    }

    // Remember each rider->driver forward edge id so we can read back which
    // ones carried flow (= were chosen) after solving.
    std::vector<std::tuple<int, int, int>> chosen; // (forwardId, rider, driver)
    chosen.reserve(edges.size());
    for (const auto& e : edges) {
        if (e.rider < 0 || e.rider >= numRiders ||
            e.driver < 0 || e.driver >= numDrivers) {
            continue; // ignore out-of-range edges defensively
        }
        int id = mcmf.addEdge(riderBase + e.rider, driverBase + e.driver, 1, e.cost);
        chosen.emplace_back(id, e.rider, e.driver);
    }

    auto [flow, cost] = mcmf.solve(source, sink, engine);
    result.totalCost = cost;
    result.matchedCount = static_cast<int>(flow);

    for (const auto& [id, rider, driver] : chosen) {
        if (mcmf.getFlow(id) > 0) {
            result.riderToDriver[rider] = driver;
        }
    }

    return result;
}
