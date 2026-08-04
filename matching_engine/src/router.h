#ifndef ROUTER_H
#define ROUTER_H

#include <limits>
#include <vector>

#include "road_graph.h"

// ============================================================================
// Shortest-path queries over the road graph.
//
// Two algorithms, deliberately kept side by side:
//
//   dijkstra() -- expands strictly by cheapest-known-cost. It has no idea where
//                 the destination is, so it grows a circle outward in every
//                 direction. Correct, and the reference the rest of the engine
//                 is measured against.
//
//   astar()    -- expands by (cost so far + estimated cost remaining). With an
//                 ADMISSIBLE estimate -- one that never over-estimates -- the
//                 answer is provably identical to Dijkstra's, but the search
//                 leans toward the destination instead of exploring behind
//                 itself. Same route, a fraction of the nodes settled.
//
// The estimate here is  haversine(v, target) / maxSpeedMps.  It is admissible
// because the great-circle distance is the shortest path that could possibly
// exist between two points (no road is shorter than a straight line), and
// dividing it by the fastest speed anywhere on the network gives the shortest
// time that distance could possibly take. Under-estimating is the whole game:
// an estimate that over-shoots can make A* settle a node before its true
// cheapest path is found, and the answer silently stops being optimal.
// ============================================================================

constexpr double kUnreachable = std::numeric_limits<double>::infinity();

struct RouteResult {
    // Travel time in seconds; kUnreachable if no route exists (one-ways and
    // extract boundaries make this a real outcome, not a theoretical one).
    double travelSeconds = kUnreachable;
    double lengthMeters = 0.0;
    // Node indices from source to target inclusive. Empty when unreachable.
    std::vector<int> path;
    // Nodes settled (popped with a final cost). The honest measure of work
    // done, and the number that shows what the heuristic actually buys --
    // unlike wall-clock, it does not move when the machine is busy.
    long long nodesSettled = 0;

    bool found() const { return travelSeconds != kUnreachable; }
};

// Cheapest-time route from `source` to `target`, by cost only.
RouteResult dijkstra(const RoadGraph& graph, int source, int target);

// Same query, guided by the admissible heuristic above. Must return the same
// travelSeconds as dijkstra() for every pair -- that equality is the test.
RouteResult astar(const RoadGraph& graph, int source, int target);

// Direction of a one-to-all search. On a directed graph these are different
// questions, and the matcher needs the second one:
//   Forward  -- "how long from `source` to everywhere?"  (a rider's trip)
//   Backward -- "how long from everywhere to `source`?"  (every driver's ETA
//               to one rider, from a single search instead of one per driver)
enum class SearchDirection { Forward, Backward };

// One-to-all travel times from (or to) `source`, indexed by node. Unreachable
// nodes hold kUnreachable.
std::vector<double> shortestPathTree(const RoadGraph& graph, int source,
                                     SearchDirection direction = SearchDirection::Forward);

// Cached one-to-all results for a set of hot origins -- depots, airport ranks,
// a stadium at closing time. Building the table costs one Dijkstra per origin;
// after that every "how far is X from this hotspot" is an array lookup.
//
// This is the shape Week 4's stretch goal asks for, and the same shape
// Contraction Hierarchies generalises: pay once up front, answer instantly
// afterwards. The tradeoff is memory -- numOrigins * numNodes doubles -- which
// is exactly why it is a cache for hot origins and not for every node.
class SourceDistanceCache {
public:
    SourceDistanceCache(const RoadGraph& graph, const std::vector<int>& origins,
                        SearchDirection direction = SearchDirection::Forward);

    // Travel seconds from origins[originSlot] to node v (or to it, if the
    // cache was built Backward). kUnreachable if there is no route.
    double at(int originSlot, int v) const {
        return table_[static_cast<std::size_t>(originSlot) * numNodes_ + v];
    }

    int numOrigins() const { return static_cast<int>(origins_.size()); }
    const std::vector<int>& origins() const { return origins_; }

private:
    std::vector<int> origins_;
    std::size_t numNodes_;
    std::vector<double> table_;
};

#endif // ROUTER_H
