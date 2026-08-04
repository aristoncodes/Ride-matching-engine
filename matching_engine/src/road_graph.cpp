#include "road_graph.h"

#include <algorithm>
#include <cmath>
#include <limits>
#include <numeric>
#include <stdexcept>

namespace {

constexpr double kEarthRadiusMeters = 6371008.8;  // IUGG mean radius
constexpr double kPi = 3.14159265358979323846;

double toRadians(double degrees) { return degrees * kPi / 180.0; }

} // namespace

double haversineMeters(double lat1, double lon1, double lat2, double lon2) {
    const double phi1 = toRadians(lat1);
    const double phi2 = toRadians(lat2);
    const double dPhi = phi2 - phi1;
    const double dLambda = toRadians(lon2 - lon1);

    const double sinDPhi = std::sin(dPhi / 2.0);
    const double sinDLambda = std::sin(dLambda / 2.0);
    const double a = sinDPhi * sinDPhi +
                     std::cos(phi1) * std::cos(phi2) * sinDLambda * sinDLambda;
    // atan2 form rather than asin(sqrt(a)): numerically stable for antipodal
    // points, where asin's argument creeps past 1.0 and produces a NaN.
    return 2.0 * kEarthRadiusMeters * std::atan2(std::sqrt(a), std::sqrt(1.0 - a));
}

// ---------------------------------------------------------------------------
// RoadGraphBuilder
// ---------------------------------------------------------------------------

int RoadGraphBuilder::addNode(long long osmId, double lat, double lon) {
    auto [it, inserted] = osmToIndex_.emplace(osmId, static_cast<int>(nodes_.size()));
    if (inserted) {
        nodes_.push_back(GeoNode{osmId, lat, lon});
    }
    return it->second;
}

void RoadGraphBuilder::addArc(int from, int to, double lengthMeters, double speedMps) {
    if (from == to) return;              // self-loops never help a shortest path
    if (speedMps <= 0.0) return;         // an untraversable arc is just absent
    arcs_.push_back(PendingArc{from, to, lengthMeters / speedMps, lengthMeters});
    maxSpeedMps_ = std::max(maxSpeedMps_, speedMps);
}

// ---------------------------------------------------------------------------
// RoadGraph
// ---------------------------------------------------------------------------

RoadGraph::RoadGraph(const RoadGraphBuilder& builder)
    : nodes_(builder.nodes_), maxSpeedMps_(builder.maxSpeedMps_) {
    if (nodes_.empty()) {
        throw std::invalid_argument("RoadGraph: cannot build a graph with no nodes");
    }
    buildCsr(builder);
    computeLargestComponent();

    // Spatial index for snapping. Two details that matter:
    //  - the point id is the DENSE node index, so a lookup hands back exactly
    //    what the router needs, with no second map;
    //  - longitude is scaled by cos(mean latitude). A degree of longitude is
    //    shorter than a degree of latitude everywhere but the equator, so
    //    without this the quadtree's euclidean metric is stretched and
    //    "nearest" can be wrong. Negligible in Bengaluru (~2.5%), badly wrong
    //    in Oslo -- and this engine is meant to be sold to fleets anywhere.
    double minLat, minLon, maxLat, maxLon;
    bounds(minLat, minLon, maxLat, maxLon);
    const double meanLat = (minLat + maxLat) / 2.0;
    const double lonScale = std::cos(toRadians(meanLat));

    const double xMin = minLon * lonScale, xMax = maxLon * lonScale;
    // A hair of padding so points exactly on the boundary are still inside.
    const double halfW = std::max((xMax - xMin) / 2.0, 1e-9) * 1.001;
    const double halfH = std::max((maxLat - minLat) / 2.0, 1e-9) * 1.001;
    nodeIndex_ = std::make_unique<QuadTree>(
        AABB((xMin + xMax) / 2.0, (minLat + maxLat) / 2.0, halfW, halfH), 8);
    for (int v = 0; v < numNodes(); ++v) {
        nodeIndex_->insert(Point(v, nodes_[v].lon * lonScale, nodes_[v].lat));
    }
    lonScale_ = lonScale;
}

void RoadGraph::buildCsr(const RoadGraphBuilder& builder) {
    const int n = numNodes();
    const std::size_t m = builder.arcs_.size();

    // Counting sort by source node: count, prefix-sum into offsets, then place.
    // Two linear passes and no per-node vector -- this is why the finished
    // graph is one allocation instead of numNodes() of them.
    outOffsets_.assign(n + 1, 0);
    inOffsets_.assign(n + 1, 0);
    for (const auto& a : builder.arcs_) {
        ++outOffsets_[a.from + 1];
        ++inOffsets_[a.to + 1];
    }
    std::partial_sum(outOffsets_.begin(), outOffsets_.end(), outOffsets_.begin());
    std::partial_sum(inOffsets_.begin(), inOffsets_.end(), inOffsets_.begin());

    arcs_.resize(m);
    reverseArcs_.resize(m);
    std::vector<int> outCursor(outOffsets_.begin(), outOffsets_.end() - 1);
    std::vector<int> inCursor(inOffsets_.begin(), inOffsets_.end() - 1);
    for (const auto& a : builder.arcs_) {
        arcs_[outCursor[a.from]++] = RoadArc{a.to, a.travelSeconds, a.lengthMeters};
        // In the reverse index an arc is stored under its TARGET, and points
        // back at its source -- so incoming(v) reads exactly like outgoing(v).
        reverseArcs_[inCursor[a.to]++] = RoadArc{a.from, a.travelSeconds, a.lengthMeters};
    }
}

void RoadGraph::computeLargestComponent() {
    const int n = numNodes();

    // Kosaraju, iteratively. Pass 1: order nodes by DFS finish time on the
    // forward graph. Pass 2: DFS the reverse graph in reverse finish order --
    // each tree it grows is one strongly connected component.
    //
    // Iterative rather than recursive on purpose: a city extract is tens of
    // thousands of nodes deep along a highway chain, and a recursive DFS
    // blowing the stack is a crash, not an exception.
    std::vector<char> visited(n, 0);
    std::vector<int> order;
    order.reserve(n);

    std::vector<std::pair<int, std::size_t>> stack;  // (node, next arc index)
    for (int start = 0; start < n; ++start) {
        if (visited[start]) continue;
        visited[start] = 1;
        stack.push_back({start, 0});
        while (!stack.empty()) {
            auto& [v, idx] = stack.back();
            const auto arcs = outgoing(v);
            if (idx < arcs.size()) {
                const int w = arcs[idx++].to;
                if (!visited[w]) {
                    visited[w] = 1;
                    stack.push_back({w, 0});
                }
            } else {
                order.push_back(v);
                stack.pop_back();
            }
        }
    }

    std::vector<int> component(n, -1);
    int componentCount = 0;
    std::vector<int> currentComponent;
    std::vector<int> best;
    for (auto it = order.rbegin(); it != order.rend(); ++it) {
        if (component[*it] != -1) continue;
        currentComponent.clear();
        std::vector<int> simpleStack{*it};
        component[*it] = componentCount;
        while (!simpleStack.empty()) {
            const int v = simpleStack.back();
            simpleStack.pop_back();
            currentComponent.push_back(v);
            for (const RoadArc& arc : incoming(v)) {
                if (component[arc.to] == -1) {
                    component[arc.to] = componentCount;
                    simpleStack.push_back(arc.to);
                }
            }
        }
        if (currentComponent.size() > best.size()) best = currentComponent;
        ++componentCount;
    }

    std::sort(best.begin(), best.end());
    largestComponent_ = std::move(best);
}

void RoadGraph::bounds(double& minLat, double& minLon,
                       double& maxLat, double& maxLon) const {
    minLat = minLon = std::numeric_limits<double>::max();
    maxLat = maxLon = std::numeric_limits<double>::lowest();
    for (const GeoNode& n : nodes_) {
        minLat = std::min(minLat, n.lat);
        maxLat = std::max(maxLat, n.lat);
        minLon = std::min(minLon, n.lon);
        maxLon = std::max(maxLon, n.lon);
    }
}

int RoadGraph::nearestNode(double lat, double lon) const {
    std::optional<Point> hit = nodeIndex_->nearestNeighbor(lon * lonScale_, lat);
    if (hit) return hit->id;

    // The quadtree only misses when the query falls outside its root box (a
    // GPS fix outside the mapped area). Fall back to a linear scan in true
    // metres rather than returning "no road exists".
    int best = 0;
    double bestDist = std::numeric_limits<double>::max();
    for (int v = 0; v < numNodes(); ++v) {
        const double d = haversineMeters(lat, lon, nodes_[v].lat, nodes_[v].lon);
        if (d < bestDist) { bestDist = d; best = v; }
    }
    return best;
}
