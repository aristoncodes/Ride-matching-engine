#ifndef GRAPH_REGISTRY_H
#define GRAPH_REGISTRY_H

#include <memory>
#include <string>
#include <unordered_map>
#include <vector>

#include "road_graph.h"

// Road graphs loaded once at startup and shared read-only by every request.
//
// Loading is a STARTUP concern, never a request-time one: parsing the
// Bengaluru extract takes ~130 ms, which would blow a batch deadline outright,
// and doing it per request would re-parse the same file thousands of times a
// minute.
//
// Thread safety: the registry is populated before the gRPC server starts
// serving and is never mutated afterwards. That makes concurrent `find()` from
// many handler threads safe with no lock at all -- immutability is a cheaper
// and more reliable guarantee than a mutex, and it is available here precisely
// because the engine is stateless per call. If graphs ever need hot-reloading,
// this is the one place that assumption has to be revisited (swap in a
// shared_ptr<const Registry> under an atomic, rather than mutating in place).
class GraphRegistry {
public:
    // Load `path` and register it under `id`. Throws on a bad file; a server
    // that cannot load its graph should fail loudly at startup rather than
    // accept traffic it will reject one request at a time.
    void load(const std::string& id, const std::string& path);

    // Register an already-built graph (used by tests, which build small grids
    // rather than reading files).
    void add(const std::string& id, std::unique_ptr<RoadGraph> graph);

    // Null if no graph is registered under this id.
    const RoadGraph* find(const std::string& id) const;

    // Registered ids, sorted. Reported by the Health RPC so a readiness probe
    // can tell "process up" from "graph actually loaded and servable".
    std::vector<std::string> ids() const;

    bool empty() const { return graphs_.empty(); }

private:
    std::unordered_map<std::string, std::unique_ptr<RoadGraph>> graphs_;
};

#endif // GRAPH_REGISTRY_H
