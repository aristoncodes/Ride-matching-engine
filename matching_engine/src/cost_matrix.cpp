#include "cost_matrix.h"

#include <cmath>

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
