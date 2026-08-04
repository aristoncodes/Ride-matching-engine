#include "mcmf.h"

#include <queue>
#include <limits>
#include <algorithm>

MinCostMaxFlow::MinCostMaxFlow(int numNodes)
    : graph_(numNodes), n_(numNodes) {}

int MinCostMaxFlow::addEdge(int from, int to, int capacity, long long cost) {
    int forwardId = static_cast<int>(edges_.size());

    // Forward edge: full capacity, real cost.
    edges_.push_back({to, capacity, cost});
    graph_[from].push_back(forwardId);

    // Reverse (residual) edge: starts at 0 capacity, negated cost. Cancelling
    // flow along it "refunds" the cost, which is what lets SSP correct an
    // earlier greedy choice — the reason this finds the true optimum.
    edges_.push_back({from, 0, -cost});
    graph_[to].push_back(forwardId + 1);

    return forwardId;
}

long long MinCostMaxFlow::getFlow(int forwardEdgeId) const {
    // Reverse edge's residual capacity == flow pushed on the forward edge.
    return edges_[forwardEdgeId ^ 1].cap;
}

// Queue-based Bellman-Ford (SPFA) from `source` over edges with residual
// capacity. Handles the residual graph's negative edges directly, which is why
// it can be used as an engine in its own right as well as to seed potentials.
// There are no negative CYCLES — min-cost flow never creates one — so it always
// terminates.
std::vector<long long> MinCostMaxFlow::bellmanFord(int source,
                                                   std::vector<int>* prevEdge) const {
    const long long INF = std::numeric_limits<long long>::max();
    std::vector<long long> dist(n_, INF);
    std::vector<char> inQueue(n_, 0);
    if (prevEdge) prevEdge->assign(n_, -1);

    dist[source] = 0;
    std::queue<int> q;
    q.push(source);
    inQueue[source] = 1;

    while (!q.empty()) {
        const int u = q.front();
        q.pop();
        inQueue[u] = 0;
        for (int id : graph_[u]) {
            const Edge& e = edges_[id];
            if (e.cap > 0 && dist[u] != INF && dist[u] + e.cost < dist[e.to]) {
                dist[e.to] = dist[u] + e.cost;
                if (prevEdge) (*prevEdge)[e.to] = id;
                if (!inQueue[e.to]) {
                    inQueue[e.to] = 1;
                    q.push(e.to);
                }
            }
        }
    }
    return dist;
}

std::pair<long long, long long> MinCostMaxFlow::solve(int source, int sink,
                                                      ShortestPathEngine engine) {
    const long long INF = std::numeric_limits<long long>::max();
    long long totalFlow = 0;
    long long totalCost = 0;

    // Pick an engine by edge density. The threshold is calibrated by
    // measurement, not taste: sweeping k from 4 to dense at N=200/600/1000,
    // SPFA won every case below ~33 edges per node and Dijkstra won every case
    // above ~49, at all three sizes (docs/Benchmarks.md). 40 is the midpoint of
    // that band, where the two are within a few percent and the choice barely
    // matters.
    //
    // The intuition: SPFA re-relaxes an edge once per improvement, so its work
    // grows with how many ways there are to reach a node -- i.e. with density.
    // Dijkstra settles each node exactly once but pays a log-factor heap and an
    // O(V) reset per augmentation, overhead that dominates when E is small.
    //
    // edges_ counts forward and reverse edges in pairs, hence the halving.
    if (engine == ShortestPathEngine::Auto) {
        const double edgesPerNode =
            n_ > 0 ? static_cast<double>(edges_.size() / 2) / n_ : 0.0;
        engine = (edgesPerNode > 40.0) ? ShortestPathEngine::Dijkstra
                                       : ShortestPathEngine::Spfa;
    }

    if (engine == ShortestPathEngine::Spfa) {
        std::vector<int> prevEdge;
        while (true) {
            const std::vector<long long> dist = bellmanFord(source, &prevEdge);
            if (dist[sink] == INF) break;      // no residual path => max flow

            int bottleneck = std::numeric_limits<int>::max();
            for (int v = sink; v != source; ) {
                const int id = prevEdge[v];
                bottleneck = std::min(bottleneck, edges_[id].cap);
                v = edges_[id ^ 1].to;         // reverse edge points at the predecessor
            }
            for (int v = sink; v != source; ) {
                const int id = prevEdge[v];
                edges_[id].cap -= bottleneck;
                edges_[id ^ 1].cap += bottleneck;
                v = edges_[id ^ 1].to;
            }

            totalFlow += bottleneck;
            totalCost += static_cast<long long>(bottleneck) * dist[sink];
        }
        return {totalFlow, totalCost};
    }

    // ---- Johnson potentials -------------------------------------------------
    //
    // Successive Shortest Paths needs a shortest path per unit of flow, and the
    // residual graph has NEGATIVE edges (cancelling flow refunds its cost), so
    // Dijkstra cannot be pointed at it directly. Bellman-Ford can, but costs
    // O(V*E) per augmentation, and with one augmentation per matched rider that
    // is where the whole solve went.
    //
    // Johnson's trick: keep a potential pot[v] (the cheapest known cost to
    // reach v) and search on the REDUCED cost
    //
    //     cost'(u -> v) = cost(u -> v) + pot[u] - pot[v]
    //
    // which is provably >= 0 whenever pot is a valid shortest-path function,
    // because pot[v] <= pot[u] + cost(u,v) is exactly the triangle inequality
    // shortest paths satisfy. Non-negative weights are all Dijkstra needs.
    //
    // The reduced costs also telescope along any path: the pot terms cancel in
    // pairs, leaving realCost = reducedCost + pot[sink] - pot[source]. So the
    // path Dijkstra finds under the reduced costs is the same path that is
    // cheapest under the real ones -- this is a change of coordinates, not an
    // approximation, and the optimum is untouched.
    //
    // The seeding pass is Bellman-Ford because the INITIAL graph may legally
    // contain negative edges (this class is a general primitive; the ride
    // matcher happens to feed it only non-negative distances). One O(V*E) pass
    // up front, then Dijkstra for every augmentation after it.
    std::vector<long long> pot = bellmanFord(source, nullptr);
    for (long long& p : pot) {
        if (p == INF) p = 0;   // unreachable now, and it can only stay that way
    }

    // (priority, node), min-heap. Lazy deletion, as in the road router.
    using QueueEntry = std::pair<long long, int>;
    std::priority_queue<QueueEntry, std::vector<QueueEntry>, std::greater<>> heap;

    std::vector<long long> dist(n_);
    std::vector<int> prevEdge(n_);
    std::vector<char> settled(n_);

    while (true) {
        std::fill(dist.begin(), dist.end(), INF);
        std::fill(prevEdge.begin(), prevEdge.end(), -1);
        std::fill(settled.begin(), settled.end(), 0);

        dist[source] = 0;
        heap.push({0, source});

        while (!heap.empty()) {
            const auto [d, u] = heap.top();
            heap.pop();
            if (settled[u]) continue;
            settled[u] = 1;

            for (int id : graph_[u]) {
                const Edge& e = edges_[id];
                if (e.cap <= 0) continue;
                // Reduced cost; non-negative by the argument above.
                const long long weight = e.cost + pot[u] - pot[e.to];
                if (d + weight < dist[e.to]) {
                    dist[e.to] = d + weight;
                    prevEdge[e.to] = id;
                    heap.push({dist[e.to], e.to});
                }
            }
        }
        while (!heap.empty()) heap.pop();   // drop stale entries before reuse

        // No residual path to the sink => max flow reached.
        if (dist[sink] == INF) break;

        // Roll the potentials forward. pot[v] now holds the true cheapest cost
        // from source to v in the current residual graph, which is what keeps
        // the next round's reduced costs non-negative. Nodes that were not
        // reached keep their old potential: they contribute no usable edge this
        // round, so no reduced cost depends on them.
        for (int v = 0; v < n_; ++v) {
            if (dist[v] != INF) pot[v] += dist[v];
        }

        // Bottleneck along the found path (walk backwards via reverse edges).
        int bottleneck = std::numeric_limits<int>::max();
        for (int v = sink; v != source; ) {
            const int id = prevEdge[v];
            bottleneck = std::min(bottleneck, edges_[id].cap);
            v = edges_[id ^ 1].to; // reverse edge points back to the predecessor
        }

        // Augment: consume forward capacity, add it back on the reverse edge.
        for (int v = sink; v != source; ) {
            const int id = prevEdge[v];
            edges_[id].cap -= bottleneck;
            edges_[id ^ 1].cap += bottleneck;
            v = edges_[id ^ 1].to;
        }

        totalFlow += bottleneck;
        // pot[sink] is the REAL path cost (pot[source] stays 0 throughout, so
        // the telescoping correction is just pot[sink]).
        totalCost += static_cast<long long>(bottleneck) * pot[sink];
    }

    return {totalFlow, totalCost};
}
