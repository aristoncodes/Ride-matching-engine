#include "cost_matrix.h"

#include <algorithm>
#include <cmath>
#include <limits>

#include "router.h"

namespace {

// Scale a real euclidean distance to the integer cost the solver expects.
long long scaledDistance(const Point& a, const Point& b) {
    double dx = a.x - b.x;
    double dy = a.y - b.y;
    double dist = std::sqrt(dx * dx + dy * dy);
    return static_cast<long long>(std::llround(dist * COST_SCALE));
}

} // namespace

std::vector<MatchEdge> buildDenseEdges(const std::vector<Point>& riders,
                                       const std::vector<Point>& drivers) {
    std::vector<MatchEdge> edges;
    edges.reserve(riders.size() * drivers.size());
    for (int i = 0; i < static_cast<int>(riders.size()); ++i) {
        for (int j = 0; j < static_cast<int>(drivers.size()); ++j) {
            edges.push_back({i, j, scaledDistance(riders[i], drivers[j])});
        }
    }
    return edges;
}

std::vector<MatchEdge> buildSparseEdges(const std::vector<Point>& riders,
                                        const std::vector<Point>& drivers,
                                        int k, double gridSize) {
    std::vector<MatchEdge> edges;
    if (drivers.empty() || k <= 0) return edges;
    edges.reserve(riders.size() * static_cast<std::size_t>(k));

    // Index drivers in a quadtree, storing each driver's INDEX as the point id
    // so kNearest hands the index straight back for the MatchEdge.
    AABB worldBounds(gridSize / 2.0, gridSize / 2.0, gridSize / 2.0, gridSize / 2.0);
    QuadTree driverTree(worldBounds, 4);
    for (int j = 0; j < static_cast<int>(drivers.size()); ++j) {
        driverTree.insert(Point(j, drivers[j].x, drivers[j].y));
    }

    for (int i = 0; i < static_cast<int>(riders.size()); ++i) {
        std::vector<Point> nearest = driverTree.kNearest(riders[i].x, riders[i].y, k);
        for (const Point& d : nearest) {
            // d.id is the driver index; d carries the driver's coordinates.
            edges.push_back({i, d.id, scaledDistance(riders[i], d)});
        }
    }
    return edges;
}

// ---------------------------------------------------------------------------
// Road-time costs (Week 4)
// ---------------------------------------------------------------------------

namespace {

// Snap every GPS fix to its nearest graph node, once. Riders and drivers sit
// on rooftops and in car parks, never on a junction, so this is the bridge
// between "where the phone says they are" and "where the router can start".
std::vector<int> snapAll(const std::vector<GeoPoint>& points, const RoadGraph& graph) {
    std::vector<int> nodes;
    nodes.reserve(points.size());
    for (const GeoPoint& p : points) nodes.push_back(graph.nearestNode(p.lat, p.lon));
    return nodes;
}

// Seconds -> the solver's integer units, saturating rather than wrapping.
// COST_SCALE=1000 makes one unit a millisecond, so a full day of driving is
// ~8.6e7 units and long long has room to spare.
long long scaledSeconds(double seconds) {
    return static_cast<long long>(std::llround(seconds * COST_SCALE));
}

} // namespace

std::vector<MatchEdge> buildRoadEdgesDense(const std::vector<GeoPoint>& riders,
                                           const std::vector<GeoPoint>& drivers,
                                           const RoadGraph& graph) {
    std::vector<MatchEdge> edges;
    if (riders.empty() || drivers.empty()) return edges;
    edges.reserve(riders.size() * drivers.size());

    const std::vector<int> riderNodes = snapAll(riders, graph);
    const std::vector<int> driverNodes = snapAll(drivers, graph);

    for (int i = 0; i < static_cast<int>(riders.size()); ++i) {
        // One backward sweep prices this rider against EVERY driver at once.
        const std::vector<double> timeToRider =
            shortestPathTree(graph, riderNodes[i], SearchDirection::Backward);

        for (int j = 0; j < static_cast<int>(drivers.size()); ++j) {
            const double seconds = timeToRider[driverNodes[j]];
            if (seconds == kUnreachable) continue;   // no edge => never matched here
            edges.push_back({i, j, scaledSeconds(seconds)});
        }
    }
    return edges;
}

std::vector<MatchEdge> buildRoadEdgesSparse(const std::vector<GeoPoint>& riders,
                                            const std::vector<GeoPoint>& drivers,
                                            const RoadGraph& graph, int k) {
    std::vector<MatchEdge> edges;
    if (riders.empty() || drivers.empty() || k <= 0) return edges;
    edges.reserve(riders.size() * static_cast<std::size_t>(k));

    const std::vector<int> riderNodes = snapAll(riders, graph);
    const std::vector<int> driverNodes = snapAll(drivers, graph);

    // Quadtree over the drivers, in the same cos(lat)-corrected projection the
    // graph's own node index uses, so "nearest" means nearest in metres rather
    // than nearest in degrees.
    double minLat = std::numeric_limits<double>::max();
    double maxLat = std::numeric_limits<double>::lowest();
    double minLon = minLat, maxLon = maxLat;
    for (const GeoPoint& d : drivers) {
        minLat = std::min(minLat, d.lat);  maxLat = std::max(maxLat, d.lat);
        minLon = std::min(minLon, d.lon);  maxLon = std::max(maxLon, d.lon);
    }
    for (const GeoPoint& r : riders) {     // riders too: a query outside the
        minLat = std::min(minLat, r.lat);  // root box would miss entirely
        maxLat = std::max(maxLat, r.lat);
        minLon = std::min(minLon, r.lon);
        maxLon = std::max(maxLon, r.lon);
    }
    // M_PI is not in the C++ standard; spelling it out keeps this portable.
    constexpr double kPi = 3.14159265358979323846;
    const double lonScale = std::cos((minLat + maxLat) / 2.0 * kPi / 180.0);
    const double xMin = minLon * lonScale, xMax = maxLon * lonScale;
    const double halfW = std::max((xMax - xMin) / 2.0, 1e-9) * 1.001;
    const double halfH = std::max((maxLat - minLat) / 2.0, 1e-9) * 1.001;

    QuadTree driverTree(AABB((xMin + xMax) / 2.0, (minLat + maxLat) / 2.0, halfW, halfH), 4);
    for (int j = 0; j < static_cast<int>(drivers.size()); ++j) {
        driverTree.insert(Point(j, drivers[j].lon * lonScale, drivers[j].lat));
    }

    for (int i = 0; i < static_cast<int>(riders.size()); ++i) {
        const std::vector<Point> candidates =
            driverTree.kNearest(riders[i].lon * lonScale, riders[i].lat, k);
        if (candidates.empty()) continue;

        // Still one sweep per rider, not one search per candidate. A backward
        // sweep settles the whole graph (~27k nodes on the Bengaluru extract);
        // a single point-to-point A* still settles thousands, so even k=1
        // would not pay for itself. What sparsity buys here is a smaller EDGE
        // list for the MCMF solver -- which is where the O(N*M) hurt -- not
        // fewer shortest-path searches.
        const std::vector<double> timeToRider =
            shortestPathTree(graph, riderNodes[i], SearchDirection::Backward);

        for (const Point& candidate : candidates) {
            const double seconds = timeToRider[driverNodes[candidate.id]];
            if (seconds == kUnreachable) continue;
            edges.push_back({i, candidate.id, scaledSeconds(seconds)});
        }
    }
    return edges;
}
