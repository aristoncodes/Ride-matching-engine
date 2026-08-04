#ifndef MCMF_H
#define MCMF_H

#include <vector>
#include <utility>

// Which shortest-path engine Successive Shortest Paths uses per augmentation.
//
// There is no universally faster choice, which is the interesting part:
//
//   Spfa     -- queue-based Bellman-Ford. Handles the residual graph's negative
//               edges directly, no potentials needed. On a SPARSE graph the
//               frontier stays tiny and it is close to O(E) with very small
//               constants.
//   Dijkstra -- with Johnson potentials (see mcmf.cpp). O(E log V), but the log
//               factor and the per-augmentation O(V) reset are pure overhead
//               when E is small. It wins decisively once E/V is large, because
//               SPFA re-relaxes the same edges many times over.
//
// Measured on this engine's own workload (see docs/Benchmarks.md): at N=M=1000
// SPFA is ~2.6x faster on the k=8 sparse matrix, and Dijkstra is ~2.2x faster
// on the dense one. Auto picks by edge density.
enum class ShortestPathEngine { Auto, Dijkstra, Spfa };

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
    //
    // `engine` selects the shortest-path routine. Both engines return the same
    // optimum -- the choice is purely about speed, and the tests assert that
    // equality on thousands of random graphs. Leave it at Auto in production;
    // it exists as a parameter so the claim is testable and benchmarkable.
    std::pair<long long, long long> solve(int source, int sink,
                                          ShortestPathEngine engine = ShortestPathEngine::Auto);

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

    // One queue-based Bellman-Ford pass over the residual graph. Used both as
    // the SPFA engine's per-augmentation search and, for the Dijkstra engine,
    // as the single seeding pass that makes the initial potentials valid.
    // `prevEdge` may be null when only the distances are wanted.
    std::vector<long long> bellmanFord(int source, std::vector<int>* prevEdge) const;

    // edges_ holds forward/reverse edges in adjacent pairs: edge id `e` and its
    // partner `e ^ 1`. graph_[u] lists edge ids leaving node u.
    std::vector<Edge> edges_;
    std::vector<std::vector<int>> graph_;
    int n_;
};

#endif // MCMF_H
