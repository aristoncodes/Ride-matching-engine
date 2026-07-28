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

std::pair<long long, long long> MinCostMaxFlow::solve(int source, int sink) {
    const long long INF = std::numeric_limits<long long>::max();
    long long totalFlow = 0;
    long long totalCost = 0;

    while (true) {
        // Shortest path (by cost) from source in the residual graph. We use
        // SPFA (a queue-based Bellman-Ford) because residual/reverse edges have
        // negative costs, which plain Dijkstra can't handle directly. There are
        // no negative *cycles* — min-cost flow never creates one — so SPFA
        // terminates. (Dijkstra-with-potentials is the faster variant; noted as
        // a Week 5 optimization, not needed for correctness here.)
        std::vector<long long> dist(n_, INF);
        std::vector<int> prevEdge(n_, -1);
        std::vector<char> inQueue(n_, 0);

        dist[source] = 0;
        std::queue<int> q;
        q.push(source);
        inQueue[source] = 1;

        while (!q.empty()) {
            int u = q.front();
            q.pop();
            inQueue[u] = 0;
            for (int id : graph_[u]) {
                const Edge& e = edges_[id];
                if (e.cap > 0 && dist[u] != INF && dist[u] + e.cost < dist[e.to]) {
                    dist[e.to] = dist[u] + e.cost;
                    prevEdge[e.to] = id;
                    if (!inQueue[e.to]) {
                        inQueue[e.to] = 1;
                        q.push(e.to);
                    }
                }
            }
        }

        // No residual path to the sink => max flow reached.
        if (dist[sink] == INF) break;

        // Bottleneck along the found path (walk backwards via reverse edges).
        int bottleneck = std::numeric_limits<int>::max();
        for (int v = sink; v != source; ) {
            int id = prevEdge[v];
            bottleneck = std::min(bottleneck, edges_[id].cap);
            v = edges_[id ^ 1].to; // reverse edge points back to the predecessor
        }

        // Augment: consume forward capacity, add it back on the reverse edge.
        for (int v = sink; v != source; ) {
            int id = prevEdge[v];
            edges_[id].cap -= bottleneck;
            edges_[id ^ 1].cap += bottleneck;
            v = edges_[id ^ 1].to;
        }

        totalFlow += bottleneck;
        totalCost += static_cast<long long>(bottleneck) * dist[sink];
    }

    return {totalFlow, totalCost};
}
