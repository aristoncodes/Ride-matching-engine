#ifndef TEST_ORACLES_H
#define TEST_ORACLES_H

// Independent, deliberately slow reference implementations.
//
// The point of an oracle is that it shares no code with the thing it checks.
// A brute-force O(N) scan and a quadtree can both be wrong, but they are very
// unlikely to be wrong in the SAME way -- which is what makes their agreement
// evidence. Every function here is written for obviousness, never speed.

#include <algorithm>
#include <cmath>
#include <functional>
#include <limits>
#include <vector>

#include "assignment.h"
#include "quadtree.h"
#include "road_graph.h"

namespace oracle {

inline double euclidean(double x1, double y1, double x2, double y2) {
    const double dx = x1 - x2, dy = y1 - y2;
    return std::sqrt(dx * dx + dy * dy);
}

// Nearest point by linear scan. Returns index into `points`, or -1.
inline int nearestByScan(const std::vector<Point>& points, double x, double y,
                         int excludeId = -1) {
    int best = -1;
    double bestDist = std::numeric_limits<double>::max();
    for (int i = 0; i < static_cast<int>(points.size()); ++i) {
        if (points[i].id == excludeId) continue;
        const double d = euclidean(points[i].x, points[i].y, x, y);
        if (d < bestDist) { bestDist = d; best = i; }
    }
    return best;
}

// The k nearest points by full sort, nearest first.
inline std::vector<Point> kNearestByScan(std::vector<Point> points, double x, double y, int k) {
    std::stable_sort(points.begin(), points.end(), [&](const Point& a, const Point& b) {
        return euclidean(a.x, a.y, x, y) < euclidean(b.x, b.y, x, y);
    });
    // erase, not resize: Point has no default constructor, so resize() cannot
    // even be instantiated -- the same trap kNearest() hit in the quadtree.
    if (static_cast<int>(points.size()) > k) {
        points.erase(points.begin() + k, points.end());
    }
    return points;
}

// Every point inside an axis-aligned box, by linear scan.
inline std::vector<Point> inRangeByScan(const std::vector<Point>& points, const AABB& box) {
    std::vector<Point> found;
    for (const Point& p : points) {
        if (box.contains(p)) found.push_back(p);
    }
    return found;
}

// Minimum total cost of a MAXIMUM matching on a dense N x M cost matrix, by
// exhaustive search. Only usable up to about 8x8 -- which is the whole idea:
// small enough to be certainly right, large enough to catch real bugs.
inline long long minCostMaxMatching(const std::vector<std::vector<long long>>& cost) {
    const int n = static_cast<int>(cost.size());
    const int m = n == 0 ? 0 : static_cast<int>(cost[0].size());
    const int target = std::min(n, m);
    if (target == 0) return 0;

    std::vector<char> usedDriver(m, 0);
    long long best = std::numeric_limits<long long>::max();

    std::function<void(int, long long, int)> rec =
        [&](int rider, long long curCost, int matched) {
            if (matched + (n - rider) < target) return;   // cannot still reach max matching
            if (curCost >= best) return;
            if (rider == n) {
                if (matched == target) best = std::min(best, curCost);
                return;
            }
            rec(rider + 1, curCost, matched);             // leave this rider unmatched
            for (int j = 0; j < m; ++j) {
                if (usedDriver[j]) continue;
                usedDriver[j] = 1;
                rec(rider + 1, curCost + cost[rider][j], matched + 1);
                usedDriver[j] = 0;
            }
        };

    rec(0, 0, 0);
    return best;
}

inline std::vector<MatchEdge> denseEdges(const std::vector<std::vector<long long>>& cost) {
    std::vector<MatchEdge> edges;
    for (int i = 0; i < static_cast<int>(cost.size()); ++i) {
        for (int j = 0; j < static_cast<int>(cost[i].size()); ++j) {
            edges.push_back({i, j, cost[i][j]});
        }
    }
    return edges;
}

// Bellman-Ford: shortest travel time from `source` to every node. Quadratically
// slower than Dijkstra and completely independent of it -- no heap, no settled
// set, no early exit, so none of Dijkstra's failure modes can be shared.
inline std::vector<double> shortestPathsByBellmanFord(const RoadGraph& graph, int source) {
    const int n = graph.numNodes();
    std::vector<double> dist(n, std::numeric_limits<double>::infinity());
    dist[source] = 0.0;
    for (int round = 0; round < n - 1; ++round) {
        bool changed = false;
        for (int v = 0; v < n; ++v) {
            if (dist[v] == std::numeric_limits<double>::infinity()) continue;
            for (const RoadArc& arc : graph.outgoing(v)) {
                const double candidate = dist[v] + arc.travelSeconds;
                if (candidate < dist[arc.to] - 1e-12) {
                    dist[arc.to] = candidate;
                    changed = true;
                }
            }
        }
        if (!changed) break;
    }
    return dist;
}

} // namespace oracle

#endif // TEST_ORACLES_H
