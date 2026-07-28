#ifndef MCMF_H
#define MCMF_H

#include <vector>
#include <utility>

// Reusable Min-Cost Max-Flow solver (Successive Shortest Paths).
//
// This is a PURE graph primitive: it knows nothing about riders, drivers, or
// coordinates. You add nodes/edges, call solve(source, sink), and it pushes as
// much flow as possible while minimizing total cost. The ride-matching
// assignment problem is *modeled on top of* this in assignment.cpp — keeping
// the solver oblivious to the domain is exactly what makes it testable and
// reusable (Week 3's "separate concerns" requirement).
//
// Costs are integers (long long) on purpose: comparing shortest-path costs with
// floating-point distances is a classic source of silent nondeterminism. The
// caller scales real distances to integers before building the graph.
class MinCostMaxFlow {
public:
    explicit MinCostMaxFlow(int numNodes);

    // Add a directed edge from->to with the given capacity and per-unit cost.
    // Internally also inserts the paired residual (reverse) edge. Returns the
    // id of the *forward* edge so the caller can query getFlow() on it later.
    int addEdge(int from, int to, int capacity, long long cost);

    // Push min-cost max-flow from source to sink. Returns {maxFlow, minCost}.
    // After this returns, getFlow() is valid for any forward edge id.
    std::pair<long long, long long> solve(int source, int sink);

    // Flow that ended up on the forward edge with this id (0 if none).
    // Trick: the paired reverse edge's residual capacity equals the flow
    // pushed along the forward edge, because it starts at 0 and grows by
    // exactly the amount augmented.
    long long getFlow(int forwardEdgeId) const;

private:
    struct Edge {
        int to;
        int cap;        // residual capacity remaining on this edge
        long long cost; // per-unit cost (negative on residual/reverse edges)
    };

    // edges_ holds forward/reverse edges in adjacent pairs: edge id `e` and its
    // partner `e ^ 1`. graph_[u] lists edge ids leaving node u.
    std::vector<Edge> edges_;
    std::vector<std::vector<int>> graph_;
    int n_;
};

#endif // MCMF_H
