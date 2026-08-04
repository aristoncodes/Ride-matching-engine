#include "router.h"

#include <algorithm>
#include <cmath>
#include <limits>
#include <queue>
#include <stdexcept>
#include <utility>

namespace {

// (priority, node). Smallest priority first -- greater<> turns the standard
// max-heap into the min-heap a shortest-path search needs.
using QueueEntry = std::pair<double, int>;
using MinHeap = std::priority_queue<QueueEntry, std::vector<QueueEntry>, std::greater<>>;

void checkNode(const RoadGraph& graph, int v, const char* what) {
    if (v < 0 || v >= graph.numNodes()) {
        throw std::out_of_range(std::string("router: ") + what + " node out of range");
    }
}

// Walk parent pointers back from target and flip. Returns false if the chain
// does not reach the source, which only happens if the caller asks for a path
// to a node that was never settled.
bool reconstruct(const std::vector<int>& parent, const std::vector<double>& parentLength,
                 int source, int target, RouteResult& out) {
    std::vector<int> path;
    double meters = 0.0;
    for (int v = target; v != -1; v = parent[v]) {
        path.push_back(v);
        if (v == source) break;
        meters += parentLength[v];
    }
    if (path.empty() || path.back() != source) return false;
    std::reverse(path.begin(), path.end());
    out.path = std::move(path);
    out.lengthMeters = meters;
    return true;
}

} // namespace

RouteResult dijkstra(const RoadGraph& graph, int source, int target) {
    checkNode(graph, source, "source");
    checkNode(graph, target, "target");

    const int n = graph.numNodes();
    std::vector<double> dist(n, kUnreachable);
    std::vector<int> parent(n, -1);
    std::vector<double> parentLength(n, 0.0);
    std::vector<char> settled(n, 0);

    RouteResult result;
    MinHeap heap;
    dist[source] = 0.0;
    heap.push({0.0, source});

    while (!heap.empty()) {
        const auto [d, v] = heap.top();
        heap.pop();
        // Lazy deletion: std::priority_queue cannot decrease a key, so a node
        // is pushed again each time its cost improves and the stale copies are
        // skipped here. Cheaper in practice than maintaining a handle map.
        if (settled[v]) continue;
        settled[v] = 1;
        ++result.nodesSettled;

        // The moment `target` is popped, its cost is final: everything still
        // in the heap costs at least this much, and arcs are non-negative, so
        // nothing left can improve it. Continuing would only settle nodes we
        // will never look at.
        if (v == target) break;

        for (const RoadArc& arc : graph.outgoing(v)) {
            const double candidate = d + arc.travelSeconds;
            if (candidate < dist[arc.to]) {
                dist[arc.to] = candidate;
                parent[arc.to] = v;
                parentLength[arc.to] = arc.lengthMeters;
                heap.push({candidate, arc.to});
            }
        }
    }

    if (dist[target] == kUnreachable) return result;
    result.travelSeconds = dist[target];
    reconstruct(parent, parentLength, source, target, result);
    return result;
}

RouteResult astar(const RoadGraph& graph, int source, int target) {
    checkNode(graph, source, "source");
    checkNode(graph, target, "target");

    const int n = graph.numNodes();
    const double maxSpeed = graph.maxSpeedMps();
    const GeoNode& goal = graph.node(target);

    // h(v): the fastest this trip could conceivably finish from v. Straight
    // line (nothing shorter exists) at the network's top speed (nothing
    // faster exists) -- so it can never over-estimate. See router.h.
    //
    // Memoised, because h(v) is asked for on every improving relaxation but
    // only ever has one answer per node. Haversine is four trig calls and an
    // atan2; recomputing it per relaxation costs more than the extra search it
    // saves, which is how A* ends up settling half as many nodes and barely
    // running faster. NaN is the "not computed yet" marker -- it cannot
    // collide with a real distance, which is always finite and non-negative.
    std::vector<double> h(n, std::numeric_limits<double>::quiet_NaN());
    auto heuristic = [&](int v) -> double {
        if (maxSpeed <= 0.0) return 0.0;   // degenerates to plain Dijkstra
        if (!std::isnan(h[v])) return h[v];
        const GeoNode& node = graph.node(v);
        h[v] = haversineMeters(node.lat, node.lon, goal.lat, goal.lon) / maxSpeed;
        return h[v];
    };

    std::vector<double> g(n, kUnreachable);
    std::vector<int> parent(n, -1);
    std::vector<double> parentLength(n, 0.0);
    std::vector<char> settled(n, 0);

    RouteResult result;
    MinHeap heap;
    g[source] = 0.0;
    heap.push({heuristic(source), source});

    while (!heap.empty()) {
        const int v = heap.top().second;
        heap.pop();
        if (settled[v]) continue;
        settled[v] = 1;
        ++result.nodesSettled;

        if (v == target) break;

        const double gv = g[v];
        for (const RoadArc& arc : graph.outgoing(v)) {
            const double candidate = gv + arc.travelSeconds;
            if (candidate < g[arc.to]) {
                g[arc.to] = candidate;
                parent[arc.to] = v;
                parentLength[arc.to] = arc.lengthMeters;
                // Ordered by f = g + h. Note the stored cost stays g: the
                // heuristic only steers the search order, it is never part of
                // the answer.
                heap.push({candidate + heuristic(arc.to), arc.to});
            }
        }
    }

    if (g[target] == kUnreachable) return result;
    result.travelSeconds = g[target];
    reconstruct(parent, parentLength, source, target, result);
    return result;
}

std::vector<double> shortestPathTree(const RoadGraph& graph, int source,
                                     SearchDirection direction) {
    checkNode(graph, source, "source");

    const int n = graph.numNodes();
    std::vector<double> dist(n, kUnreachable);
    std::vector<char> settled(n, 0);

    MinHeap heap;
    dist[source] = 0.0;
    heap.push({0.0, source});

    while (!heap.empty()) {
        const auto [d, v] = heap.top();
        heap.pop();
        if (settled[v]) continue;
        settled[v] = 1;

        // Backward walks the reversed graph, so `arc.to` is the node the arc
        // comes FROM. Same relaxation, opposite question.
        const auto arcs = (direction == SearchDirection::Forward) ? graph.outgoing(v)
                                                                  : graph.incoming(v);
        for (const RoadArc& arc : arcs) {
            const double candidate = d + arc.travelSeconds;
            if (candidate < dist[arc.to]) {
                dist[arc.to] = candidate;
                heap.push({candidate, arc.to});
            }
        }
    }
    return dist;
}

SourceDistanceCache::SourceDistanceCache(const RoadGraph& graph,
                                         const std::vector<int>& origins,
                                         SearchDirection direction)
    : origins_(origins), numNodes_(static_cast<std::size_t>(graph.numNodes())) {
    table_.assign(origins_.size() * numNodes_, kUnreachable);
    for (std::size_t slot = 0; slot < origins_.size(); ++slot) {
        const std::vector<double> tree = shortestPathTree(graph, origins_[slot], direction);
        std::copy(tree.begin(), tree.end(), table_.begin() + slot * numNodes_);
    }
}
